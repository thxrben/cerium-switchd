package rstp

import (
	"slices"
	"sort"
)

// Protocol constants (17.13, 17.14).
const (
	MigrateTime = 3
	TxHoldCount = 6
)

// Role is a port role.
type Role int

const (
	RoleDisabled Role = iota
	RoleRoot
	RoleDesignated
	RoleAlternate
	RoleBackup
)

func (r Role) String() string {
	return [...]string{"disabled", "root", "designated", "alternate", "backup"}[r]
}

type infoIs int

const (
	infoDisabled infoIs = iota
	infoMine
	infoAged
	infoReceived
)

type rcvdInfo int

const (
	otherInfo rcvdInfo = iota
	superiorDesignated
	repeatedDesignated
	inferiorDesignated
	inferiorRootAlternate
)

// Machine states (only those with lasting effect are kept; the others are
// transient actions).
type (
	pimState int
	prtState int
	tcmState int
	ppmState int
	prxState int
)

const (
	pimDisabled pimState = iota
	pimAged
	pimCurrent
)

const (
	prtDisabledPort prtState = iota
	prtDisablePort           // waiting for learning and forwarding to stop
	prtRoot
	prtDesignated
	prtBlockPort // waiting for learning and forwarding to stop
	prtAlternate
)

const (
	tcmInactive tcmState = iota
	tcmLearning
	tcmActive
)

const (
	ppmCheckingRSTP ppmState = iota
	ppmSelectingSTP
	ppmSensing
)

const (
	prxDiscard prxState = iota
	prxReceive
)

// BridgeConfig is the bridge's configuration.
type BridgeConfig struct {
	ID           BridgeID
	HelloTime    int
	MaxAge       int
	ForwardDelay int
}

// PortConfig is a port's configuration and link state.
type PortConfig struct {
	Priority  uint16 // 0-240
	Cost      uint32 // path cost; 0: from SpeedMbps
	AdminEdge bool
	AutoEdge  bool
	RootGuard bool // restrictedRole: never root port
	// P2P: point-to-point link (full duplex, or configured).
	P2P       bool
	SpeedMbps int
}

// PathCost is the 802.1D-2004 recommended path cost for a link speed.
func PathCost(mbps int) uint32 {
	if mbps <= 0 {
		return 20000000
	}
	c := 20000000 / mbps
	if c < 1 {
		c = 1
	}
	return uint32(c)
}

// PortVars is the complete state of one port (exported for snapshots).
type PortVars struct {
	Number  uint16
	Config  PortConfig
	Enabled bool

	ID                  PortID
	PIM                 pimState
	PRT                 prtState
	TCM                 tcmState
	PPM                 ppmState
	PRX                 prxState
	Info                infoIs
	PortPriority        Vector
	PortTimes           Times
	MsgPriority         Vector
	MsgTimes            Times
	DesignatedPriority  Vector
	DesignatedTimes     Times
	Role, SelectedRole  Role
	Selected            bool
	UpdtInfo            bool
	Reselect            bool
	Proposing           bool
	Proposed            bool
	Agree, Agreed       bool
	Synced, Sync        bool
	ReRoot              bool
	Learn, Learning     bool
	Forward, Forwarding bool
	Disputed            bool
	NewInfo             bool
	TcProp              bool
	TcAck               bool
	OperEdge            bool
	SendRSTP            bool
	RcvdRSTP, RcvdSTP   bool
	RcvdTc, RcvdTcn     bool
	RcvdTcAck           bool
	RcvdMsg             bool
	Mcheck              bool
	RootInconsistent    bool
	FdbFlush            bool
	TxCount             int
	// Timers (seconds).
	EdgeDelayWhile, FdWhile, HelloWhen, MdelayWhile, RbWhile, RcvdInfoWhile, RrWhile, TcWhile int
	// Counters.
	RxBPDUs, TxBPDUs uint64

	msg *BPDU // being processed
}

// Callbacks are the bridge's effects.
type Callbacks struct {
	// Send transmits a BPDU on a port.
	Send func(port uint16, b *BPDU)
	// State sets a port's learning and forwarding.
	State func(port uint16, learning, forwarding bool)
	// Flush removes the addresses learned on a port.
	Flush func(port uint16)
}

// Bridge is one RSTP bridge.
type Bridge struct {
	cfg   BridgeConfig
	cb    Callbacks
	ports map[uint16]*PortVars

	rootPriority Vector
	rootTimes    Times
	rootPort     PortID
	// TopologyChanges counts topology changes detected or learned.
	TopologyChanges uint64
	LastChangeTick  uint64
	ticks           uint64
}

// New creates a bridge.
func New(cfg BridgeConfig, cb Callbacks) *Bridge {
	if cb.Send == nil {
		cb.Send = func(uint16, *BPDU) {}
	}
	if cb.State == nil {
		cb.State = func(uint16, bool, bool) {}
	}
	if cb.Flush == nil {
		cb.Flush = func(uint16) {}
	}
	b := &Bridge{cfg: cfg, cb: cb, ports: map[uint16]*PortVars{}}
	b.rootPriority = b.bridgePriority()
	b.rootTimes = b.bridgeTimes()
	return b
}

func (b *Bridge) bridgePriority() Vector {
	return Vector{Root: b.cfg.ID, Bridge: b.cfg.ID}
}

func (b *Bridge) bridgeTimes() Times {
	return Times{MaxAge: b.cfg.MaxAge, HelloTime: b.cfg.HelloTime, ForwardDelay: b.cfg.ForwardDelay}
}

// SetConfig changes the bridge configuration (priority, timers).
func (b *Bridge) SetConfig(cfg BridgeConfig) {
	if cfg == b.cfg {
		return
	}
	b.cfg = cfg
	for _, p := range b.ports {
		p.Reselect, p.Selected = true, false
	}
	b.run()
}

// Config returns the bridge configuration.
func (b *Bridge) Config() BridgeConfig { return b.cfg }

// AddPort adds a port (disabled until SetEnabled), as after BEGIN.
func (b *Bridge) AddPort(number uint16, cfg PortConfig) {
	if _, ok := b.ports[number]; ok {
		b.SetPortConfig(number, cfg)
		return
	}
	p := &PortVars{Number: number, Config: cfg}
	p.ID = MakePortID(cfg.Priority, number)
	b.ports[number] = p
	b.begin(p)
	b.run()
}

// RemovePort removes a port.
func (b *Bridge) RemovePort(number uint16) {
	if _, ok := b.ports[number]; !ok {
		return
	}
	delete(b.ports, number)
	for _, p := range b.ports {
		p.Reselect, p.Selected = true, false
	}
	b.run()
}

// SetPortConfig changes a port's configuration.
func (b *Bridge) SetPortConfig(number uint16, cfg PortConfig) {
	p := b.ports[number]
	if p == nil || p.Config == cfg {
		return
	}
	p.Config = cfg
	p.ID = MakePortID(cfg.Priority, number)
	if !cfg.AdminEdge && !cfg.AutoEdge {
		p.OperEdge = false
	}
	if cfg.AdminEdge && !p.Enabled {
		p.OperEdge = true
	}
	for _, q := range b.ports {
		q.Reselect, q.Selected = true, false
	}
	b.run()
}

// SetEnabled sets a port's link state (portEnabled).
func (b *Bridge) SetEnabled(number uint16, up bool) {
	p := b.ports[number]
	if p == nil || p.Enabled == up {
		return
	}
	p.Enabled = up
	b.run()
}

// Receive processes a BPDU received on a port.
func (b *Bridge) Receive(number uint16, bpdu *BPDU) {
	p := b.ports[number]
	if p == nil || !p.Enabled {
		return
	}
	// Our own BPDU looped back to the port that sent it (9.3.4).
	if bpdu.Type != TypeTCN && bpdu.Priority.Bridge == b.cfg.ID && bpdu.Priority.Port.Number() == p.ID.Number() {
		return
	}
	p.RxBPDUs++
	// Port Receive machine (17.23).
	if p.RcvdMsg {
		return // (never: messages are processed at once)
	}
	switch bpdu.Type {
	case TypeRST:
		p.RcvdRSTP = true
	default:
		p.RcvdSTP = true
	}
	p.OperEdge = false
	p.RcvdMsg = true
	p.EdgeDelayWhile = MigrateTime
	p.PRX = prxReceive
	p.msg = bpdu
	b.run()
	p.msg = nil
}

// Tick advances all timers by one second.
func (b *Bridge) Tick() {
	b.ticks++
	for _, p := range b.ports {
		dec := func(v *int) {
			if *v > 0 {
				*v--
			}
		}
		dec(&p.EdgeDelayWhile)
		dec(&p.FdWhile)
		dec(&p.HelloWhen)
		dec(&p.MdelayWhile)
		dec(&p.RbWhile)
		dec(&p.RcvdInfoWhile)
		dec(&p.RrWhile)
		dec(&p.TcWhile)
		dec(&p.TxCount)
	}
	b.run()
}

func (b *Bridge) sorted() []*PortVars {
	out := make([]*PortVars, 0, len(b.ports))
	for _, p := range b.ports {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// begin initialises a port's machines (BEGIN).
func (b *Bridge) begin(p *PortVars) {
	// PIM DISABLED
	p.RcvdMsg = false
	p.Proposing, p.Proposed, p.Agree, p.Agreed = false, false, false, false
	p.RcvdInfoWhile = 0
	p.Info = infoDisabled
	p.Reselect, p.Selected = true, false
	p.PIM = pimDisabled
	// PRT INIT_PORT
	p.Role = RoleDisabled
	p.SelectedRole = RoleDisabled
	p.Learn, p.Forward = false, false
	p.Synced = false
	p.Sync, p.ReRoot = true, true
	p.RrWhile = b.fwdDelay(p)
	p.FdWhile = b.maxAge(p)
	p.RbWhile = 0
	p.PRT = prtDisablePort
	// PST DISCARDING
	if p.Learning || p.Forwarding {
		p.Learning, p.Forwarding = false, false
	}
	b.cb.State(p.Number, false, false)
	// TCM INACTIVE
	p.FdbFlush = true
	p.TcWhile = 0
	p.TcAck = false
	p.TCM = tcmInactive
	// PPM CHECKING_RSTP
	p.Mcheck = false
	p.SendRSTP = true
	p.MdelayWhile = MigrateTime
	p.PPM = ppmCheckingRSTP
	// BDM
	p.OperEdge = p.Config.AdminEdge
	// PRX DISCARD
	p.RcvdRSTP, p.RcvdSTP = false, false
	p.EdgeDelayWhile = MigrateTime
	p.PRX = prxDiscard
	// PTX TRANSMIT_INIT
	p.NewInfo = true
	p.TxCount = 0
	p.HelloWhen = 0
	b.cfgIDs(p)
}

func (b *Bridge) cfgIDs(p *PortVars) {
	p.DesignatedPriority = Vector{Root: b.rootPriority.Root, Cost: b.rootPriority.Cost, Bridge: b.cfg.ID, Port: p.ID, RxPort: p.ID}
	p.DesignatedTimes = b.rootTimes
	p.DesignatedTimes.HelloTime = b.cfg.HelloTime
}

// Derived timer values (17.20).
func (b *Bridge) fwdDelay(p *PortVars) int {
	if p.DesignatedTimes.ForwardDelay > 0 {
		return p.DesignatedTimes.ForwardDelay
	}
	return b.cfg.ForwardDelay
}

func (b *Bridge) maxAge(p *PortVars) int {
	if p.DesignatedTimes.MaxAge > 0 {
		return p.DesignatedTimes.MaxAge
	}
	return b.cfg.MaxAge
}

func (b *Bridge) helloTime(p *PortVars) int {
	if p.DesignatedTimes.HelloTime > 0 {
		return p.DesignatedTimes.HelloTime
	}
	return b.cfg.HelloTime
}

func (b *Bridge) forwardDelay(p *PortVars) int {
	if p.SendRSTP {
		return b.helloTime(p)
	}
	return b.fwdDelay(p)
}

func (b *Bridge) edgeDelay(p *PortVars) int {
	if p.Config.P2P {
		return MigrateTime
	}
	return b.maxAge(p)
}

func (b *Bridge) cost(p *PortVars) uint32 {
	if p.Config.Cost > 0 {
		return p.Config.Cost
	}
	return PathCost(p.Config.SpeedMbps)
}

// run executes the state machines until nothing changes.
func (b *Bridge) run() {
	for range 64 {
		changed := false
		for _, p := range b.sorted() {
			changed = b.prx(p) || changed
			changed = b.ppm(p) || changed
			changed = b.bdm(p) || changed
			changed = b.pim(p) || changed
		}
		changed = b.prs() || changed
		for _, p := range b.sorted() {
			changed = b.prt(p) || changed
			changed = b.pst(p) || changed
			changed = b.tcm(p) || changed
		}
		for _, p := range b.sorted() {
			changed = b.ptx(p) || changed
		}
		if !changed {
			return
		}
	}
}

// ---- Port Receive (17.23) ----

func (b *Bridge) prx(p *PortVars) bool {
	if p.PRX == prxReceive && !p.Enabled {
		p.PRX = prxDiscard
		p.RcvdMsg = false
		p.EdgeDelayWhile = MigrateTime
		return true
	}
	return false
}

// ---- Port Protocol Migration (17.24) ----

func (b *Bridge) ppm(p *PortVars) bool {
	switch p.PPM {
	case ppmCheckingRSTP:
		if p.MdelayWhile != MigrateTime && !p.Enabled {
			p.Mcheck, p.SendRSTP, p.MdelayWhile = false, true, MigrateTime
			return false
		}
		if p.MdelayWhile == 0 {
			p.PPM = ppmSensing
			p.RcvdRSTP, p.RcvdSTP = false, false
			return true
		}
	case ppmSelectingSTP:
		if p.MdelayWhile == 0 || !p.Enabled || p.Mcheck {
			p.PPM = ppmSensing
			p.RcvdRSTP, p.RcvdSTP = false, false
			return true
		}
	case ppmSensing:
		switch {
		case !p.Enabled || p.Mcheck || (!p.SendRSTP && p.RcvdRSTP):
			p.PPM = ppmCheckingRSTP
			p.Mcheck, p.SendRSTP, p.MdelayWhile = false, true, MigrateTime
			return true
		case p.SendRSTP && p.RcvdSTP:
			p.PPM = ppmSelectingSTP
			p.SendRSTP, p.MdelayWhile = false, MigrateTime
			return true
		}
	}
	return false
}

// ---- Bridge Detection (17.25) ----

func (b *Bridge) bdm(p *PortVars) bool {
	if p.OperEdge {
		if (!p.Enabled && !p.Config.AdminEdge) || !p.OperEdge {
			p.OperEdge = false
			return true
		}
		return false
	}
	if (!p.Enabled && p.Config.AdminEdge) ||
		(p.EdgeDelayWhile == 0 && p.Config.AutoEdge && p.SendRSTP && p.Proposing) {
		p.OperEdge = true
		return true
	}
	return false
}

// ---- Port Information (17.27) ----

func (b *Bridge) pim(p *PortVars) bool {
	if !p.Enabled && p.Info != infoDisabled {
		p.RcvdMsg = false
		p.Proposing, p.Proposed, p.Agree, p.Agreed = false, false, false, false
		p.RcvdInfoWhile = 0
		p.Info = infoDisabled
		p.Reselect, p.Selected = true, false
		p.PIM = pimDisabled
		return true
	}
	switch p.PIM {
	case pimDisabled:
		if p.RcvdMsg {
			p.RcvdMsg = false
			return true
		}
		if p.Enabled {
			p.PIM = pimAged
			p.Info = infoAged
			p.Reselect, p.Selected = true, false
			return true
		}
	case pimAged:
		if p.Selected && p.UpdtInfo {
			b.pimUpdate(p)
			return true
		}
	case pimCurrent:
		switch {
		case p.Selected && p.UpdtInfo:
			b.pimUpdate(p)
			return true
		case p.Info == infoReceived && p.RcvdInfoWhile == 0 && !p.UpdtInfo && !p.RcvdMsg:
			p.PIM = pimAged
			p.Info = infoAged
			p.Reselect, p.Selected = true, false
			return true
		case p.RcvdMsg && !p.UpdtInfo:
			b.pimReceive(p)
			return true
		}
	}
	return false
}

func (b *Bridge) pimUpdate(p *PortVars) {
	p.Proposing, p.Proposed = false, false
	p.Agreed = p.Agreed && b.betterOrSame(p, infoMine)
	p.Synced = p.Synced && p.Agreed
	p.PortPriority = p.DesignatedPriority
	p.PortTimes = p.DesignatedTimes
	p.UpdtInfo = false
	p.Info = infoMine
	p.NewInfo = true
	p.PIM = pimCurrent
}

func (b *Bridge) betterOrSame(p *PortVars, newInfo infoIs) bool {
	if newInfo == infoReceived && p.Info == infoReceived {
		return p.MsgPriority.cmp(p.PortPriority) <= 0
	}
	if newInfo == infoMine && p.Info == infoMine {
		return p.DesignatedPriority.cmp(p.PortPriority) <= 0
	}
	return false
}

func (b *Bridge) pimReceive(p *PortVars) {
	m := p.msg
	p.PIM = pimCurrent
	if m == nil {
		p.RcvdMsg = false
		return
	}
	p.MsgPriority = m.Priority
	p.MsgPriority.RxPort = p.ID
	p.MsgTimes = m.Times
	switch b.rcvInfo(p, m) {
	case superiorDesignated:
		p.Agreed, p.Proposing = false, false
		b.recordProposal(p, m)
		b.setTcFlags(p, m)
		p.Agree = p.Agree && b.betterOrSame(p, infoReceived)
		p.PortPriority = p.MsgPriority
		p.PortTimes = p.MsgTimes
		b.updtRcvdInfoWhile(p)
		p.Info = infoReceived
		p.Reselect, p.Selected = true, false
	case repeatedDesignated:
		b.recordProposal(p, m)
		b.setTcFlags(p, m)
		b.updtRcvdInfoWhile(p)
	case inferiorDesignated:
		// recordDispute (802.1Q 13.26.13).
		if m.Type == TypeRST && m.Flags&flagLearning != 0 {
			p.Disputed = true
			p.Agreed = false
		}
	case inferiorRootAlternate:
		b.recordAgreement(p, m)
		b.setTcFlags(p, m)
	}
	if m.Type == TypeTCN {
		b.setTcFlags(p, m)
	}
	p.RcvdMsg = false
}

func (b *Bridge) rcvInfo(p *PortVars, m *BPDU) rcvdInfo {
	if m.Type == TypeTCN {
		return otherInfo
	}
	role := m.role()
	if role == encDesig {
		switch {
		case superior(p.MsgPriority, p.PortPriority) || (p.MsgPriority.cmp(p.PortPriority) == 0 && p.MsgTimes != p.PortTimes):
			return superiorDesignated
		case p.MsgPriority.cmp(p.PortPriority) == 0 && p.MsgTimes == p.PortTimes:
			return repeatedDesignated
		default:
			return inferiorDesignated
		}
	}
	if (role == encRoot || role == encAltBackup) && p.MsgPriority.cmp(p.PortPriority) >= 0 {
		return inferiorRootAlternate
	}
	return otherInfo
}

func (b *Bridge) recordProposal(p *PortVars, m *BPDU) {
	if m.role() == encDesig && m.Flags&flagProposal != 0 {
		p.Proposed = true
	}
}

func (b *Bridge) recordAgreement(p *PortVars, m *BPDU) {
	if p.Config.P2P && m.Flags&flagAgree != 0 {
		p.Agreed, p.Proposing = true, false
	} else {
		p.Agreed = false
	}
}

func (b *Bridge) setTcFlags(p *PortVars, m *BPDU) {
	switch m.Type {
	case TypeTCN:
		p.RcvdTcn = true
	case TypeConfig:
		p.RcvdTc = p.RcvdTc || m.Flags&flagTC != 0
		p.RcvdTcAck = p.RcvdTcAck || m.Flags&flagTCAck != 0
	default:
		p.RcvdTc = p.RcvdTc || m.Flags&flagTC != 0
	}
}

func (b *Bridge) updtRcvdInfoWhile(p *PortVars) {
	if p.PortTimes.MessageAge+1 <= p.PortTimes.MaxAge {
		h := p.PortTimes.HelloTime
		if h < 1 {
			h = 1
		}
		p.RcvdInfoWhile = 3 * h
	} else {
		p.RcvdInfoWhile = 0
	}
}

// ---- Port Role Selection (17.28) ----

func (b *Bridge) prs() bool {
	any := false
	for _, p := range b.ports {
		any = any || p.Reselect
	}
	if !any {
		return false
	}
	for _, p := range b.ports {
		p.Reselect = false
	}
	b.updtRolesTree()
	// setSelectedTree
	for _, p := range b.ports {
		if p.Reselect {
			return true
		}
	}
	for _, p := range b.ports {
		p.Selected = true
	}
	return true
}

func (b *Bridge) updtRolesTree() {
	best := b.bridgePriority()
	var rootPort *PortVars
	own := b.cfg.ID.MAC()
	ports := b.sorted()
	// Root path priority vectors of ports with received information.
	guarded := map[*PortVars]Vector{}
	for _, p := range ports {
		if p.Info != infoReceived || p.PortPriority.Bridge.MAC() == own {
			continue
		}
		v := p.PortPriority
		v.Cost += b.cost(p)
		v.RxPort = p.ID
		if p.Config.RootGuard {
			guarded[p] = v
			continue
		}
		if v.cmp5(best) < 0 {
			best, rootPort = v, p
		}
	}
	b.rootPriority = best
	if rootPort == nil {
		b.rootPort = 0
		b.rootTimes = b.bridgeTimes()
	} else {
		b.rootPort = rootPort.ID
		b.rootTimes = rootPort.PortTimes
		b.rootTimes.MessageAge++
	}
	for _, p := range ports {
		p.DesignatedPriority = Vector{Root: best.Root, Cost: best.Cost, Bridge: b.cfg.ID, Port: p.ID, RxPort: p.ID}
		p.DesignatedTimes = b.rootTimes
		p.DesignatedTimes.HelloTime = b.cfg.HelloTime
		p.RootInconsistent = false
		switch p.Info {
		case infoDisabled:
			p.SelectedRole = RoleDisabled
		case infoAged:
			p.SelectedRole = RoleDesignated
			p.UpdtInfo = true
		case infoMine:
			p.SelectedRole = RoleDesignated
			if p.PortPriority.cmp(p.DesignatedPriority) != 0 || p.PortTimes != p.DesignatedTimes {
				p.UpdtInfo = true
			}
		case infoReceived:
			switch {
			case p == rootPort:
				p.SelectedRole = RoleRoot
				p.UpdtInfo = false
			case p.DesignatedPriority.cmp(p.PortPriority) < 0:
				p.SelectedRole = RoleDesignated
				p.UpdtInfo = true
			case p.PortPriority.Bridge.MAC() == own:
				p.SelectedRole = RoleBackup
				p.UpdtInfo = false
			default:
				p.SelectedRole = RoleAlternate
				p.UpdtInfo = false
				if v, ok := guarded[p]; ok && v.cmp5(best) < 0 {
					p.RootInconsistent = true // it would be the root port
				}
			}
		}
	}
}

// ---- Port Role Transitions (17.29) ----

func (b *Bridge) allSynced() bool {
	for _, p := range b.ports {
		if !p.Selected || p.Role != p.SelectedRole || p.UpdtInfo || !(p.Synced || p.Role == RoleRoot) {
			return false
		}
	}
	return true
}

func (b *Bridge) reRooted(self *PortVars) bool {
	for _, p := range b.ports {
		if p != self && p.RrWhile != 0 {
			return false
		}
	}
	return true
}

func (b *Bridge) setSyncTree() {
	for _, p := range b.ports {
		p.Sync = true
	}
}

func (b *Bridge) setReRootTree() {
	for _, p := range b.ports {
		p.ReRoot = true
	}
}

func (b *Bridge) prt(p *PortVars) bool {
	if p.Role != p.SelectedRole && p.Selected && !p.UpdtInfo {
		switch p.SelectedRole {
		case RoleDisabled:
			p.Role = RoleDisabled
			p.Learn, p.Forward = false, false
			p.PRT = prtDisablePort
		case RoleRoot:
			p.Role = RoleRoot
			p.RrWhile = b.fwdDelay(p)
			p.PRT = prtRoot
		case RoleDesignated:
			p.Role = RoleDesignated
			p.PRT = prtDesignated
		case RoleAlternate, RoleBackup:
			p.Role = p.SelectedRole
			p.Learn, p.Forward = false, false
			p.PRT = prtBlockPort
		}
		return true
	}
	ok := p.Selected && !p.UpdtInfo
	switch p.PRT {
	case prtDisablePort:
		if ok && !p.Learning && !p.Forwarding {
			p.PRT = prtDisabledPort
			b.disabledPort(p)
			return true
		}
	case prtDisabledPort:
		if ok && (p.FdWhile != b.maxAge(p) || p.Sync || p.ReRoot || !p.Synced) {
			b.disabledPort(p)
			return true
		}
	case prtBlockPort:
		if ok && !p.Learning && !p.Forwarding {
			p.PRT = prtAlternate
			b.alternatePort(p)
			return true
		}
	case prtAlternate:
		return b.prtAlternate(p, ok)
	case prtRoot:
		return b.prtRoot(p, ok)
	case prtDesignated:
		return b.prtDesignated(p, ok)
	}
	return false
}

func (b *Bridge) disabledPort(p *PortVars) {
	p.FdWhile = b.maxAge(p)
	p.Synced = true
	p.RrWhile = 0
	p.Sync, p.ReRoot = false, false
}

func (b *Bridge) alternatePort(p *PortVars) {
	p.FdWhile = b.forwardDelay(p)
	p.Synced = true
	p.RrWhile = 0
	p.Sync, p.ReRoot = false, false
}

func (b *Bridge) prtAlternate(p *PortVars, ok bool) bool {
	if !ok {
		return false
	}
	switch {
	case p.Proposed && !p.Agree:
		b.setSyncTree()
		p.Proposed = false
		return true
	case (b.allSynced() && !p.Agree) || (p.Proposed && p.Agree):
		p.Proposed = false
		p.Agree = true
		p.NewInfo = true
		return true
	case p.SelectedRole == RoleBackup && p.RbWhile != 2*b.helloTime(p):
		p.RbWhile = 2 * b.helloTime(p)
		return true
	case p.FdWhile != b.forwardDelay(p) || p.Sync || p.ReRoot || !p.Synced:
		b.alternatePort(p)
		return true
	}
	return false
}

func (b *Bridge) prtRoot(p *PortVars, ok bool) bool {
	if !ok {
		return false
	}
	switch {
	case p.Proposed && !p.Agree:
		b.setSyncTree()
		p.Proposed = false
		return true
	case (b.allSynced() && !p.Agree) || (p.Proposed && p.Agree):
		p.Proposed, p.Sync = false, false
		p.Agree = true
		p.NewInfo = true
		return true
	case !p.Forward && !p.ReRoot:
		b.setReRootTree()
		return true
	case (p.FdWhile == 0 || (b.reRooted(p) && p.RbWhile == 0)) && !p.Learn:
		p.FdWhile = b.forwardDelay(p)
		p.Learn = true
		return true
	case (p.FdWhile == 0 || (b.reRooted(p) && p.RbWhile == 0)) && p.Learn && !p.Forward:
		p.FdWhile = 0
		p.Forward = true
		return true
	case p.ReRoot && p.Forward:
		p.ReRoot = false
		return true
	case p.RrWhile != b.fwdDelay(p):
		p.RrWhile = b.fwdDelay(p)
		return true
	}
	return false
}

func (b *Bridge) prtDesignated(p *PortVars, ok bool) bool {
	if !ok {
		return false
	}
	switch {
	case !p.Forward && !p.Agreed && !p.Proposing && !p.OperEdge:
		p.Proposing = true
		p.EdgeDelayWhile = b.edgeDelay(p)
		p.NewInfo = true
		return true
	case (!p.Learning && !p.Forwarding && !p.Synced) || (p.Agreed && !p.Synced) || (p.OperEdge && !p.Synced) || (p.Sync && p.Synced):
		p.RrWhile = 0
		p.Synced = true
		p.Sync = false
		return true
	case p.RrWhile == 0 && p.ReRoot:
		p.ReRoot = false
		return true
	case ((p.Sync && !p.Synced) || (p.ReRoot && p.RrWhile != 0) || p.Disputed) && !p.OperEdge && (p.Learn || p.Forward):
		p.Learn, p.Forward, p.Disputed = false, false, false
		p.FdWhile = b.forwardDelay(p)
		return true
	case (p.FdWhile == 0 || p.Agreed || p.OperEdge) && (p.RrWhile == 0 || !p.ReRoot) && !p.Sync && !p.Learn:
		p.Learn = true
		p.FdWhile = b.forwardDelay(p)
		return true
	case (p.FdWhile == 0 || p.Agreed || p.OperEdge) && (p.RrWhile == 0 || !p.ReRoot) && !p.Sync && p.Learn && !p.Forward:
		p.Forward = true
		p.FdWhile = 0
		p.Agreed = p.SendRSTP
		return true
	}
	return false
}

// ---- Port State Transition (17.30) ----

func (b *Bridge) pst(p *PortVars) bool {
	switch {
	case p.Forwarding && !p.Forward:
		p.Learning, p.Forwarding = false, false // FORWARDING -> DISCARDING
	case p.Forwarding:
		return false
	case p.Learning && !p.Learn:
		p.Learning = false // LEARNING -> DISCARDING
	case p.Learning && p.Forward:
		p.Forwarding = true // LEARNING -> FORWARDING
	case !p.Learning && p.Learn:
		p.Learning = true // DISCARDING -> LEARNING
	default:
		return false
	}
	b.cb.State(p.Number, p.Learning, p.Forwarding)
	return true
}

// ---- Topology Change (17.31) ----

func (b *Bridge) newTcWhile(p *PortVars) {
	if p.TcWhile != 0 {
		return
	}
	if p.SendRSTP {
		p.TcWhile = b.helloTime(p) + 1
		p.NewInfo = true
	} else {
		p.TcWhile = b.rootTimes.MaxAge + b.rootTimes.ForwardDelay
	}
}

func (b *Bridge) setTcPropTree(self *PortVars) {
	for _, p := range b.ports {
		if p != self {
			p.TcProp = true
		}
	}
}

func (b *Bridge) flush(p *PortVars) {
	if p.FdbFlush {
		b.cb.Flush(p.Number)
		p.FdbFlush = false
	}
}

func (b *Bridge) tcm(p *PortVars) bool {
	switch p.TCM {
	case tcmInactive:
		b.flush(p)
		if p.Learn && !p.FdbFlush {
			p.TCM = tcmLearning
			p.RcvdTc, p.RcvdTcn, p.RcvdTcAck, p.TcProp = false, false, false, false
			return true
		}
	case tcmLearning:
		switch {
		case (p.Role == RoleRoot || p.Role == RoleDesignated) && p.Forward && !p.OperEdge:
			// DETECTED
			b.newTcWhile(p)
			b.setTcPropTree(p)
			p.NewInfo = true
			p.TCM = tcmActive
			b.TopologyChanges++
			b.LastChangeTick = b.ticks
			return true
		case p.RcvdTc || p.RcvdTcn || p.RcvdTcAck || p.TcProp:
			p.RcvdTc, p.RcvdTcn, p.RcvdTcAck, p.TcProp = false, false, false, false
			return true
		case p.Role != RoleRoot && p.Role != RoleDesignated && !(p.Learn || p.Learning):
			p.TCM = tcmInactive
			p.FdbFlush = true
			p.TcWhile = 0
			p.TcAck = false
			b.flush(p)
			return true
		}
	case tcmActive:
		switch {
		case (p.Role != RoleRoot && p.Role != RoleDesignated) || p.OperEdge:
			p.TCM = tcmLearning
			p.RcvdTc, p.RcvdTcn, p.RcvdTcAck, p.TcProp = false, false, false, false
			return true
		case p.RcvdTcn:
			// NOTIFIED_TCN, then NOTIFIED_TC
			b.newTcWhile(p)
			fallthrough
		case p.RcvdTc:
			p.RcvdTcn, p.RcvdTc = false, false
			if p.Role == RoleDesignated {
				p.TcAck = true
			}
			b.setTcPropTree(p)
			b.TopologyChanges++
			b.LastChangeTick = b.ticks
			return true
		case p.TcProp && !p.OperEdge:
			// PROPAGATING
			b.newTcWhile(p)
			p.FdbFlush = true
			b.flush(p)
			p.TcProp = false
			return true
		case p.RcvdTcAck:
			p.TcWhile = 0
			p.RcvdTcAck = false
			return true
		}
	}
	return false
}

// ---- Port Transmit (17.26) ----

func (b *Bridge) ptx(p *PortVars) bool {
	if !p.Enabled {
		p.NewInfo, p.TxCount = true, 0
		return false
	}
	if !p.Selected || p.UpdtInfo {
		return false
	}
	if p.HelloWhen == 0 {
		p.NewInfo = p.NewInfo || p.Role == RoleDesignated || (p.Role == RoleRoot && p.TcWhile != 0)
		p.HelloWhen = b.helloTime(p)
	}
	if !p.NewInfo || p.TxCount >= TxHoldCount {
		return false
	}
	switch {
	case p.SendRSTP:
		b.txRSTP(p)
	case p.Role == RoleRoot:
		b.cb.Send(p.Number, &BPDU{Type: TypeTCN})
	case p.Role == RoleDesignated:
		b.txConfig(p)
		p.TcAck = false
	default:
		return false
	}
	p.TxBPDUs++
	p.TxCount++
	p.NewInfo = false
	return true
}

func (b *Bridge) txConfig(p *PortVars) {
	var f byte
	if p.TcWhile != 0 {
		f |= flagTC
	}
	if p.TcAck {
		f |= flagTCAck
	}
	b.cb.Send(p.Number, &BPDU{Version: 0, Type: TypeConfig, Flags: f, Priority: p.DesignatedPriority, Times: p.DesignatedTimes})
}

func (b *Bridge) txRSTP(p *PortVars) {
	var f byte
	if p.TcWhile != 0 {
		f |= flagTC
	}
	if p.Proposing {
		f |= flagProposal
	}
	switch p.Role {
	case RoleRoot:
		f |= encRoot << roleShift
	case RoleDesignated:
		f |= encDesig << roleShift
	case RoleAlternate, RoleBackup:
		f |= encAltBackup << roleShift
	}
	if p.Learning {
		f |= flagLearning
	}
	if p.Forwarding {
		f |= flagForward
	}
	if p.Agree {
		f |= flagAgree
	}
	b.cb.Send(p.Number, &BPDU{Version: 2, Type: TypeRST, Flags: f, Priority: p.DesignatedPriority, Times: p.DesignatedTimes})
}

// ---- Status and snapshots ----

// PortStatus is a port as shown by "show spanning-tree interface".
type PortStatus struct {
	Number           uint16
	ID               PortID
	Role             Role
	Learning         bool
	Forwarding       bool
	Cost             uint32
	Designated       Vector // the port priority vector (designated bridge and port)
	Edge, OperEdge   bool
	P2P              bool
	RSTP             bool // sending RST BPDUs (false: 802.1D neighbour)
	RootInconsistent bool
	Enabled          bool
	RxBPDUs, TxBPDUs uint64
}

// Ports reports every port.
func (b *Bridge) Ports() []PortStatus {
	var out []PortStatus
	for _, p := range b.sorted() {
		out = append(out, PortStatus{Number: p.Number, ID: p.ID, Role: p.Role, Learning: p.Learning, Forwarding: p.Forwarding,
			Cost: b.cost(p), Designated: p.PortPriority, Edge: p.Config.AdminEdge, OperEdge: p.OperEdge, P2P: p.Config.P2P,
			RSTP: p.SendRSTP, RootInconsistent: p.RootInconsistent, Enabled: p.Enabled, RxBPDUs: p.RxBPDUs, TxBPDUs: p.TxBPDUs})
	}
	return out
}

// Root returns the root priority vector, the root port (0: this bridge is
// root) and the times in use.
func (b *Bridge) Root() (Vector, PortID, Times) { return b.rootPriority, b.rootPort, b.rootTimes }

// Snapshot is the complete bridge state.
type Snapshot struct {
	Ports           []PortVars
	RootPriority    Vector
	RootTimes       Times
	RootPort        PortID
	TopologyChanges uint64
}

// Snapshot copies the state.
func (b *Bridge) Snapshot() Snapshot {
	s := Snapshot{RootPriority: b.rootPriority, RootTimes: b.rootTimes, RootPort: b.rootPort, TopologyChanges: b.TopologyChanges}
	for _, p := range b.sorted() {
		v := *p
		v.msg = nil
		s.Ports = append(s.Ports, v)
	}
	return s
}

// Restore continues from a snapshot on a bridge without ports: the ports
// in cfgs that the snapshot has keep their roles and states (no BEGIN, the
// State callback repeats their state), the others start as new ports.
// Received information gets a fresh rcvdInfoWhile, so nothing ages out
// because the snapshot is a moment old.
func (b *Bridge) Restore(s Snapshot, cfgs map[uint16]PortConfig) {
	b.rootPriority, b.rootTimes, b.rootPort, b.TopologyChanges = s.RootPriority, s.RootTimes, s.RootPort, s.TopologyChanges
	for _, v := range s.Ports {
		cfg, ok := cfgs[v.Number]
		if !ok {
			continue
		}
		p := new(PortVars)
		*p = v
		p.Config = cfg
		p.ID = MakePortID(cfg.Priority, v.Number)
		if p.Info == infoReceived {
			b.updtRcvdInfoWhile(p)
		}
		p.Reselect = true
		b.ports[v.Number] = p
		b.cb.State(p.Number, p.Learning, p.Forwarding)
	}
	for num, cfg := range cfgs {
		if _, ok := b.ports[num]; !ok {
			p := &PortVars{Number: num, Config: cfg, ID: MakePortID(cfg.Priority, num)}
			b.ports[num] = p
			b.begin(p)
		}
	}
	b.run()
}

// Numbers lists the port numbers.
func (b *Bridge) Numbers() []uint16 {
	var out []uint16
	for n := range b.ports {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
