package cli

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"mclag/internal/commit"
	"mclag/internal/config"
	"mclag/internal/lacp"
	"mclag/internal/lldp"
	"mclag/internal/model"
	"mclag/internal/schema"
)

// Operational supplies live data for show and clear commands.
type Operational interface {
	Interfaces() ([]IfStatus, error)
	MACTable() ([]MACEntry, error)
	// ClearMACTable removes learned entries (vlan 0 / iface "" = all).
	ClearMACTable(vlan int, iface string) (int, error)
	// Hardware lists the physical ports of this member (reference 1.6).
	Hardware() ([]HardwarePort, error)
	// Neighbors returns the ARP (ipv6=false) or IPv6 neighbour table.
	Neighbors(ipv6 bool) ([]Neighbor, error)
	// Uptime returns boot and start times and the load averages.
	Uptime() (Uptime, error)
	// NTP reports the NTP client of this member.
	NTP() (NTPStatus, error)
	// Power reboots ("reboot"), halts ("halt") or powers off ("power-off")
	// the member after minutes (0 = now); user is who asked.
	Power(action string, minutes int, user string) error
	// CancelPower cancels a scheduled reboot/halt/power-off.
	CancelPower(user string) error
	// Offload lists the hardware capabilities per port (reference 1.7).
	Offload() ([]OffloadPort, error)
	// Routes lists a routing table ("" = the default instance).
	Routes(instance string) ([]Route, error)
	// VirtualChassis reports the stack and the VC ports of this member.
	VirtualChassis() (VCStatus, error)
	// LACP reports the LACP bundles of this member.
	LACP() ([]lacp.BundleStatus, error)
	// LLDP reports what LLDP announces, its ports, counters and neighbours.
	LLDP() (LLDPStatus, error)
	// MCLAG reports this member's MC-LAG domain (Domain 0: none).
	MCLAG() (MCLAGStatus, error)
	StackMTU() (StackMTUStatus, error)
	Limits() (LimitsStatus, error)
	// SwitchMaster hands mastership to member to (0: the best other member).
	SwitchMaster(to int, user string) error
	// ForceMaster lets this member continue alone after the stack lost
	// its majority (restarts switchd).
	ForceMaster(user string) error
	// Cards lists this member's known cards (present and absent).
	Cards() ([]CardStatus, error)
	// CardInterfaces lists the configured interfaces on these cards.
	CardInterfaces(cards ...int) []string
	// CardRenumber gives present card from the number to; CardForget
	// releases an absent card's number.
	CardRenumber(from, to int, user string) error
	CardForget(card int, user string) error
	// VLANMTUDrops counts frames dropped per VLAN for exceeding its mtu.
	VLANMTUDrops() (map[int]uint64, error)
	// DHCPBindings reports this member's DHCP clients.
	DHCPBindings() ([]DHCPBinding, error)
	// SpanningTree reports the stack's RSTP bridge (from the RSTP owner).
	SpanningTree() (STPStatus, error)
	// Maintenance enters (drains this member; force: despite the checks)
	// or exits maintenance mode; the text reports the outcome.
	Maintenance(enter, force bool, user string) (string, error)
	// RemoveVCMember removes a member from the stack.
	RemoveVCMember(id int, user string) error
	// SetVCPort designates (add) or releases a VC port "<card>/<port>".
	SetVCPort(local string, add bool, user string) error
	// AddVCMember returns a one-time join token for member id.
	AddVCMember(id int, user string) (string, error)
	// JoinVC joins the stack that issued token; it returns the new member id
	// (switchd restarts afterwards).
	JoinVC(token, user string) (int, error)
}

// LLDPStatus is what the show lldp commands need (reference 5.5).
type LLDPStatus struct {
	Running   bool
	System    lldp.System
	Ports     []lldp.PortSpec
	Stats     []lldp.Stats
	Neighbors []lldp.Neighbor
}

// VCStatus is what "show virtual-chassis" shows.
type VCStatus struct {
	StackID  string
	Member   int
	HostName string
	Ports    []VCPort
	// Control: the replicated stack state is running (else every switch
	// shows itself as master).
	Control bool
	Master  int // 0: no master (no majority)
	// Members in the member list; Voters: members that vote; Reachable:
	// members this member can reach over the stacking links.
	Members, Voters, Reachable []int
	// Maintenance: members in maintenance mode.
	Maintenance []int
}

// VCPort is one VC port.
type VCPort struct {
	Port, Linux, State, Neighbor, PeerPort, LastError string
	UpSince                                           time.Time
	SpeedMbps                                         int // 0: unknown
}

// LimitsStatus is what "show system limits" needs from this member's
// hardware and kernel (frame sizes are frame sizes: the MTU plus the
// Ethernet header).
type LimitsStatus struct {
	Member                        int
	Ports, StackPorts             int
	LowestMaxMTU, HighestMaxMTU   int // hardware maximum per port (0: none reports one)
	LowestMaxPort, HighestMaxPort string
	FastestMbps                   int
	FastestPort                   string
	MACEntries                    int
}

// StackMTUStatus is "show virtual-chassis mtu" of one member (frame sizes,
// reference 1.3).
type StackMTUStatus struct {
	Member  int
	DataMTU int    // the largest data mtu in the stack
	Where   string // the statement that sets it
	Stack   bool   // there are other switch members (stack tunnels exist)
	Ports   []StackMTUPort
}

// StackMTUPort is a stacking port: its current and largest frame size.
type StackMTUPort struct {
	Port        string
	MTU, MaxMTU int
	// PathMTU is what probe frames verified the cable carries (0: not
	// known: the link is down or the first probe round is running).
	PathMTU int
}

// MCLAGStatus is "show mclag".
type MCLAGStatus struct {
	Domain, Member, Peer int
	Primary              bool
	PeerReachable        bool // over the stack (and so its stack tunnel)
	PeerKnown            bool // leg states received from the peer
	PeerSeen             time.Time
	Reach, Members       int // switch members reached (itself included) / in the stack
	Bundles              []MCLAGBundle
}

// MCLAGBundle is one MC-LAG bundle of "show mclag".
type MCLAGBundle struct {
	Name                       string
	LocalUp, PeerUp, PeerKnown bool
	SplitHorizon               bool
	Hold                       string // reason ("": not held)
	// Consistency check: what each member applies ("": unknown).
	Facts, PeerFacts string
	DiffersSince     time.Time // zero: consistent
}

// Route is one line of "show route".
type Route struct {
	Dest, Via, Proto string
	Metric           int
}

// OffloadPort is one line of "show system offload". Feature values are
// "on", "off" (available but off) or "-" (not supported).
type OffloadPort struct {
	Name, Linux, Driver            string
	MaxSpeedMbps                   int
	Pause                          string // yes, no, "-" (unknown)
	Switchdev                      bool
	TC, VLANFilter, Csum, TSO, GRO string
}

// Neighbor is one entry of "show arp" / "show ipv6 neighbors".
type Neighbor struct {
	MAC, IP, Interface, Instance, State string
}

// NTPStatus is what "show system ntp" needs.
type NTPStatus struct {
	Synced     bool
	LastAdjust time.Time
	LastStep   bool
	Via        string // routing instance the queries leave through ("" = default)
	Servers    []NTPServerStatus
}

// NTPServerStatus is one configured NTP server.
type NTPServerStatus struct {
	Host     string
	Prefer   bool
	Addr     string
	Stratum  int
	Offset   time.Duration
	Delay    time.Duration
	LastPoll time.Time
	Reach    bool
	Err      string
	Selected bool
}

// Uptime is what "show system uptime" needs from the host.
type Uptime struct {
	Booted, Started time.Time
	Load            [3]float64
}

// HardwarePort is one line of "show chassis hardware".
type HardwarePort struct {
	Name, Linux, Bus, Driver, MAC string
}

// IfStatus is one interface as shown by "show interfaces".
type IfStatus struct {
	Name        string // configuration name (1/0/3, ae1)
	Linux       string
	Configured  bool
	Role        string // e.g. "access v10", "trunk", "member of ae1", "plain"
	AdminUp     bool
	OperUp      bool
	MTU         int // frame size including the Ethernet header
	SpeedMbps   int
	Description string
	MAC         string
	VLANs       []string // "v10 (10, untagged)"
	Addrs       []string // IP addresses of a routed unit
	Counters    IfCounters
	TaggedDrops uint64
}

// IfCounters are interface statistics.
type IfCounters struct {
	RxPackets, TxPackets, RxBytes, TxBytes   uint64
	RxErrors, TxErrors, RxDropped, TxDropped uint64
	RxMulticast                              uint64
}

// MACEntry is one MAC table entry.
type MACEntry struct {
	VLAN      int
	VLANName  string
	MAC       string
	Interface string
	Static    bool
	Age       int
}

// Logs supplies the local log buffer and the syslog forwarders.
type Logs interface {
	Recent() []LogLine
	Forwarders() []ForwarderStatus
}

// LogLine is one buffered log message.
type LogLine struct {
	Time     time.Time
	Facility string
	Severity string
	Text     string
}

// ForwarderStatus describes one remote syslog server.
type ForwarderStatus struct {
	Target    string // host:port/transport
	Filter    string // facility/severity
	Connected bool
	Sent      uint64
	Dropped   uint64
	Queued    int
	LastError string
}

func (sh *Shell) showLog(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if sh.env.Logs == nil {
		return errors.New("log buffer not available")
	}
	host := sh.env.HostName()
	for _, l := range sh.env.Logs.Recent() {
		fmt.Fprintf(c.out, "%s %s %s.%s: %s\n", l.Time.Local().Format("2006-01-02 15:04:05"), host, l.Facility, l.Severity, l.Text)
	}
	return nil
}

func (sh *Shell) showSyslog(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if sh.env.Logs == nil {
		return errors.New("syslog not available")
	}
	fs := sh.env.Logs.Forwarders()
	if len(fs) == 0 {
		c.out.WriteString("No remote syslog servers configured.\n")
		return nil
	}
	for _, f := range fs {
		state := "connected"
		if !f.Connected {
			state = "not connected"
		}
		fmt.Fprintf(c.out, "%s (%s): %s, sent %d, queued %d, dropped %d\n", f.Target, f.Filter, state, f.Sent, f.Queued, f.Dropped)
		if f.LastError != "" {
			fmt.Fprintf(c.out, "  last error: %s\n", f.LastError)
		}
	}
	return nil
}

var errNoOps = errors.New("operational data is not available (switchd data plane not running)")

// PartialError is returned by the stack-wide listings of Operational when
// some members did not answer: the rows of the others are complete.
type PartialError struct{ Members []int }

func (e *PartialError) Error() string {
	ids := make([]string, len(e.Members))
	for i, m := range e.Members {
		ids[i] = strconv.Itoa(m)
	}
	return "no answer from member " + strings.Join(ids, ", ")
}

// partial turns a PartialError into a warning line (the listing goes on)
// and returns every other error.
func partial(c *call, err error) error {
	var pe *PartialError
	if errors.As(err, &pe) {
		fmt.Fprintf(c.out, "warning: %v; its interfaces are missing below\n", pe)
		return nil
	}
	return err
}

func (sh *Shell) ops() (Operational, error) {
	if sh.env.Ops == nil {
		return nil, errNoOps
	}
	return sh.env.Ops, nil
}

func speed(mbps int) string {
	switch {
	case mbps <= 0:
		return "-"
	case mbps >= 1000 && mbps%1000 == 0:
		return strconv.Itoa(mbps/1000) + "G"
	}
	return strconv.Itoa(mbps) + "M"
}

func upDown(b bool) string {
	if b {
		return "up"
	}
	return "down"
}

// showInterfaces: show interfaces [terse|extensive] [<name>]
func (sh *Shell) showInterfaces(c *call) error {
	o, err := sh.ops()
	if err != nil {
		return err
	}
	mode, name := "", ""
	for _, a := range c.args {
		switch {
		case !a.Quoted && prefixOf(a.Text, "terse") && mode == "":
			mode = "terse"
		case !a.Quoted && prefixOf(a.Text, "extensive") && mode == "":
			mode = "extensive"
		case name == "":
			name = a.Text
		default:
			return &posError{pos: a.Pos, msg: "syntax error"}
		}
	}
	ifs, err := o.Interfaces()
	if err := partial(c, err); err != nil {
		return err
	}
	ifs = slices.DeleteFunc(ifs, func(i IfStatus) bool { return !c.shows(i.Name) })
	sort.Slice(ifs, func(i, j int) bool { return config.NaturalLess(ifs[i].Name, ifs[j].Name) })
	if name != "" {
		var sel []IfStatus
		for _, i := range ifs {
			if i.Name == name || i.Linux == name {
				sel = append(sel, i)
			}
		}
		if len(sel) == 0 {
			return fmt.Errorf("interface %s not found", name)
		}
		ifs = sel
	}
	if mode == "terse" {
		fmt.Fprintf(c.out, "%-14s %-5s %-5s %-6s %-6s %-22s %s\n", "Interface", "Admin", "Link", "MTU", "Speed", "Role", "Description")
		for _, i := range ifs {
			role := i.Role
			if !i.Configured {
				role = "(not configured)"
			}
			fmt.Fprintf(c.out, "%-14s %-5s %-5s %-6d %-6s %-22s %s\n", i.Name, upDown(i.AdminUp), upDown(i.OperUp), i.MTU, speed(i.SpeedMbps), role, i.Description)
		}
		return nil
	}
	for n, i := range ifs {
		if n > 0 {
			c.out.WriteString("\n")
		}
		fmt.Fprintf(c.out, "Interface: %s, Enabled: %s, Link: %s\n", i.Name, upDown(i.AdminUp), upDown(i.OperUp))
		if i.Description != "" {
			fmt.Fprintf(c.out, "  Description: %s\n", i.Description)
		}
		cfg := "yes"
		if !i.Configured {
			cfg = "no (not managed by switchd)"
		}
		fmt.Fprintf(c.out, "  Linux name: %s, MAC: %s, Speed: %s, MTU: %d\n", i.Linux, i.MAC, speed(i.SpeedMbps), i.MTU)
		fmt.Fprintf(c.out, "  Configured: %s, Role: %s\n", cfg, i.Role)
		if len(i.Addrs) > 0 {
			fmt.Fprintf(c.out, "  Addresses: %s\n", strings.Join(i.Addrs, ", "))
		}
		if len(i.VLANs) > 0 {
			fmt.Fprintf(c.out, "  VLANs: %s\n", strings.Join(i.VLANs, ", "))
		}
		k := i.Counters
		fmt.Fprintf(c.out, "  Input:  %d packets, %d bytes\n  Output: %d packets, %d bytes\n", k.RxPackets, k.RxBytes, k.TxPackets, k.TxBytes)
		if mode == "extensive" {
			fmt.Fprintf(c.out, "  Input errors: %d, drops: %d, multicast: %d\n", k.RxErrors, k.RxDropped, k.RxMulticast)
			fmt.Fprintf(c.out, "  Output errors: %d, drops: %d\n", k.TxErrors, k.TxDropped)
			fmt.Fprintf(c.out, "  Tagged frames dropped (access port): %d\n", i.TaggedDrops)
		}
	}
	return nil
}

// macFilter parses "[vlan <v>] [interface <i>]".
func (sh *Shell) macFilter(c *call) (vlan int, vlanName, iface string, err error) {
	cfg := sh.activeModel()
	for i := 0; i < len(c.args); i++ {
		a := c.args[i]
		if i+1 >= len(c.args) {
			return 0, "", "", &posError{pos: len(c.line), msg: "missing argument"}
		}
		v := c.args[i+1].Text
		switch {
		case prefixOf(a.Text, "vlan") && vlan == 0:
			if x, ok := cfg.VLANs[v]; ok {
				vlan, vlanName = x.ID, v
			} else if id, err := strconv.Atoi(v); err == nil && id > 0 && id < 4095 {
				vlan = id
			} else {
				return 0, "", "", &posError{pos: c.args[i+1].Pos, msg: "unknown VLAN"}
			}
		case prefixOf(a.Text, "interface") && iface == "":
			iface = v
		default:
			return 0, "", "", &posError{pos: a.Pos, msg: "syntax error, expecting 'vlan <vlan>' or 'interface <interface>'"}
		}
		i++
	}
	return vlan, vlanName, iface, nil
}

func (sh *Shell) activeModel() *model.Config {
	cfg, _ := model.Build(sh.env.Engine.Active(), nil)
	return cfg
}

// showMACTable: show ethernet-switching table [vlan <v>] [interface <i>]
func (sh *Shell) showMACTable(c *call) error {
	o, err := sh.ops()
	if err != nil {
		return err
	}
	vlan, _, iface, err := sh.macFilter(c)
	if err != nil {
		return err
	}
	es, err := o.MACTable()
	if err := partial(c, err); err != nil {
		return err
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].VLAN != es[j].VLAN {
			return es[i].VLAN < es[j].VLAN
		}
		return es[i].MAC < es[j].MAC
	})
	fmt.Fprintf(c.out, "%-12s %-5s %-18s %-8s %-6s %s\n", "VLAN name", "Tag", "MAC address", "Type", "Age", "Interface")
	n := 0
	for _, e := range es {
		if (vlan != 0 && e.VLAN != vlan) || (iface != "" && e.Interface != iface) || !c.shows(e.Interface) {
			continue
		}
		typ, age := "learned", strconv.Itoa(e.Age)
		if e.Static {
			typ, age = "static", "-"
		}
		fmt.Fprintf(c.out, "%-12s %-5d %-18s %-8s %-6s %s\n", e.VLANName, e.VLAN, e.MAC, typ, age, e.Interface)
		n++
	}
	fmt.Fprintf(c.out, "%d entries\n", n)
	return nil
}

// clearMACTable: clear ethernet-switching table [vlan <v>] [interface <i>]
func (sh *Shell) clearMACTable(c *call) error {
	o, err := sh.ops()
	if err != nil {
		return err
	}
	vlan, _, iface, err := sh.macFilter(c)
	if err != nil {
		return err
	}
	n, err := o.ClearMACTable(vlan, iface)
	if err := partial(c, err); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%d entries cleared\n", n)
	return nil
}

// showVLANs lists VLANs and their ports from the active configuration.
func (sh *Shell) showHardware(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if sh.env.Ops == nil {
		return errors.New("hardware information is not available")
	}
	ports, err := sh.env.Ops.Hardware()
	if err := partial(c, err); err != nil {
		return err
	}
	sort.SliceStable(ports, func(i, j int) bool { return config.NaturalLess(ports[i].Name, ports[j].Name) })
	fmt.Fprintf(c.out, "%-10s %-16s %-14s %-12s %s\n", "Interface", "Linux name", "Bus address", "Driver", "MAC address")
	for _, p := range ports {
		if c.shows(p.Name) {
			fmt.Fprintf(c.out, "%-10s %-16s %-14s %-12s %s\n", p.Name, p.Linux, p.Bus, p.Driver, p.MAC)
		}
	}
	sh.showCards(c)
	return nil
}

// setVCPort implements "request virtual-chassis vc-port set|delete
// <interface>" for a port of any member (the request goes to that member),
// and the Junos form "... pic-slot <card> port <port>" for this switch.
func (sh *Shell) setVCPort(c *call, add bool) error {
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	verb := "delete"
	if add {
		verb = "set"
	}
	var name string
	switch {
	case len(c.args) == 1:
		p, ok := schema.ParsePhysical(c.args[0].Text)
		if !ok {
			return &posError{pos: c.argPos(0), msg: "expecting a port, e.g. 1/1/0"}
		}
		name = p.String()
	case len(c.args) == 4 && prefixOf(c.args[0].Text, "pic-slot") && prefixOf(c.args[2].Text, "port"):
		card, err1 := strconv.Atoi(c.args[1].Text)
		port, err2 := strconv.Atoi(c.args[3].Text)
		if err1 != nil || card < 0 || card > schema.MaxCard {
			return &posError{pos: c.argPos(1), msg: fmt.Sprintf("expecting a card number (0-%d)", schema.MaxCard)}
		}
		if err2 != nil || port < 0 || port > schema.MaxPort {
			return &posError{pos: c.argPos(3), msg: fmt.Sprintf("expecting a port number (0-%d)", schema.MaxPort)}
		}
		st, err := sh.env.Ops.VirtualChassis()
		if err != nil && !errors.As(err, new(*PartialError)) {
			return err
		}
		name = fmt.Sprintf("%d/%d/%d", st.Member, card, port)
	default:
		return &posError{pos: c.argPos(0), msg: "syntax error, expecting a port (e.g. 1/1/0)"}
	}
	p, _ := schema.ParsePhysical(name)
	// A configured data port would be taken away from the data plane.
	if cfg := sh.activeModel(); add && cfg != nil && cfg.Interfaces[name] != nil {
		return fmt.Errorf("%s is configured under 'interfaces'; delete that configuration first", name)
	}
	if sh.env.Stack != nil && p.Member != sh.env.Stack.Self() {
		if !slices.Contains(sh.env.Stack.Members(), p.Member) {
			return fmt.Errorf("member %d is not in this virtual chassis (a switch that has not joined yet is member 1 of its own: run the command on it)", p.Member)
		}
		out, err := sh.env.Stack.Exec(c.ctx, p.Member, "request virtual-chassis vc-port "+verb+" "+name, true)
		c.out.WriteString(out)
		return err
	}
	st, err := sh.env.Ops.VirtualChassis()
	if err != nil && !errors.As(err, new(*PartialError)) {
		return err
	}
	if st.Member != 0 && p.Member != st.Member {
		return fmt.Errorf("%s is a port of member %d; this switch is member %d", name, p.Member, st.Member)
	}
	return sh.env.Ops.SetVCPort(fmt.Sprintf("%d/%d", p.Card, p.Port), add, sh.env.User)
}

func (sh *Shell) addVCMember(c *call) error {
	if len(c.args) != 1 {
		return &posError{pos: c.argPos(0), msg: "expecting the member id"}
	}
	id, err := strconv.Atoi(c.args[0].Text)
	if err != nil || id < 1 || id > 16 {
		return &posError{pos: c.argPos(0), msg: "expecting a member id (1-16)"}
	}
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	tok, err := sh.env.Ops.AddVCMember(id, sh.env.User)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Join token for member %d (valid for one hour, usable once):\n\n    %s\n\n", id, tok)
	fmt.Fprintf(c.out, "On the new switch (with its VC ports cabled to this stack):\n    request virtual-chassis join token %s\n", tok)
	return nil
}

func (sh *Shell) joinVC(c *call) error {
	if len(c.args) != 2 || !prefixOf(c.args[0].Text, "token") {
		return &posError{pos: c.argPos(0), msg: "syntax error, expecting 'token <token>'"}
	}
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	a, err := c.term.Ask("This switch's configuration is replaced by the virtual chassis configuration (the current one is kept\n"+
		"as a file) and switchd restarts with its new member id. Continue? [yes,no] (no) ", true)
	if err != nil || !isYes(a) {
		return nil
	}
	c.out.WriteString("Joining (up to a minute)...\n")
	id, err := sh.env.Ops.JoinVC(c.args[1].Text, sh.env.User)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Joined as member %d. switchd restarts now; interface names become %d/<card>/<port>.\n", id, id)
	return nil
}

func completeVCPort(sh *Shell, args []config.Token, partial string) []Completion {
	switch {
	case len(args) == 0:
		var out []Completion
		for _, p := range sh.ports() {
			out = append(out, Completion{Text: p, Help: "Port"})
		}
		return append(filter(out, partial), Completion{Text: "<interface>", Help: "Port of any member, e.g. 1/1/0", Placeholder: true})
	case len(args) >= 1 && prefixOf(args[0].Text, "pic-slot"):
		switch len(args) {
		case 1:
			return []Completion{{Text: "<card>", Help: "Card number of this switch's port", Placeholder: true}}
		case 2:
			return filter([]Completion{{Text: "port", Help: "Port number on the card"}}, partial)
		case 3:
			return []Completion{{Text: "<port>", Help: "Port number", Placeholder: true}}
		}
	}
	return []Completion{enter}
}

func (sh *Shell) vcStatus() (VCStatus, error) {
	if sh.env.Ops == nil {
		return VCStatus{}, errors.New("virtual chassis information is not available")
	}
	st, err := sh.env.Ops.VirtualChassis()
	if errors.As(err, new(*PartialError)) {
		err = nil // the overview is this member's view; vc-port warns about missing ports
	}
	return st, err
}

func (sh *Shell) showVC(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	st, err := sh.vcStatus()
	if err != nil {
		return err
	}
	cfg := sh.activeModel()
	fmt.Fprintf(c.out, "Virtual chassis %s, this switch is member %d\n", st.StackID, st.Member)
	switch {
	case !st.Control:
		c.out.WriteString("Stack control is not running: this switch works as its own master.\n")
	case st.Master == 0:
		c.out.WriteString("No master: the stack has no majority (configuration changes are not possible).\n")
	}
	c.out.WriteString("\n")
	fmt.Fprintf(c.out, "%-7s %-20s %-9s %-9s %-10s %s\n", "Member", "Host name", "Role", "Priority", "Vote", "Status")
	present := map[int]bool{st.Member: true}
	host := map[int]string{st.Member: st.HostName}
	for _, id := range st.Reachable {
		present[id] = true
	}
	for _, p := range st.Ports {
		var id int
		if n, _ := fmt.Sscanf(p.Neighbor, "member %d", &id); n == 1 && p.State == "up" {
			present[id] = true
		}
	}
	ids := map[int]bool{st.Member: true}
	inList, voter := map[int]bool{}, map[int]bool{}
	for _, id := range st.Members {
		ids[id], inList[id] = true, true
	}
	for _, id := range st.Voters {
		voter[id] = true
	}
	if cfg != nil {
		for id := range cfg.Members {
			ids[id] = true
		}
	}
	prioOf := func(id int) int {
		if cfg != nil && cfg.Members[id] != nil {
			return cfg.Members[id].Priority
		}
		return 128
	}
	// backup: the voter with the highest priority after the master.
	backup, bp := 0, -1
	for _, id := range st.Voters {
		if id != st.Master && (prioOf(id) > bp || (prioOf(id) == bp && id < backup)) {
			backup, bp = id, prioOf(id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		name := host[id]
		if cfg != nil && cfg.Members[id] != nil && cfg.Members[id].HostName != "" {
			name = cfg.Members[id].HostName
		}
		role, status, vote := "linecard", "not present", "-"
		if present[id] {
			status = "present"
			if slices.Contains(st.Maintenance, id) {
				status = "maintenance"
			}
		}
		switch {
		case !st.Control && id == st.Member:
			role = "master"
		case id == st.Master:
			role = "master"
		case st.Master != 0 && id == backup:
			role = "backup"
		}
		switch {
		case voter[id]:
			vote = "voter"
		case inList[id]:
			vote = "non-voter"
		}
		if st.Control && len(st.Members) > 0 && !inList[id] {
			status = "not joined"
		}
		fmt.Fprintf(c.out, "%-7d %-20s %-9s %-9d %-10s %s\n", id, name, role, prioOf(id), vote, status)
	}
	return nil
}

func (sh *Shell) switchMaster(c *call) error {
	to := 0
	switch {
	case len(c.args) == 0:
	case len(c.args) == 2 && prefixOf(c.args[0].Text, "member"):
		id, err := strconv.Atoi(c.args[1].Text)
		if err != nil || id < 1 || id > 16 {
			return &posError{pos: c.argPos(1), msg: "expecting a member id (1-16)"}
		}
		to = id
	default:
		return &posError{pos: c.argPos(0), msg: "syntax error, expecting 'member <id>' or nothing"}
	}
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	if err := sh.env.Ops.SwitchMaster(to, sh.env.User); err != nil {
		return err
	}
	c.out.WriteString("Mastership handed over; configuration sessions on the old master end (the shared candidate is kept).\n")
	return nil
}

func (sh *Shell) forceMaster(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	a, err := c.term.Ask("This member becomes master on its own, although the stack has lost its majority.\n"+
		"If the other members are still running (only the cables between you and them are cut), both sides\n"+
		"will commit different configurations and the changes of one side are lost when they meet again.\n"+
		"Use it only when the others are really down. switchd restarts. Continue? [yes,no] (no) ", true)
	if err != nil || !isYes(a) {
		return nil
	}
	if err := sh.env.Ops.ForceMaster(sh.env.User); err != nil {
		return err
	}
	c.out.WriteString("switchd restarts; this member becomes master and configuration works again in a few seconds.\n")
	return nil
}

// maintenance is "request system maintenance-mode enter [force]|exit".
func (sh *Shell) maintenance(c *call, enter bool) error {
	force := false
	switch {
	case len(c.args) == 0:
	case enter && len(c.args) == 1 && prefixOf(c.args[0].Text, "force"):
		force = true
	case enter:
		return &posError{pos: c.argPos(0), msg: "syntax error, expecting 'force' or nothing"}
	default:
		return noArgs(c)
	}
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	if enter {
		a, err := c.term.Ask("Drain this member and put it into maintenance mode ? [yes,no] (no) ", true)
		if err != nil || !isYes(a) {
			return nil
		}
	}
	text, err := sh.env.Ops.Maintenance(enter, force, sh.env.User)
	c.out.WriteString(text)
	return err
}

func (sh *Shell) removeVCMember(c *call) error {
	if len(c.args) != 1 {
		return &posError{pos: c.argPos(0), msg: "expecting the member id"}
	}
	id, err := strconv.Atoi(c.args[0].Text)
	if err != nil || id < 1 || id > 16 {
		return &posError{pos: c.argPos(0), msg: "expecting a member id (1-16)"}
	}
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	a, err := c.term.Ask(fmt.Sprintf("Member %d leaves the stack: it loses its stack keys and keeps running standalone.\n"+
		"Its configuration in the stack stays until you delete it. Continue? [yes,no] (no) ", id), true)
	if err != nil || !isYes(a) {
		return nil
	}
	if err := sh.env.Ops.RemoveVCMember(id, sh.env.User); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Member %d removed from the virtual chassis.\n", id)
	return nil
}

func (sh *Shell) showVCPorts(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	st, err := sh.env.Ops.VirtualChassis()
	if err := partial(c, err); err != nil {
		return err
	}
	st.Ports = slices.DeleteFunc(st.Ports, func(p VCPort) bool { return !c.shows(p.Port) })
	sort.SliceStable(st.Ports, func(i, j int) bool { return config.NaturalLess(st.Ports[i].Port, st.Ports[j].Port) })
	if len(st.Ports) == 0 {
		c.out.WriteString("No VC ports ('request virtual-chassis vc-port set <interface>').\n")
		return nil
	}
	fmt.Fprintf(c.out, "%-10s %-12s %-7s %-7s %-24s %-10s %s\n", "Port", "Linux name", "State", "Speed", "Neighbor", "Peer port", "Up")
	now := time.Now()
	for _, p := range st.Ports {
		up := "-"
		if !p.UpSince.IsZero() {
			up = fmtDuration(now.Sub(p.UpSince))
		}
		speed := "-"
		if p.SpeedMbps > 0 {
			speed = fmtSpeed(p.SpeedMbps)
		}
		fmt.Fprintf(c.out, "%-10s %-12s %-7s %-7s %-24s %-10s %s\n", p.Port, p.Linux, p.State, speed, p.Neighbor, dash(p.PeerPort), up)
		switch {
		case p.LastError != "" && p.State != "up":
			fmt.Fprintf(c.out, "           last error: %s\n", p.LastError)
		case p.LastError != "" && p.Neighbor == "other stack":
			fmt.Fprintf(c.out, "           detail: %s\n", p.LastError)
		}
	}
	return nil
}

// showStackMTU is "show virtual-chassis mtu" (reference 5.2, stack MTU).
func (sh *Shell) showStackMTU(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if sh.env.Ops == nil {
		return errors.New("stack information is not available")
	}
	st, err := sh.env.Ops.StackMTU()
	if err := partial(c, err); err != nil {
		return err
	}
	st.Ports = slices.DeleteFunc(st.Ports, func(p StackMTUPort) bool { return !c.shows(p.Port) })
	sort.SliceStable(st.Ports, func(i, j int) bool { return config.NaturalLess(st.Ports[i].Port, st.Ports[j].Port) })
	need := st.DataMTU + model.StackOverhead
	fmt.Fprintf(c.out, "Frame sizes including the Ethernet header, without VLAN tags (reference 1.3)\n")
	fmt.Fprintf(c.out, "  Largest data mtu in the stack:  %d (%s; hosts up to MTU %d)\n", st.DataMTU, st.Where, st.DataMTU-model.EthHeader)
	if !st.Stack {
		c.out.WriteString("  No other switch member: no stack tunnels.\n")
	} else {
		fmt.Fprintf(c.out, "  Needed on the stacking links:   %d (+%d: tunnel 50, VLAN tags 8)\n", need, model.StackOverhead)
	}
	if len(st.Ports) == 0 {
		c.out.WriteString("\nNo stacking ports.\n")
		return nil
	}
	limit := 0
	for _, p := range st.Ports {
		if p.MaxMTU > 0 && (limit == 0 || p.MaxMTU < limit) {
			limit = p.MaxMTU
		}
	}
	if limit > 0 {
		carry := min(limit-model.StackOverhead, 16000) // the largest configurable mtu
		fmt.Fprintf(c.out, "  The stacking ports allow data mtu up to %d (hosts up to MTU %d)\n", carry, carry-model.EthHeader)
	}
	fmt.Fprintf(c.out, "\n  %-8s %-7s %-8s %-9s %s\n", "Port", "MTU", "Maximum", "Verified", "Status")
	for _, p := range st.Ports {
		status := "ok"
		switch {
		case !st.Stack:
			status = "-"
		case p.MaxMTU > 0 && p.MaxMTU < need:
			status = fmt.Sprintf("too small: the NIC carries at most %d", p.MaxMTU)
		case p.MTU < need:
			status = "too small (set to the maximum when switchd starts)"
		case p.PathMTU > 0 && p.PathMTU+model.EthHeader < need:
			status = fmt.Sprintf("the cable carries only %d: larger frames are lost (check media converters, bridges, switches in between)", p.PathMTU+model.EthHeader)
		}
		max, verified := "unknown", "-"
		if p.MaxMTU > 0 {
			max = strconv.Itoa(p.MaxMTU)
		}
		if p.PathMTU > 0 {
			verified = strconv.Itoa(p.PathMTU + model.EthHeader)
		}
		fmt.Fprintf(c.out, "  %-8s %-7d %-8s %-9s %s\n", p.Port, p.MTU, max, verified, status)
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// showRoute implements "show route [instance <name>]".
func (sh *Shell) showRoute(c *call) error {
	instance := ""
	switch {
	case len(c.args) == 0:
	case len(c.args) == 2 && prefixOf(c.args[0].Text, "instance"):
		instance = c.args[1].Text
	default:
		return &posError{pos: c.argPos(0), msg: "syntax error, expecting 'instance <name>'"}
	}
	if sh.env.Ops == nil {
		return errors.New("routing information is not available")
	}
	rs, err := sh.env.Ops.Routes(instance)
	if err != nil {
		return err
	}
	name := instance
	if name == "" {
		name = "default"
	}
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := strings.Contains(rs[i].Dest, ":"), strings.Contains(rs[j].Dest, ":")
		if a != b {
			return !a // IPv4 first
		}
		return config.NaturalLess(rs[i].Dest, rs[j].Dest)
	})
	fmt.Fprintf(c.out, "Routing instance %s: %d routes\n", name, len(rs))
	fmt.Fprintf(c.out, "%-28s %-8s %-7s %s\n", "Destination", "Source", "Metric", "Next hop")
	for _, r := range rs {
		fmt.Fprintf(c.out, "%-28s %-8s %-7d %s\n", r.Dest, r.Proto, r.Metric, r.Via)
	}
	return nil
}

func completeRoute(sh *Shell, args []config.Token, partial string) []Completion {
	switch len(args) {
	case 0:
		return filter([]Completion{enter, {Text: "instance", Help: "Routing instance"}}, partial)
	case 1:
		var out []Completion
		for _, e := range sh.env.Engine.Active().Root.Entries("routing-instances") {
			out = append(out, Completion{Text: e.Key, Help: "Routing instance"})
		}
		return filter(out, partial)
	}
	return []Completion{enter}
}

func (sh *Shell) showARP(c *call) error {
	if len(c.args) > 1 || (len(c.args) == 1 && !prefixOf(c.args[0].Text, "no-resolve")) {
		return &posError{pos: c.argPos(0), msg: "syntax error, expecting 'no-resolve'"}
	}
	return sh.showNeighbors(c, false)
}

func (sh *Shell) showNDP(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	return sh.showNeighbors(c, true)
}

// showNeighbors prints the neighbour table (host names are never
// resolved, so "no-resolve" is the only behaviour).
func (sh *Shell) showNeighbors(c *call, ipv6 bool) error {
	if sh.env.Ops == nil {
		return errors.New("neighbour information is not available")
	}
	ns, err := sh.env.Ops.Neighbors(ipv6)
	if err := partial(c, err); err != nil {
		return err
	}
	ns = slices.DeleteFunc(ns, func(n Neighbor) bool { return !c.shows(n.Interface) })
	sort.Slice(ns, func(i, j int) bool {
		if ns[i].Instance != ns[j].Instance {
			return ns[i].Instance < ns[j].Instance
		}
		if ns[i].Interface != ns[j].Interface {
			return config.NaturalLess(ns[i].Interface, ns[j].Interface)
		}
		return config.NaturalLess(ns[i].IP, ns[j].IP)
	})
	w := 16
	for _, n := range ns {
		w = max(w, len(n.IP))
	}
	fmt.Fprintf(c.out, "%-18s %-*s %-12s %-10s %s\n", "MAC Address", w, "Address", "Interface", "Instance", "State")
	for _, n := range ns {
		fmt.Fprintf(c.out, "%-18s %-*s %-12s %-10s %s\n", n.MAC, w, n.IP, n.Interface, n.Instance, n.State)
	}
	fmt.Fprintf(c.out, "Total entries: %d\n", len(ns))
	return nil
}

func (sh *Shell) showOffload(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if sh.env.Ops == nil {
		return errors.New("hardware information is not available")
	}
	ps, err := sh.env.Ops.Offload()
	if err := partial(c, err); err != nil {
		return err
	}
	ps = slices.DeleteFunc(ps, func(p OffloadPort) bool { return !c.shows(p.Name) })
	sort.SliceStable(ps, func(i, j int) bool { return config.NaturalLess(ps[i].Name, ps[j].Name) })
	fmt.Fprintf(c.out, "%-10s %-12s %-11s %-6s %-5s %-9s %-4s %-5s %-4s %-4s %s\n",
		"Interface", "Linux name", "Driver", "Speed", "Pause", "Switchdev", "TC", "VLAN", "Csum", "TSO", "GRO")
	for _, p := range ps {
		sd := "no"
		if p.Switchdev {
			sd = "yes"
		}
		fmt.Fprintf(c.out, "%-10s %-12s %-11s %-6s %-5s %-9s %-4s %-5s %-4s %-4s %s\n",
			p.Name, p.Linux, p.Driver, speed(p.MaxSpeedMbps), p.Pause, sd, p.TC, p.VLANFilter, p.Csum, p.TSO, p.GRO)
	}
	c.out.WriteString("Speed: highest supported link speed. TC: tc rule offload (storm control, filters). VLAN: VLAN filter offload.\n" +
		"on = active, off = available but off, - = not supported by the NIC or driver.\n")
	return nil
}

func (sh *Shell) showUptime(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if sh.env.Ops == nil {
		return errors.New("system information is not available")
	}
	u, err := sh.env.Ops.Uptime()
	if err != nil {
		return err
	}
	now := time.Now()
	ts := func(t time.Time) string {
		return fmt.Sprintf("%s (%s ago)", t.Format("2006-01-02 15:04:05 MST"), fmtDuration(now.Sub(t)))
	}
	fmt.Fprintf(c.out, "Current time: %s\n", now.Format("2006-01-02 15:04:05 MST"))
	if !u.Booted.IsZero() {
		fmt.Fprintf(c.out, "System booted: %s\n", ts(u.Booted))
	}
	if !u.Started.IsZero() {
		fmt.Fprintf(c.out, "switchd started: %s\n", ts(u.Started))
	}
	if h := sh.env.Engine.History(); len(h) > 0 {
		fmt.Fprintf(c.out, "Last configured: %s by %s\n", ts(h[0].Time.Local()), h[0].User)
	}
	fmt.Fprintf(c.out, "Load averages: %.2f %.2f %.2f (1, 5, 15 minutes)\n", u.Load[0], u.Load[1], u.Load[2])
	return nil
}

func (sh *Shell) showNTP(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if sh.env.Ops == nil {
		return errors.New("system information is not available")
	}
	st, err := sh.env.Ops.NTP()
	if err != nil {
		return err
	}
	if len(st.Servers) == 0 {
		c.out.WriteString("No NTP servers configured ('set system ntp server <host>').\n")
		return nil
	}
	now := time.Now()
	via := st.Via
	if via == "" {
		via = "the default routing instance"
	}
	if st.Synced {
		how := "slewed"
		if st.LastStep {
			how = "stepped"
		}
		fmt.Fprintf(c.out, "Synchronized: yes, clock %s %s ago\n", how, fmtDuration(now.Sub(st.LastAdjust)))
	} else {
		c.out.WriteString("Synchronized: no\n")
	}
	fmt.Fprintf(c.out, "Queries leave through: %s\n\n", via)
	fmt.Fprintf(c.out, "  %-28s %-16s %-3s %-12s %-10s %s\n", "Server", "Address", "St", "Offset", "Delay", "Last poll")
	for _, sv := range st.Servers {
		mark := " "
		if sv.Selected {
			mark = "*"
		}
		name := sv.Host
		if sv.Prefer {
			name += " (prefer)"
		}
		if sv.LastPoll.IsZero() {
			fmt.Fprintf(c.out, "%s %-28s %-16s %-3s %-12s %-10s %s\n", mark, name, "-", "-", "-", "-", "not queried yet")
			continue
		}
		ago := fmtDuration(now.Sub(sv.LastPoll)) + " ago"
		if !sv.Reach {
			fmt.Fprintf(c.out, "%s %-28s %-16s %-3s %-12s %-10s %s (%s)\n", mark, name, "-", "-", "-", "-", ago, sv.Err)
			continue
		}
		fmt.Fprintf(c.out, "%s %-28s %-16s %-3d %-12s %-10s %s\n", mark, name, sv.Addr, sv.Stratum,
			sv.Offset.Round(time.Microsecond), sv.Delay.Round(time.Microsecond), ago)
	}
	c.out.WriteString("\n* = the server the clock follows. Offset: server time minus this switch's time.\n")
	return nil
}

// fmtDuration renders "3d 04:05" / "04:05:06".
func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	h, m, s := int(d.Hours())%24, int(d.Minutes())%60, int(d.Seconds())%60
	if days > 0 {
		return fmt.Sprintf("%dd %02d:%02d", days, h, m)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// power implements "request system reboot|halt|power-off [in <minutes>]".
func (sh *Shell) power(c *call, action string) error {
	minutes := 0
	switch {
	case len(c.args) == 0:
	case len(c.args) == 2 && prefixOf(c.args[0].Text, "in"):
		n, err := strconv.Atoi(c.args[1].Text)
		if err != nil || n < 0 || n > 24*60 {
			return &posError{pos: c.argPos(1), msg: "expecting minutes (0-1440)"}
		}
		minutes = n
	default:
		return &posError{pos: c.argPos(0), msg: "syntax error, expecting 'in <minutes>'"}
	}
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	what := map[string]string{"reboot": "Reboot", "halt": "Halt", "power-off": "Power off"}[action]
	a, err := c.term.Ask(what+" the system ? [yes,no] (no) ", true)
	if err != nil || !isYes(a) {
		return nil
	}
	if err := sh.env.Ops.Power(action, minutes, sh.env.User); err != nil {
		return err
	}
	if minutes > 0 {
		fmt.Fprintf(c.out, "%s scheduled in %d minutes ('clear system reboot' cancels it)\n", what, minutes)
	} else {
		fmt.Fprintf(c.out, "%s requested\n", what)
	}
	return nil
}

func (sh *Shell) cancelPower(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	if err := sh.env.Ops.CancelPower(sh.env.User); err != nil {
		return err
	}
	c.out.WriteString("scheduled reboot/halt/power-off cancelled\n")
	return nil
}

func (sh *Shell) showVLANs(c *call) error {
	extensive := false
	switch {
	case len(c.args) == 0:
	case len(c.args) == 1 && prefixOf(c.args[0].Text, "extensive"):
		extensive = true
	default:
		return &posError{pos: c.argPos(0), msg: "syntax error, expecting 'extensive' or nothing"}
	}
	var drops map[int]uint64
	if extensive && sh.env.Ops != nil {
		drops, _ = sh.env.Ops.VLANMTUDrops()
	}
	cfg := sh.activeModel()
	var vs []*model.VLAN
	for _, v := range cfg.VLANs {
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].ID < vs[j].ID })
	fmt.Fprintf(c.out, "%-14s %-5s %-6s %-9s %s\n", "Name", "Tag", "MTU", "VNI", "Interfaces")
	for _, v := range vs {
		var ports []string
		for _, i := range cfg.Interfaces {
			if !i.Switching {
				continue
			}
			for _, id := range i.VLANs {
				if id != v.ID {
					continue
				}
				p := i.Name
				if !c.shows(p) {
					continue
				}
				if i.Mode == "trunk" && i.NativeVLAN != id {
					p += "*" // tagged
				}
				ports = append(ports, p)
			}
		}
		sort.Slice(ports, func(a, b int) bool { return config.NaturalLess(ports[a], ports[b]) })
		mtu, vni := "-", "-"
		if v.MTU != 0 {
			mtu = strconv.Itoa(v.MTU)
		}
		if v.VNI != 0 {
			vni = strconv.Itoa(v.VNI)
		}
		fmt.Fprintf(c.out, "%-14s %-5d %-6s %-9s %s\n", v.Name, v.ID, mtu, vni, strings.Join(ports, ", "))
		if extensive && v.MTU != 0 {
			fmt.Fprintf(c.out, "  mtu-exceeded drops: %d\n", drops[v.ID])
		}
	}
	c.out.WriteString("* = tagged\n")
	return nil
}

// registerOperational adds the show/clear commands for live data. It is
// called at the end of the command table's init (explicit order).
func registerOperational() {
	for _, cmd := range operational {
		switch cmd.name {
		case "show":
			cmd.sub = append(cmd.sub,
				&command{name: "interfaces", help: "Show interface status and counters", class: commit.ReadOnly,
					run: (*Shell).showInterfaces, complete: completeIfArgs},
				&command{name: "ethernet-switching", help: "Show switching information", class: commit.ReadOnly, sub: []*command{
					{name: "table", help: "Show the MAC address table", class: commit.ReadOnly, run: (*Shell).showMACTable, complete: completeMACArgs},
				}},
				&command{name: "vlans", help: "Show VLANs and their interfaces", class: commit.ReadOnly, run: (*Shell).showVLANs,
					complete: words(Completion{Text: "extensive", Help: "With the frames dropped for exceeding the VLAN mtu"})},
				&command{name: "virtual-chassis", help: "Show the virtual chassis (stack)", class: commit.ReadOnly, run: (*Shell).showVC, sub: []*command{
					{name: "vc-port", help: "Show the stacking ports and their neighbours", class: commit.ReadOnly, run: (*Shell).showVCPorts},
					{name: "mtu", help: "Show the frame sizes the stack tunnels carry", class: commit.ReadOnly, run: (*Shell).showStackMTU},
				}},
				&command{name: "route", help: "Show a routing table", class: commit.ReadOnly, run: (*Shell).showRoute, complete: completeRoute},
				&command{name: "arp", help: "Show the IPv4 neighbour (ARP) table", class: commit.ReadOnly, run: (*Shell).showARP,
					complete: words(Completion{Text: "no-resolve", Help: "Do not resolve host names"})},
				&command{name: "ipv6", help: "Show IPv6 information", class: commit.ReadOnly, sub: []*command{
					{name: "neighbors", help: "Show the IPv6 neighbour table", class: commit.ReadOnly, run: (*Shell).showNDP},
				}},
				&command{name: "chassis", help: "Show chassis information", class: commit.ReadOnly, sub: []*command{
					{name: "hardware", help: "Show the physical ports and their NICs", class: commit.ReadOnly, run: (*Shell).showHardware},
				}},
				&command{name: "log", help: "Show recent log messages", class: commit.ReadOnly, run: (*Shell).showLog},
				&command{name: "mclag", help: "Show the MC-LAG domain of this member", class: commit.ReadOnly, run: (*Shell).showMCLAG,
					complete: words(Completion{Text: "consistency", Help: "Compare the MC-LAG bundles with the peer"})},
				&command{name: "lacp", help: "Show LACP information", class: commit.ReadOnly, sub: []*command{
					{name: "interfaces", help: "Show LACP state per bundle and port", class: commit.ReadOnly, run: (*Shell).showLACP, complete: completeAE},
					{name: "statistics", help: "Show LACP statistics", class: commit.ReadOnly, sub: []*command{
						{name: "interfaces", help: "Show LACPDU counters per port", class: commit.ReadOnly, run: (*Shell).showLACPStats, complete: completeAE},
					}},
				}},
				stpCommand(),
				lldpCommand(),
				&command{name: "dhcp", help: "Show DHCP information", class: commit.ReadOnly, sub: []*command{
					{name: "client", help: "DHCP client", class: commit.ReadOnly, sub: []*command{
						{name: "binding", help: "Leases of the interfaces with 'family inet dhcp'", class: commit.ReadOnly, run: (*Shell).showDHCPBinding},
					}},
				}},
			)
			for _, sc := range cmd.sub {
				if sc.name == "system" {
					sc.sub = append(sc.sub, &command{name: "syslog", help: "Show remote syslog servers", class: commit.ReadOnly, run: (*Shell).showSyslog},
						&command{name: "uptime", help: "Show the time, boot time and last configuration change", class: commit.ReadOnly, run: (*Shell).showUptime},
						&command{name: "ntp", help: "Show the NTP servers and the clock", class: commit.ReadOnly, run: (*Shell).showNTP},
						&command{name: "offload", help: "Show hardware capabilities and acceleration per port", class: commit.ReadOnly, run: (*Shell).showOffload},
						&command{name: "limits", help: "Show what the switch can carry and how much is used", class: commit.ReadOnly, run: (*Shell).showLimits})
				}
			}
			sort.Slice(cmd.sub, func(i, j int) bool { return cmd.sub[i].name < cmd.sub[j].name })
		}
	}
	operational = append(operational, &command{name: "clear", help: "Clear information", class: commit.Operator, sub: []*command{
		{name: "ethernet-switching", help: "Clear switching information", class: commit.Operator, sub: []*command{
			{name: "table", help: "Remove learned MAC addresses", class: commit.Operator, run: (*Shell).clearMACTable, complete: completeMACArgs},
		}},
		{name: "system", help: "Clear system state", class: commit.SuperUser, sub: []*command{
			{name: "reboot", help: "Cancel a scheduled reboot, halt or power-off", class: commit.SuperUser, run: (*Shell).cancelPower},
		}},
	}})
	power := func(action, help string) *command {
		return &command{name: action, help: help, class: commit.SuperUser, run: func(sh *Shell, c *call) error { return sh.power(c, action) },
			complete: words(Completion{Text: "in", Help: "Delay in minutes"})}
	}
	vcPort := func(add bool) *command {
		name, help := "set", "Make a port a stacking (VC) port"
		if !add {
			name, help = "delete", "Release a stacking (VC) port"
		}
		return &command{name: name, help: help, class: commit.SuperUser,
			run:      func(sh *Shell, c *call) error { return sh.setVCPort(c, add) },
			complete: completeVCPort}
	}
	operational = append(operational, &command{name: "request", help: "Make system-level requests", class: commit.SuperUser, sub: []*command{
		{name: "system", help: "System requests", class: commit.SuperUser, sub: []*command{
			power("reboot", "Reboot this member"),
			power("halt", "Halt this member"),
			power("power-off", "Power off this member"),
			{name: "maintenance-mode", help: "Take this member out of service without losing traffic", class: commit.SuperUser, sub: []*command{
				{name: "enter", help: "Drain this member (mastership, stack transit, MC-LAG legs)", class: commit.SuperUser,
					run:      func(sh *Shell, c *call) error { return sh.maintenance(c, true) },
					complete: words(Completion{Text: "force", Help: "Enter even if traffic would be cut"})},
				{name: "exit", help: "Return this member to service", class: commit.SuperUser,
					run: func(sh *Shell, c *call) error { return sh.maintenance(c, false) }},
			}},
		}},
		{name: "chassis", help: "Chassis requests", class: commit.SuperUser, sub: []*command{
			{name: "card", help: "Card numbering: '<card> renumber <card>' or '<card> forget'", class: commit.SuperUser, run: (*Shell).chassisCard,
				complete: words(Completion{Text: "<card>", Help: "Card number", Placeholder: true})},
			{name: "routing-engine", help: "Routing engine (stack master) requests", class: commit.SuperUser, sub: []*command{
				{name: "master", help: "Stack mastership", class: commit.SuperUser, sub: []*command{
					{name: "switch", help: "Hand mastership to another member", class: commit.SuperUser, run: (*Shell).switchMaster,
						complete: words(Completion{Text: "member", Help: "Member to become master (default: the next by priority)"})},
				}},
			}},
		}},
		{name: "virtual-chassis", help: "Virtual chassis (stack) requests", class: commit.SuperUser, sub: []*command{
			{name: "vc-port", help: "Stacking ports of this switch", class: commit.SuperUser, sub: []*command{vcPort(true), vcPort(false)}},
			{name: "member", help: "Stack members", class: commit.SuperUser, sub: []*command{
				{name: "add", help: "Allow a switch to join as this member (prints a one-time token)", class: commit.SuperUser,
					run: (*Shell).addVCMember, complete: words(Completion{Text: "<member-id>", Help: "Member id 1-16", Placeholder: true})},
				{name: "remove", help: "Remove a member from the stack (decommission)", class: commit.SuperUser,
					run: (*Shell).removeVCMember, complete: words(Completion{Text: "<member-id>", Help: "Member id 1-16", Placeholder: true})},
			}},
			{name: "force-master", help: "Continue alone when the stack has no majority (split-brain risk)", class: commit.SuperUser, run: (*Shell).forceMaster},
			{name: "join", help: "Join a virtual chassis over the VC ports", class: commit.SuperUser, run: (*Shell).joinVC,
				complete: words(Completion{Text: "token", Help: "Token from 'request virtual-chassis member add' on the stack"})},
		}},
	}})
	sort.Slice(operational, func(i, j int) bool { return operational[i].name < operational[j].name })
	markPerMember()
}

var (
	vlanRefType = schema.VlanRef
	ifRefType   = schema.Interface
)

func completeIfArgs(sh *Shell, args []config.Token, partial string) []Completion {
	out := []Completion{enter}
	if len(args) == 0 {
		out = append(out, filter([]Completion{{Text: "terse", Help: "One line per interface"}, {Text: "extensive", Help: "All counters"}}, partial)...)
	}
	if sh.env.Ops != nil {
		if ifs, err := sh.env.Ops.Interfaces(); err == nil {
			for _, i := range ifs {
				if strings.HasPrefix(i.Name, partial) {
					out = append(out, Completion{Text: i.Name, Help: i.Description})
				}
			}
		}
	}
	return out
}

func completeMACArgs(sh *Shell, args []config.Token, partial string) []Completion {
	if n := len(args); n > 0 {
		switch {
		case prefixOf(args[n-1].Text, "vlan"):
			return sh.valueCompletions(vlanRefType, sh.env.Engine.Active(), partial)
		case prefixOf(args[n-1].Text, "interface"):
			return sh.valueCompletions(ifRefType, sh.env.Engine.Active(), partial)
		}
	}
	return append([]Completion{enter}, filter([]Completion{
		{Text: "vlan", Help: "Only this VLAN"}, {Text: "interface", Help: "Only this interface"}}, partial)...)
}

// showLimits is "show system limits" (reference 3.5.1).
func (sh *Shell) showLimits(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if sh.env.Ops == nil {
		return errors.New("limits are not available")
	}
	cfg, _ := model.Build(sh.env.Engine.Active(), nil)
	if cfg == nil {
		return errors.New("no valid configuration")
	}
	hw, err := sh.env.Ops.Limits()
	if err != nil {
		return err
	}
	vc, _ := sh.env.Ops.VirtualChassis()
	stack, _ := sh.env.Ops.StackMTU()

	use := func(n, max int) string {
		s := fmt.Sprintf("%d of %d", n, max)
		if n >= max {
			s += " (full)"
		}
		return s
	}
	line := func(name, value string) { fmt.Fprintf(c.out, "  %-32s %s\n", name+":", value) }
	head := func(title string) { fmt.Fprintf(c.out, "\n%s\n", title) }

	fmt.Fprintf(c.out, "Limits of member %d (frame sizes include the Ethernet header, no VLAN tags)\n", hw.Member)

	head("Frame sizes")
	line("Configurable mtu", fmt.Sprintf("%d..%d (default %d)", schema.MinMTU, schema.MaxMTU, schema.DefaultMTU))
	mtu, where := cfg.MaxDataMTU()
	if where == "virtual-chassis" { // nothing sets one
		where = "default"
	}
	line("Largest mtu configured", fmt.Sprintf("%d (%s; hosts up to MTU %d)", mtu, orDash(where), mtu-model.EthHeader))
	if len(cfg.SwitchMembers()) > 1 {
		line("Added by the stack tunnels", fmt.Sprintf("%d bytes (tunnel 50, VLAN tags 8)", model.StackOverhead))
		limit := 0
		for _, p := range stack.Ports {
			if p.MaxMTU > 0 && (limit == 0 || p.MaxMTU < limit) {
				limit = p.MaxMTU
			}
		}
		if limit > 0 {
			carry := min(limit-model.StackOverhead, schema.MaxMTU)
			line("Largest mtu the stack carries", fmt.Sprintf("%d (hosts up to MTU %d; 'show virtual-chassis mtu')", carry, carry-model.EthHeader))
		}
	} else {
		line("Stack tunnels", "none (a single switch)")
	}
	if hw.LowestMaxMTU > 0 {
		line("Hardware maximum of the ports", fmt.Sprintf("%d (%s) .. %d (%s)", hw.LowestMaxMTU, hw.LowestMaxPort, hw.HighestMaxMTU, hw.HighestMaxPort))
	} else {
		line("Hardware maximum of the ports", "not reported by the drivers")
	}

	head("Switching")
	vnis := 0
	for _, v := range cfg.VLANs {
		if v.VNI != 0 {
			vnis++
		}
	}
	line("VLAN ids", fmt.Sprintf("%d..%d; %s", schema.MinVLANID, schema.MaxVLANID,
		use(len(cfg.VLANs), schema.MaxVLANID-schema.MinVLANID+1)))
	line("VXLAN VNIs", fmt.Sprintf("1..%d; %d in use", schema.MaxVNI, vnis))
	line("MAC addresses learned now", strconv.Itoa(hw.MACEntries))
	line("MAC aging time", fmt.Sprintf("%d..%d seconds (default %d)", schema.MinMACAging, schema.MaxMACAging, schema.DefaultMACAging))
	line("mac-limit per interface", fmt.Sprintf("%d..%d", schema.MinMACLimit, schema.MaxMACLimit))

	head("Aggregation")
	bundles, largest := 0, 0
	for _, i := range cfg.Interfaces {
		if i.AE {
			bundles++
			largest = max(largest, len(i.MemberPorts))
		}
	}
	line("Bundles (ae0..ae"+strconv.Itoa(schema.MaxAE)+")", use(bundles, schema.MaxAE+1))
	line("Largest bundle", fmt.Sprintf("%d ports", largest))

	head("MC-LAG")
	mc := 0
	for _, i := range cfg.Interfaces {
		if i.AE && i.MCLAG {
			mc++
		}
	}
	line("Domains", use(len(cfg.Domains), schema.MaxDomain))
	line("Members per domain", strconv.Itoa(schema.MembersPerDomain))
	line("Domains per member", strconv.Itoa(schema.DomainsPerMember))
	line("MC-LAG bundles", strconv.Itoa(mc))

	head("Stack")
	line("Members", use(len(cfg.Members), schema.MaxMember))
	if vc.Control {
		line("Voters", fmt.Sprintf("%d (at most %d)", len(vc.Voters), schema.MaxVoters))
	}
	up := 0
	for _, p := range vc.Ports {
		if p.State == "up" {
			up++
		}
	}
	line("Stacking links of this member", fmt.Sprintf("%d, %d up", len(vc.Ports), up))

	head("Ports")
	line("Physical ports", strconv.Itoa(hw.Ports))
	line("Stacking ports", strconv.Itoa(hw.StackPorts))
	if hw.FastestMbps > 0 {
		line("Fastest port", fmt.Sprintf("%s (%s)", fmtSpeed(hw.FastestMbps), hw.FastestPort))
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fmtSpeed(mbps int) string {
	if mbps >= 1000 && mbps%1000 == 0 {
		return fmt.Sprintf("%dG", mbps/1000)
	}
	return fmt.Sprintf("%dM", mbps)
}
