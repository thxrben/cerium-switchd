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
	// SwitchMaster hands mastership to member to (0: the best other member).
	SwitchMaster(to int, user string) error
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
}

// VCPort is one VC port.
type VCPort struct {
	Port, Linux, State, Neighbor, PeerPort, LastError string
	UpSince                                           time.Time
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
	if err != nil {
		return err
	}
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
	if err != nil {
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
		if (vlan != 0 && e.VLAN != vlan) || (iface != "" && e.Interface != iface) {
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
	if err != nil {
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
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%-10s %-16s %-14s %-12s %s\n", "Interface", "Linux name", "Bus address", "Driver", "MAC address")
	for _, p := range ports {
		fmt.Fprintf(c.out, "%-10s %-16s %-14s %-12s %s\n", p.Name, p.Linux, p.Bus, p.Driver, p.MAC)
	}
	return nil
}

// setVCPort implements "request virtual-chassis vc-port set|delete
// pic-slot <card> port <port>".
func (sh *Shell) setVCPort(c *call, add bool) error {
	if len(c.args) != 4 || !prefixOf(c.args[0].Text, "pic-slot") || !prefixOf(c.args[2].Text, "port") {
		return &posError{pos: c.argPos(0), msg: "syntax error, expecting 'pic-slot <card> port <port>'"}
	}
	card, err1 := strconv.Atoi(c.args[1].Text)
	port, err2 := strconv.Atoi(c.args[3].Text)
	if err1 != nil || card < 0 || card > schema.MaxCard {
		return &posError{pos: c.argPos(1), msg: fmt.Sprintf("expecting a card number (0-%d)", schema.MaxCard)}
	}
	if err2 != nil || port < 0 || port > schema.MaxPort {
		return &posError{pos: c.argPos(3), msg: fmt.Sprintf("expecting a port number (0-%d)", schema.MaxPort)}
	}
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	local := fmt.Sprintf("%d/%d", card, port)
	if add {
		// A configured data port would be taken away from the data plane.
		st, err := sh.vcStatus()
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%d/%s", st.Member, local)
		if cfg := sh.activeModel(); cfg != nil && cfg.Interfaces[name] != nil {
			return fmt.Errorf("%s is configured under 'interfaces'; delete that configuration first", name)
		}
	}
	return sh.env.Ops.SetVCPort(local, add, sh.env.User)
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

func completeVCPort(_ *Shell, args []config.Token, partial string) []Completion {
	switch len(args) {
	case 0:
		return filter([]Completion{{Text: "pic-slot", Help: "Card number of the port"}}, partial)
	case 1:
		return []Completion{{Text: "<card>", Help: "Card number (see show chassis hardware)", Placeholder: true}}
	case 2:
		return filter([]Completion{{Text: "port", Help: "Port number on the card"}}, partial)
	case 3:
		return []Completion{{Text: "<port>", Help: "Port number", Placeholder: true}}
	}
	return []Completion{enter}
}

func (sh *Shell) vcStatus() (VCStatus, error) {
	if sh.env.Ops == nil {
		return VCStatus{}, errors.New("virtual chassis information is not available")
	}
	return sh.env.Ops.VirtualChassis()
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
	st, err := sh.vcStatus()
	if err != nil {
		return err
	}
	if len(st.Ports) == 0 {
		c.out.WriteString("No VC ports ('request virtual-chassis vc-port set pic-slot <card> port <port>').\n")
		return nil
	}
	fmt.Fprintf(c.out, "%-8s %-12s %-7s %-24s %-10s %s\n", "Port", "Linux name", "State", "Neighbor", "Peer port", "Up")
	now := time.Now()
	for _, p := range st.Ports {
		up := "-"
		if !p.UpSince.IsZero() {
			up = fmtDuration(now.Sub(p.UpSince))
		}
		fmt.Fprintf(c.out, "%-8s %-12s %-7s %-24s %-10s %s\n", p.Port, p.Linux, p.State, p.Neighbor, dash(p.PeerPort), up)
		if p.LastError != "" && p.State != "up" {
			fmt.Fprintf(c.out, "         last error: %s\n", p.LastError)
		}
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
	if err != nil {
		return err
	}
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
	if err != nil {
		return err
	}
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
	if err := noArgs(c); err != nil {
		return err
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
				&command{name: "vlans", help: "Show VLANs and their interfaces", class: commit.ReadOnly, run: (*Shell).showVLANs},
				&command{name: "virtual-chassis", help: "Show the virtual chassis (stack)", class: commit.ReadOnly, run: (*Shell).showVC, sub: []*command{
					{name: "vc-port", help: "Show the stacking ports and their neighbours", class: commit.ReadOnly, run: (*Shell).showVCPorts},
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
			)
			for _, sc := range cmd.sub {
				if sc.name == "system" {
					sc.sub = append(sc.sub, &command{name: "syslog", help: "Show remote syslog servers", class: commit.ReadOnly, run: (*Shell).showSyslog},
						&command{name: "uptime", help: "Show the time, boot time and last configuration change", class: commit.ReadOnly, run: (*Shell).showUptime},
						&command{name: "offload", help: "Show hardware capabilities and acceleration per port", class: commit.ReadOnly, run: (*Shell).showOffload})
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
		}},
		{name: "chassis", help: "Chassis requests", class: commit.SuperUser, sub: []*command{
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
			{name: "join", help: "Join a virtual chassis over the VC ports", class: commit.SuperUser, run: (*Shell).joinVC,
				complete: words(Completion{Text: "token", Help: "Token from 'request virtual-chassis member add' on the stack"})},
		}},
	}})
	sort.Slice(operational, func(i, j int) bool { return operational[i].name < operational[j].name })
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
