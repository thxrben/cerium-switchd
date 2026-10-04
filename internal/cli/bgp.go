package cli

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/internal/bgpd"
	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/pkg/bgp"
)

// BGP is implemented by members that run cer-bgpd (reference 5.14).
type BGP interface {
	BGPStatus(instance *string) ([]bgpd.InstanceStatus, error)
	BGPAdj(q bgpd.AdjRequest) ([]bgp.InPath, error)
	ClearBGP(q bgpd.ClearRequest) (int, error)
}

func (sh *Shell) bgp() (BGP, error) {
	b, ok := sh.env.Ops.(BGP)
	if sh.env.Ops == nil || !ok {
		return nil, errors.New("BGP information is not available")
	}
	return b, nil
}

type bgpArgs struct {
	instance *string
	target   string // a neighbour address or a group name
	mode     string // clear: soft, soft-inbound
}

func parseBGPArgs(c *call, modes bool) (bgpArgs, error) {
	var a bgpArgs
	for i := 0; i < len(c.args); i++ {
		t := c.args[i].Text
		switch {
		case t == "instance":
			if i+1 >= len(c.args) {
				return a, &posError{pos: c.argPos(i + 1), msg: "expecting a routing instance"}
			}
			i++
			s := c.args[i].Text
			a.instance = &s
		case modes && (t == bgp.ClearSoft || t == bgp.ClearSoftInbound):
			a.mode = t
		case a.target == "":
			a.target = t
		default:
			return a, &posError{pos: c.argPos(i), msg: "syntax error"}
		}
	}
	return a, nil
}

func bgpCommand() *command {
	run := func(f func(sh *Shell, c *call, a bgpArgs) error) func(*Shell, *call) error {
		return func(sh *Shell, c *call) error {
			a, err := parseBGPArgs(c, false)
			if err != nil {
				return err
			}
			return f(sh, c, a)
		}
	}
	inst := words(Completion{Text: "instance", Help: "Routing instance"})
	return &command{name: "bgp", help: "Show BGP information", class: commit.ReadOnly, sub: []*command{
		{name: "summary", help: "Show the BGP neighbours and their prefixes", class: commit.ReadOnly, run: run((*Shell).showBGPSummary), complete: inst},
		{name: "neighbor", help: "Show everything about the BGP neighbours", class: commit.ReadOnly, run: run((*Shell).showBGPNeighbor), complete: inst},
		{name: "group", help: "Show the BGP groups and their neighbours", class: commit.ReadOnly, run: run((*Shell).showBGPGroup), complete: inst},
	}}
}

func clearBGPCommand() *command {
	return &command{name: "bgp", help: "Clear BGP state", class: commit.Operator, sub: []*command{
		{name: "neighbor", help: "Reset BGP sessions (soft: send and request the routes again; soft-inbound: evaluate the import policy again)",
			class: commit.Operator,
			run: func(sh *Shell, c *call) error {
				a, err := parseBGPArgs(c, true)
				if err != nil {
					return err
				}
				b, err := sh.bgp()
				if err != nil {
					return err
				}
				q := bgpd.ClearRequest{Mode: a.mode}
				if a.instance != nil {
					q.Instance = *a.instance
				}
				if a.target != "" {
					if q.Neighbor, err = netip.ParseAddr(a.target); err != nil {
						return &posError{pos: c.argPos(0), msg: "expecting a neighbour address"}
					}
				}
				n, err := b.ClearBGP(q)
				if err != nil {
					return err
				}
				what := "sessions reset"
				switch a.mode {
				case bgp.ClearSoft:
					what = "neighbours refreshed"
				case bgp.ClearSoftInbound:
					what = "neighbours' routes evaluated again"
				}
				fmt.Fprintf(c.out, "%d %s\n", n, what)
				return nil
			},
			complete: words(Completion{Text: "soft", Help: "Send the routes again and ask for the neighbour's"},
				Completion{Text: "soft-inbound", Help: "Evaluate the import policy again"},
				Completion{Text: "instance", Help: "Routing instance"})},
	}}
}

func (sh *Shell) bgpStatus(c *call, a bgpArgs) ([]bgpd.InstanceStatus, error) {
	b, err := sh.bgp()
	if err != nil {
		return nil, err
	}
	st, err := b.BGPStatus(a.instance)
	if err != nil {
		return nil, err
	}
	if len(st) == 0 {
		c.out.WriteString("BGP is not running here (it runs on the master, where it is configured).\n")
	}
	return st, nil
}

func bgpInstTitle(c *call, st []bgpd.InstanceStatus, in bgpd.InstanceStatus) {
	if len(st) > 1 || in.Instance != "" {
		fmt.Fprintf(c.out, "Instance: %s\n", instName(in.Instance))
	}
}

func instName(n string) string {
	if n == "" {
		return "default"
	}
	return n
}

// bgpAge is Junos' up/down time column.
func bgpAge(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd %d:%02d:%02d", d/(24*time.Hour), int(d.Hours())%24, int(d.Minutes())%60, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

func tableName(instance string, f bgp.Family) string {
	n := "inet.0"
	if f == bgp.IPv6Unicast {
		n = "inet6.0"
	}
	if instance != "" {
		return instance + "." + n
	}
	return n
}

func (sh *Shell) showBGPSummary(c *call, a bgpArgs) error {
	st, err := sh.bgpStatus(c, a)
	if err != nil {
		return err
	}
	for _, in := range st {
		bgpInstTitle(c, st, in)
		groups, down := map[string]bool{}, 0
		tot := map[bgp.Family][2]int{}
		for _, n := range in.Neighbors {
			groups[n.Group] = true
			if n.State != bgp.Established.String() {
				down++
			}
			for _, fc := range n.Counts {
				t := tot[fc.Family]
				t[0] += fc.Received
				t[1] += fc.Active
				tot[fc.Family] = t
			}
		}
		fmt.Fprintf(c.out, "Local AS: %d   Router ID: %s\n", in.AS, in.RouterID)
		fmt.Fprintf(c.out, "Groups: %d Peers: %d Down peers: %d\n", len(groups), len(in.Neighbors), down)
		fmt.Fprintf(c.out, "%-14s %10s %10s\n", "Table", "Tot Paths", "Act Paths")
		for _, f := range []bgp.Family{bgp.IPv4Unicast, bgp.IPv6Unicast} {
			if t, ok := tot[f]; ok {
				fmt.Fprintf(c.out, "%-14s %10d %10d\n", tableName(in.Instance, f), t[0], t[1])
			}
		}
		fmt.Fprintf(c.out, "%-24s %10s %8s %8s %6s %13s %s\n", "Peer", "AS", "InPkt", "OutPkt", "Flaps", "Last Up/Dwn", "State|#Active/Received/Accepted")
		for _, n := range in.Neighbors {
			state := n.State
			if n.Disabled {
				state = "Idle (disabled)"
			}
			if n.State == bgp.Established.String() {
				state = "Establ"
			}
			fmt.Fprintf(c.out, "%-24s %10d %8d %8d %6d %13s %s\n", n.Addr, n.PeerAS, n.Stats.MsgsIn, n.Stats.MsgsOut, n.Stats.Flaps, bgpAge(n.Since), state)
			if n.State == bgp.Established.String() {
				for _, fc := range n.Counts {
					fmt.Fprintf(c.out, "  %s: %d/%d/%d\n", tableName(in.Instance, fc.Family), fc.Active, fc.Received, fc.Accepted)
				}
			}
		}
	}
	return nil
}

func (sh *Shell) showBGPNeighbor(c *call, a bgpArgs) error {
	st, err := sh.bgpStatus(c, a)
	if err != nil {
		return err
	}
	found := false
	for _, in := range st {
		for _, n := range in.Neighbors {
			if a.target != "" && n.Addr.String() != a.target {
				continue
			}
			found = true
			typ := "External"
			if n.Internal {
				typ = "Internal"
			}
			fmt.Fprintf(c.out, "Peer: %s AS %d    Local: %s AS %d\n", n.Addr, n.PeerAS, dashAddr(n.Local), n.LocalAS)
			fmt.Fprintf(c.out, "  Group: %s    Routing-Instance: %s\n", n.Group, instName(in.Instance))
			fmt.Fprintf(c.out, "  Type: %s    State: %s (%s)\n", typ, n.State, bgpAge(n.Since))
			lastErr := n.LastError
			if lastErr == "" {
				lastErr = "None"
			}
			fmt.Fprintf(c.out, "  Last Error: %s\n", lastErr)
			fmt.Fprintf(c.out, "  Export: [ %s ] Import: [ %s ]\n", strings.Join(n.Export, " "), strings.Join(n.Import, " "))
			var opts []string
			if n.Passive {
				opts = append(opts, "Passive")
			}
			if n.Client {
				opts = append(opts, "Cluster (route reflector client)")
			}
			if n.Disabled {
				opts = append(opts, "Disabled")
			}
			if len(opts) > 0 {
				fmt.Fprintf(c.out, "  Options: <%s>\n", strings.Join(opts, " "))
			}
			fmt.Fprintf(c.out, "  Holdtime: %d    Active Holdtime: %d    Number of flaps: %d\n", n.LocalHold, n.HoldTime, n.Stats.Flaps)
			if n.State == bgp.Established.String() {
				fmt.Fprintf(c.out, "  Peer ID: %s    Local ID: %s\n", n.RouterID, in.RouterID)
				var fams []string
				for _, f := range n.Families {
					fams = append(fams, f.String())
				}
				fmt.Fprintf(c.out, "  NLRI for this session: %s\n", strings.Join(fams, " "))
				if n.RouteRefresh {
					c.out.WriteString("  Peer supports Refresh capability\n")
				}
				if n.AS4 {
					fmt.Fprintf(c.out, "  Peer supports 4 byte AS extension (peer-as %d)\n", n.PeerAS)
				}
				if n.GR {
					fmt.Fprintf(c.out, "  Peer supports graceful restart (restart time %d s)\n", n.GRTime)
				}
			}
			if n.Stale {
				c.out.WriteString("  The neighbour is restarting: its routes are kept (stale)\n")
			}
			for _, fc := range n.Counts {
				fmt.Fprintf(c.out, "  Table %s\n", tableName(in.Instance, fc.Family))
				fmt.Fprintf(c.out, "    Active prefixes:     %8d\n", fc.Active)
				fmt.Fprintf(c.out, "    Received prefixes:   %8d\n", fc.Received)
				fmt.Fprintf(c.out, "    Accepted prefixes:   %8d\n", fc.Accepted)
				fmt.Fprintf(c.out, "    Advertised prefixes: %8d\n", fc.Advertised)
			}
			fmt.Fprintf(c.out, "  Input messages:  Total %d    Updates %d\n", n.Stats.MsgsIn, n.Stats.UpdatesIn)
			fmt.Fprintf(c.out, "  Output messages: Total %d    Updates %d\n\n", n.Stats.MsgsOut, n.Stats.UpdatesOut)
		}
	}
	if a.target != "" && !found && len(st) > 0 {
		return fmt.Errorf("no BGP neighbour %s", a.target)
	}
	return nil
}

func dashAddr(a netip.Addr) string {
	if !a.IsValid() {
		return "-"
	}
	return a.String()
}

func (sh *Shell) showBGPGroup(c *call, a bgpArgs) error {
	st, err := sh.bgpStatus(c, a)
	if err != nil {
		return err
	}
	for _, in := range st {
		var order []string
		by := map[string][]bgp.NeighborStatus{}
		for _, n := range in.Neighbors {
			if a.target != "" && n.Group != a.target {
				continue
			}
			if _, ok := by[n.Group]; !ok {
				order = append(order, n.Group)
			}
			by[n.Group] = append(by[n.Group], n)
		}
		for _, g := range order {
			ns := by[g]
			typ, est := "External", 0
			if ns[0].Internal {
				typ = "Internal"
			}
			for _, n := range ns {
				if n.State == bgp.Established.String() {
					est++
				}
			}
			fmt.Fprintf(c.out, "Group Type: %s    Local AS: %d\n", typ, ns[0].LocalAS)
			fmt.Fprintf(c.out, "  Name: %s    Routing-Instance: %s\n", g, instName(in.Instance))
			fmt.Fprintf(c.out, "  Export: [ %s ] Import: [ %s ]\n", strings.Join(ns[0].Export, " "), strings.Join(ns[0].Import, " "))
			fmt.Fprintf(c.out, "  Total peers: %d    Established: %d\n", len(ns), est)
			for _, n := range ns {
				fmt.Fprintf(c.out, "  %s (%s)\n", n.Addr, n.State)
			}
			c.out.WriteString("\n")
		}
	}
	return nil
}
