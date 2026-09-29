package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

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
}

// IfStatus is one interface as shown by "show interfaces".
type IfStatus struct {
	Name        string // configuration name (1/ens19, ae1)
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
			)
			sort.Slice(cmd.sub, func(i, j int) bool { return cmd.sub[i].name < cmd.sub[j].name })
		}
	}
	operational = append(operational, &command{name: "clear", help: "Clear information", class: commit.Operator, sub: []*command{
		{name: "ethernet-switching", help: "Clear switching information", class: commit.Operator, sub: []*command{
			{name: "table", help: "Remove learned MAC addresses", class: commit.Operator, run: (*Shell).clearMACTable, complete: completeMACArgs},
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
