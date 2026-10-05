package cli

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/lib/conf/config"
)

// McastStatus is what the snooping show commands need from one member
// (reference 5.5).
type McastStatus struct {
	Member  int
	Groups  []McastGroup
	Routers []McastRouter
	VLANs   []McastVLAN
}

// McastGroup is one membership: a group with receivers behind an interface.
type McastGroup struct {
	VLAN      int
	Group     string
	Interface string // configuration name (vc-<n>: learned over the stack)
	Static    bool   // installed (e.g. for the MC-LAG peer), not learned here
	Mode      string
	Sources   []string
	Expires   float64
}

// McastRouter is a multicast-router port of a VLAN.
type McastRouter struct {
	VLAN      int
	Interface string
	Permanent bool
	Expires   float64
}

// McastVLAN is the snooping state of one VLAN.
type McastVLAN struct {
	VLAN              int
	Snooping, Querier bool
}

func (sh *Shell) showSnooping(c *call, ipv6 bool, what string) error {
	if sh.env.Ops == nil {
		return errors.New("multicast information is not available")
	}
	vlan := 0
	switch {
	case len(c.args) == 0:
	case len(c.args) == 2 && prefixOf(c.args[0].Text, "vlan") && what == "membership":
		cfg := sh.activeModel()
		if v, ok := cfg.VLANs[c.args[1].Text]; ok {
			vlan = v.ID
		} else if id, err := strconv.Atoi(c.args[1].Text); err == nil && id > 0 && id < 4095 {
			vlan = id
		} else {
			return &posError{pos: c.args[1].Pos, msg: "unknown VLAN"}
		}
	default:
		return &posError{pos: c.argPos(0), msg: "syntax error"}
	}
	sts, err := sh.env.Ops.Multicast()
	if err := partial(c, err); err != nil {
		return err
	}
	cfg := sh.activeModel()
	vname := func(id int) string {
		if v := cfg.VLANByID[id]; v != nil {
			return v.Name
		}
		return strconv.Itoa(id)
	}
	family := func(g string) bool { return strings.Contains(g, ":") == ipv6 }
	if what == "vlans" {
		var vids []int
		snoop, querier := map[int]bool{}, map[int]bool{}
		routers, learned := map[int][]string{}, map[int][]string{}
		for _, st := range sts {
			for _, v := range st.VLANs {
				if !slices.Contains(vids, v.VLAN) {
					vids = append(vids, v.VLAN)
				}
				snoop[v.VLAN] = snoop[v.VLAN] || v.Snooping
				querier[v.VLAN] = querier[v.VLAN] || v.Querier
			}
			for _, r := range st.Routers {
				if strings.HasPrefix(r.Interface, "vc-") || !c.shows(r.Interface) {
					continue // the stack tunnels are always router ports
				}
				if !slices.Contains(routers[r.VLAN], r.Interface) {
					routers[r.VLAN] = append(routers[r.VLAN], r.Interface)
				}
				if !r.Permanent && !slices.Contains(learned[r.VLAN], r.Interface) {
					learned[r.VLAN] = append(learned[r.VLAN], r.Interface)
				}
			}
		}
		slices.Sort(vids)
		fmt.Fprintf(c.out, "%-14s %-5s %-9s %-34s %s\n", "VLAN", "Tag", "Snooping", "Querier", "Router ports")
		for _, id := range vids {
			on, q := "on", "none (group traffic is flooded)"
			if !snoop[id] {
				on, q = "off", "-"
			}
			switch {
			case !snoop[id]:
			case querier[id]:
				q = "this switch"
			case len(learned[id]) > 0:
				q = "seen on " + strings.Join(learned[id], ", ")
			}
			rp := routers[id]
			slices.SortFunc(rp, func(a, b string) int { return strings.Compare(a, b) })
			fmt.Fprintf(c.out, "%-14s %-5d %-9s %-34s %s\n", vname(id), id, on, q, orDash(strings.Join(rp, ", ")))
		}
		c.out.WriteString("The stack tunnels are router ports in every VLAN (group traffic reaches every member).\n")
		return nil
	}
	var gs []McastGroup
	for _, st := range sts {
		for _, g := range st.Groups {
			// Listed where they were learned: not the copies learned over
			// the stack tunnels.
			if family(g.Group) && !strings.HasPrefix(g.Interface, "vc-") && (vlan == 0 || g.VLAN == vlan) && c.shows(g.Interface) {
				gs = append(gs, g)
			}
		}
	}
	slices.SortFunc(gs, func(a, b McastGroup) int {
		if a.VLAN != b.VLAN {
			return a.VLAN - b.VLAN
		}
		if a.Group != b.Group {
			return strings.Compare(a.Group, b.Group)
		}
		if config.NaturalLess(a.Interface, b.Interface) {
			return -1
		}
		return 1
	})
	fmt.Fprintf(c.out, "%-14s %-5s %-26s %-12s %-8s %s\n", "VLAN", "Tag", "Group", "Interface", "Expires", "Sources")
	for _, g := range gs {
		exp := fmt.Sprintf("%.0fs", g.Expires)
		if g.Static {
			exp = "static"
		}
		src := "-"
		if len(g.Sources) > 0 {
			src = g.Mode + " " + strings.Join(g.Sources, ", ")
		}
		fmt.Fprintf(c.out, "%-14s %-5d %-26s %-12s %-8s %s\n", vname(g.VLAN), g.VLAN, g.Group, g.Interface, exp, src)
	}
	fmt.Fprintf(c.out, "%d memberships\n", len(gs))
	return nil
}

func snoopingCommand(name, help string, ipv6 bool) *command {
	run := func(what string) func(*Shell, *call) error {
		return func(sh *Shell, c *call) error { return sh.showSnooping(c, ipv6, what) }
	}
	return &command{name: name, help: help, class: commit.ReadOnly, sub: []*command{
		{name: "snooping", help: help + " snooping", class: commit.ReadOnly, sub: []*command{
			{name: "membership", help: "Groups and the interfaces with receivers", class: commit.ReadOnly, run: run("membership"),
				complete: words(Completion{Text: "vlan", Help: "Only this VLAN"})},
			{name: "vlans", help: "Snooping, querier and router ports per VLAN", class: commit.ReadOnly, run: run("vlans")},
		}},
	}}
}

// VXLANStatus is one member's VXLAN state (reference 5.7).
type VXLANStatus struct {
	Member int
	Source string
	Ports  []VXLANPort
	Routes []VTEPRoute
}

// VXLANPort is one VNI's VXLAN port on a member.
type VXLANPort struct {
	VNI, VLAN  int
	Port       string
	Up         bool
	Remotes    []string
	RemoteMACs int
	RxPackets  uint64
	TxPackets  uint64
}

// VTEPRoute is how a member reaches a remote VTEP.
type VTEPRoute struct {
	VTEP, Via, Interface string // Via "": directly connected
	NoRoute              bool
}

func (sh *Shell) showVXLAN(c *call, remotes bool) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if sh.env.Ops == nil {
		return errors.New("VXLAN information is not available")
	}
	sts, err := sh.env.Ops.VXLAN()
	if err := partial(c, err); err != nil {
		return err
	}
	cfg := sh.activeModel()
	if cfg.Switch.VTEPSource == "" {
		c.out.WriteString("VXLAN is not configured (switch-options vxlan source-address).\n")
		return nil
	}
	slices.SortFunc(sts, func(a, b VXLANStatus) int { return a.Member - b.Member })
	if remotes {
		fmt.Fprintf(c.out, "%-16s %-7s %s\n", "Remote VTEP", "Member", "Reached via")
		for _, st := range sts {
			if c.only != nil && !slices.Contains(c.only, st.Member) {
				continue
			}
			for _, r := range st.Routes {
				via := "no route (cannot send to it)"
				switch {
				case r.NoRoute:
				case r.Via == "":
					via = "directly connected, " + r.Interface
				default:
					via = r.Via + ", " + r.Interface
				}
				fmt.Fprintf(c.out, "%-16s %-7d %s\n", r.VTEP, st.Member, via)
			}
		}
		return nil
	}
	fmt.Fprintf(c.out, "Stack VTEP %s, UDP port %d\n\n", cfg.Switch.VTEPSource, cfg.Switch.VXLANPort)
	fmt.Fprintf(c.out, "%-9s %-14s %-12s %-7s %-6s %-11s %-11s %s\n", "VNI", "VLAN", "Port", "Member", "Link", "Rx packets", "Tx packets", "Remote VTEPs (remote MACs)")
	for _, st := range sts {
		if c.only != nil && !slices.Contains(c.only, st.Member) {
			continue
		}
		for _, p := range st.Ports {
			vn := strconv.Itoa(p.VLAN)
			if v := cfg.VLANByID[p.VLAN]; v != nil {
				vn = v.Name
			}
			fmt.Fprintf(c.out, "%-9d %-14s %-12s %-7d %-6s %-11d %-11d %s (%d)\n", p.VNI, vn, p.Port, st.Member, upDown(p.Up),
				p.RxPackets, p.TxPackets, orDash(strings.Join(p.Remotes, ", ")), p.RemoteMACs)
		}
	}
	return nil
}
