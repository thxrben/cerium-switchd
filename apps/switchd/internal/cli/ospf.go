package cli

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/lib/ospf"
)

// The show ospf|ospf3 … and clear ospf|ospf3 neighbor commands (reference
// 5.13).

// OSPFInstance is the state of OSPF or OSPFv3 in one routing instance.
type OSPFInstance struct {
	Instance string       `json:"instance"`
	Version  ospf.Version `json:"version"`
	ospf.Status
}

// OSPF is implemented by members that run cer-ospfd.
type OSPF interface {
	OSPFStatus(v ospf.Version, instance *string, detail bool) ([]OSPFInstance, error)
	ClearOSPF(v ospf.Version, instance string, nbr netip.Addr) (int, error)
}

func (sh *Shell) ospf() (OSPF, error) {
	o, ok := sh.env.Ops.(OSPF)
	if sh.env.Ops == nil || !ok {
		return nil, errors.New("OSPF information is not available")
	}
	return o, nil
}

func ospfCommand(v ospf.Version) *command {
	name, title := "ospf", "OSPF"
	if v == ospf.V3 {
		name, title = "ospf3", "OSPFv3"
	}
	run := func(f func(sh *Shell, c *call, v ospf.Version, a ospfArgs) error) func(*Shell, *call) error {
		return func(sh *Shell, c *call) error {
			a, err := parseOSPFArgs(c)
			if err != nil {
				return err
			}
			return f(sh, c, v, a)
		}
	}
	opts := words(Completion{Text: "instance", Help: "Routing instance"}, Completion{Text: "detail", Help: "More details"})
	return &command{name: name, help: "Show " + title + " information", class: commit.ReadOnly, sub: []*command{
		{name: "neighbor", help: "Show the " + title + " neighbours", class: commit.ReadOnly, run: run((*Shell).showOSPFNeighbors), complete: opts},
		{name: "interface", help: "Show the " + title + " interfaces", class: commit.ReadOnly, run: run((*Shell).showOSPFInterfaces), complete: opts},
		{name: "database", help: "Show the link-state database", class: commit.ReadOnly, run: run((*Shell).showOSPFDatabase),
			complete: words(Completion{Text: "router", Help: "Router LSAs"}, Completion{Text: "network", Help: "Network LSAs"},
				Completion{Text: "summary", Help: "Summary / inter-area prefix LSAs"}, Completion{Text: "asbr-summary", Help: "ASBR summary / inter-area router LSAs"},
				Completion{Text: "external", Help: "AS-external LSAs"}, Completion{Text: "link", Help: "Link LSAs (OSPFv3)"},
				Completion{Text: "intra-area-prefix", Help: "Intra-area prefix LSAs (OSPFv3)"},
				Completion{Text: "lsa-id", Help: "One LS id"}, Completion{Text: "advertising-router", Help: "One router's LSAs"},
				Completion{Text: "detail", Help: "LSA contents"}, Completion{Text: "extensive", Help: "LSA contents"},
				Completion{Text: "instance", Help: "Routing instance"})},
		{name: "route", help: "Show the routes " + title + " computed", class: commit.ReadOnly, run: run((*Shell).showOSPFRoutes), complete: opts},
		{name: "overview", help: "Show the " + title + " router", class: commit.ReadOnly, run: run((*Shell).showOSPFOverview), complete: opts},
		{name: "statistics", help: "Show packet counters", class: commit.ReadOnly, run: run((*Shell).showOSPFStats), complete: opts},
	}}
}

func clearOSPFCommand(v ospf.Version) *command {
	name, title := "ospf", "OSPF"
	if v == ospf.V3 {
		name, title = "ospf3", "OSPFv3"
	}
	return &command{name: name, help: "Clear " + title + " state", class: commit.Operator, sub: []*command{
		{name: "neighbor", help: "Restart " + title + " adjacencies", class: commit.Operator,
			run: func(sh *Shell, c *call) error {
				a, err := parseOSPFArgs(c)
				if err != nil {
					return err
				}
				o, err := sh.ospf()
				if err != nil {
					return err
				}
				var nbr netip.Addr
				if a.target != "" {
					if nbr, err = netip.ParseAddr(a.target); err != nil {
						return &posError{pos: c.argPos(0), msg: "expecting a neighbour's router id or address"}
					}
				}
				inst := ""
				if a.instance != nil {
					inst = *a.instance
				}
				n, err := o.ClearOSPF(v, inst, nbr)
				if err != nil {
					return err
				}
				fmt.Fprintf(c.out, "%d adjacencies restarted\n", n)
				return nil
			},
			complete: words(Completion{Text: "instance", Help: "Routing instance"})},
	}}
}

type ospfArgs struct {
	instance  *string
	detail    bool
	target    string // a neighbour address or an interface (unit)
	lsaType   string
	lsaID     string
	advRouter string
}

func parseOSPFArgs(c *call) (ospfArgs, error) {
	var a ospfArgs
	for i := 0; i < len(c.args); i++ {
		t := c.args[i].Text
		next := func(what string) (string, error) {
			if i+1 >= len(c.args) {
				return "", &posError{pos: c.argPos(i + 1), msg: "expecting " + what}
			}
			i++
			return c.args[i].Text, nil
		}
		var err error
		switch {
		case t == "instance":
			var s string
			s, err = next("a routing instance")
			a.instance = &s
		case t == "detail" || t == "extensive":
			a.detail = true
		case t == "lsa-id":
			a.lsaID, err = next("an LS id")
		case t == "advertising-router":
			a.advRouter, err = next("a router id")
		case slices.Contains([]string{"router", "network", "summary", "asbr-summary", "external", "link", "intra-area-prefix"}, t):
			a.lsaType = t
		case a.target == "":
			a.target = t
		default:
			return a, &posError{pos: c.argPos(i), msg: "syntax error"}
		}
		if err != nil {
			return a, err
		}
	}
	return a, nil
}

func (sh *Shell) ospfStatus(c *call, v ospf.Version, a ospfArgs) ([]OSPFInstance, error) {
	o, err := sh.ospf()
	if err != nil {
		return nil, err
	}
	st, err := o.OSPFStatus(v, a.instance, a.detail)
	if err != nil {
		return nil, err
	}
	if len(st) == 0 {
		name := "OSPF"
		if v == ospf.V3 {
			name = "OSPFv3"
		}
		fmt.Fprintf(c.out, "%s is not running here (it runs on the master, where it is configured).\n", name)
	}
	return st, nil
}

func instTitle(c *call, st []OSPFInstance, in OSPFInstance) {
	if len(st) > 1 || in.Instance != "" {
		name := in.Instance
		if name == "" {
			name = "default"
		}
		fmt.Fprintf(c.out, "Instance: %s\n", name)
	}
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd %s", d/(24*time.Hour), (d % (24 * time.Hour)).String())
	}
	return d.String()
}

func (sh *Shell) showOSPFNeighbors(c *call, v ospf.Version, a ospfArgs) error {
	st, err := sh.ospfStatus(c, v, a)
	if err != nil {
		return err
	}
	for _, in := range st {
		instTitle(c, st, in)
		fmt.Fprintf(c.out, "%-26s %-12s %-9s %-15s %-4s %s\n", "Address", "Interface", "State", "ID", "Pri", "Dead")
		for _, n := range in.Neighbors {
			if a.target != "" && n.Addr.String() != a.target && n.ID.String() != a.target {
				continue
			}
			state := n.State
			if n.HelperFor > 0 {
				state += " (helper)"
			}
			fmt.Fprintf(c.out, "%-26s %-12s %-9s %-15s %-4d %d\n", n.Addr, n.Iface, state, n.ID, n.Priority, int(n.DeadIn.Seconds()))
			if n.HelperFor > 0 && a.detail {
				fmt.Fprintf(c.out, "  Restarting: this router helps it for %s more (graceful restart)\n", fmtDur(n.HelperFor))
			}
			if a.detail {
				fmt.Fprintf(c.out, "  Area %s, DR %s, BDR %s, options 0x%x, up %s, %d state changes, %d to retransmit, %d requested\n",
					n.Area, n.DR, n.BDR, n.Options, fmtDur(n.Up), n.Events, n.Retrans, n.Requests)
			}
		}
	}
	return nil
}

func (sh *Shell) showOSPFInterfaces(c *call, v ospf.Version, a ospfArgs) error {
	st, err := sh.ospfStatus(c, v, a)
	if err != nil {
		return err
	}
	for _, in := range st {
		instTitle(c, st, in)
		fmt.Fprintf(c.out, "%-12s %-8s %-10s %-31s %-5s %s\n", "Interface", "State", "Area", "DR/BDR", "Nbrs", "Cost")
		for _, i := range in.Interfaces {
			if a.target != "" && i.Name != a.target {
				continue
			}
			fmt.Fprintf(c.out, "%-12s %-8s %-10s %-31s %-5d %d\n", i.Name, i.State, i.Area, orDash(i.DRs), i.Neighbors, i.Cost)
			if a.detail {
				typ := "broadcast"
				if i.P2P {
					typ = "p2p"
				}
				if i.Passive {
					typ += ", passive"
				}
				fmt.Fprintf(c.out, "  Type %s, address %s, hello %ds, dead %ds, %d adjacent, in state for %s\n",
					typ, i.Addr, i.Hello, i.Dead, i.Adjacent, fmtDur(i.Since))
			}
		}
	}
	return nil
}

func lsaTypeMatches(v ospf.Version, t ospf.LSType, want string) bool {
	if want == "" {
		return true
	}
	m := map[string][2]ospf.LSType{
		"router": {ospf.V2Router, ospf.V3Router}, "network": {ospf.V2Network, ospf.V3Network},
		"summary": {ospf.V2Summary, ospf.V3InterAreaPrefix}, "asbr-summary": {ospf.V2ASBRSummary, ospf.V3InterAreaRouter},
		"external": {ospf.V2External, ospf.V3External}, "link": {0xffff, ospf.V3Link},
		"intra-area-prefix": {0xffff, ospf.V3IntraAreaPrefix},
	}[want]
	if v == ospf.V2 {
		return t == m[0]
	}
	return t == m[1]
}

func (sh *Shell) showOSPFDatabase(c *call, v ospf.Version, a ospfArgs) error {
	st, err := sh.ospfStatus(c, v, a)
	if err != nil {
		return err
	}
	for _, in := range st {
		instTitle(c, st, in)
		last := ""
		for _, l := range in.Database {
			if !lsaTypeMatches(v, l.Type, a.lsaType) || (a.lsaID != "" && l.ID.String() != a.lsaID) ||
				(a.advRouter != "" && l.AdvRtr.String() != a.advRouter) {
				continue
			}
			head := "    AS external link state database"
			switch l.Scope {
			case "area":
				head = fmt.Sprintf("    OSPF database, Area %s", l.Area)
			case "link":
				head = fmt.Sprintf("    OSPF link-local database, interface %s, Area %s", l.Iface, l.Area)
			}
			if head != last {
				fmt.Fprintf(c.out, "\n%s\n %-10s %-15s %-15s %-10s %-5s %-6s %s\n", head, "Type", "ID", "Adv Rtr", "Seq", "Age", "Cksum", "Len")
				last = head
			}
			self := " "
			if l.Self {
				self = "*"
			}
			fmt.Fprintf(c.out, "%s%-10s %-15s %-15s 0x%08x %-5d 0x%04x %d\n", self, l.TypeName, l.ID, l.AdvRtr, l.Seq, l.Age, l.Checksum, l.Length)
			if a.detail && l.LSA != nil {
				writeLSABody(c, v, l.LSA)
			}
		}
	}
	return nil
}

func writeLSABody(c *call, v ospf.Version, l *ospf.LSA) {
	w := func(f string, args ...any) { fmt.Fprintf(c.out, "  "+f+"\n", args...) }
	linkType := map[uint8]string{ospf.LinkP2P: "PointToPoint", ospf.LinkTransit: "Transit", ospf.LinkStub: "Stub", ospf.LinkVirtual: "Virtual"}
	switch {
	case l.Type == ospf.V2Router || l.Type == ospf.V3Router:
		var flags []string
		if l.Flags&ospf.FlagB != 0 {
			flags = append(flags, "ABR")
		}
		if l.Flags&ospf.FlagE != 0 {
			flags = append(flags, "ASBR")
		}
		w("bits 0x%x %s, %d links", l.Flags, strings.Join(flags, " "), len(l.Links))
		for _, k := range l.Links {
			if v == ospf.V2 {
				w("  id %s, data %s, type %s, metric %d", k.ID, k.Data, linkType[k.Type], k.Metric)
			} else {
				w("  type %s, metric %d, interface %d, neighbour %s interface %d", linkType[k.Type], k.Metric, k.IfID, k.NbrRouter, k.NbrIfID)
			}
		}
	case l.Type == ospf.V2Network || l.Type == ospf.V3Network:
		var rs []string
		for _, r := range l.Attached {
			rs = append(rs, r.String())
		}
		if v == ospf.V2 {
			w("mask /%d, attached routers %s", l.MaskBits, strings.Join(rs, " "))
		} else {
			w("attached routers %s", strings.Join(rs, " "))
		}
	case l.Type == ospf.V2Summary || l.Type == ospf.V3InterAreaPrefix:
		w("prefix %s, metric %d", l.Prefix, l.Metric)
	case l.Type == ospf.V2ASBRSummary || l.Type == ospf.V3InterAreaRouter:
		w("router %s, metric %d", l.DestRouter, l.Metric)
	case l.Type == ospf.V2External || l.Type == ospf.V3External:
		t := 1
		if l.E2 {
			t = 2
		}
		fw := "-"
		if l.Forward.IsValid() {
			fw = l.Forward.String()
		}
		w("prefix %s, type %d, metric %d, forward %s, tag %d", l.Prefix, t, l.Metric, fw, l.Tag)
	case l.Type == ospf.V3Link:
		w("link-local %s, priority %d", l.LinkLocal, l.Priority)
		for _, p := range l.Prefixes {
			w("  prefix %s", p.Prefix)
		}
	case l.Type == ospf.V3IntraAreaPrefix:
		w("referenced %s %s %s", v.TypeName(l.RefType), l.RefID, l.RefAdvRtr)
		for _, p := range l.Prefixes {
			w("  prefix %s metric %d", p.Prefix, p.Metric)
		}
	}
}

func (sh *Shell) showOSPFRoutes(c *call, v ospf.Version, a ospfArgs) error {
	st, err := sh.ospfStatus(c, v, a)
	if err != nil {
		return err
	}
	for _, in := range st {
		instTitle(c, st, in)
		fmt.Fprintf(c.out, "%-28s %-6s %-12s %-10s %s\n", "Prefix", "Type", "Area", "Cost", "Next hops")
		for _, r := range in.Routes {
			cost := fmt.Sprint(r.Cost)
			if r.Type == ospf.External2 {
				cost = fmt.Sprintf("%d (%d)", r.Cost2, r.Cost)
			}
			area := r.Area.String()
			if r.Type >= ospf.External1 {
				area = "-"
			}
			var hops []string
			if r.Direct {
				hops = []string{"direct " + r.NextHops[0].Iface}
			}
			for _, h := range r.NextHops {
				if !r.Direct {
					hops = append(hops, h.String())
				}
			}
			fmt.Fprintf(c.out, "%-28s %-6s %-12s %-10s %s\n", r.Prefix, r.Type, area, cost, strings.Join(hops, ", "))
		}
	}
	return nil
}

func (sh *Shell) showOSPFOverview(c *call, v ospf.Version, a ospfArgs) error {
	st, err := sh.ospfStatus(c, v, a)
	if err != nil {
		return err
	}
	for _, in := range st {
		instTitle(c, st, in)
		o := in.Overview
		var areas []string
		for _, ar := range o.Areas {
			areas = append(areas, ar.String())
		}
		fmt.Fprintf(c.out, "Router ID: %s\n", o.RouterID)
		if len(o.Roles) > 0 {
			fmt.Fprintf(c.out, "Roles: %s\n", strings.Join(o.Roles, ", "))
		}
		fmt.Fprintf(c.out, "Areas: %s\n", strings.Join(areas, ", "))
		if o.Overloaded {
			c.out.WriteString("Overload: announced (maximum metric)\n")
		}
		switch {
		case !o.GracefulRestart:
			c.out.WriteString("Graceful restart: disabled\n")
		case o.RestartLeft > 0:
			fmt.Fprintf(c.out, "Graceful restart: restarting, at most %s more\n", fmtDur(o.RestartLeft))
		case o.Helping > 0:
			fmt.Fprintf(c.out, "Graceful restart: helping %d restarting neighbour(s)\n", o.Helping)
		default:
			c.out.WriteString("Graceful restart: enabled (helper and restarting)\n")
		}
		last := "never"
		if o.SPFRuns > 0 {
			last = fmtDur(o.LastSPF) + " ago"
		}
		fmt.Fprintf(c.out, "SPF: %d runs, last %s (%s)\nLSAs: %d, of them external: %d\nUp: %s\n",
			o.SPFRuns, last, o.SPFDuration.Round(time.Microsecond), o.LSAs, o.Externals, fmtDur(o.Uptime))
	}
	return nil
}

func (sh *Shell) showOSPFStats(c *call, v ospf.Version, a ospfArgs) error {
	st, err := sh.ospfStatus(c, v, a)
	if err != nil {
		return err
	}
	for _, in := range st {
		instTitle(c, st, in)
		s := in.Stats
		fmt.Fprintf(c.out, "%-8s %12s %12s\n", "Packet", "Sent", "Received")
		for t := uint8(1); t <= 5; t++ {
			fmt.Fprintf(c.out, "%-8s %12d %12d\n", ospf.TypeName(t), s.Tx[t], s.Rx[t])
		}
		fmt.Fprintf(c.out, "Errors: %d, authentication failures: %d\n", s.RxErrors, s.AuthFailures)
	}
	return nil
}
