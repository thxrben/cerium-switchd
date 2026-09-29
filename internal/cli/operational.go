package cli

import (
	"errors"
	"fmt"
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
						&command{name: "uptime", help: "Show the time, boot time and last configuration change", class: commit.ReadOnly, run: (*Shell).showUptime})
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
	operational = append(operational, &command{name: "request", help: "Make system-level requests", class: commit.SuperUser, sub: []*command{
		{name: "system", help: "System requests", class: commit.SuperUser, sub: []*command{
			power("reboot", "Reboot this member"),
			power("halt", "Halt this member"),
			power("power-off", "Power off this member"),
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
