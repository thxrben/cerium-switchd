package cli

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/schema"
	"github.com/thxrben/cerium-switchd/pkg/lacp"
)

// completeAE offers the configured aggregated interfaces.
func completeAE(sh *Shell, args []config.Token, partial string) []Completion {
	if len(args) > 0 {
		return nil
	}
	out := []Completion{enter}
	if cfg := sh.activeModel(); cfg != nil {
		var names []string
		for n, i := range cfg.Interfaces {
			if i.AE {
				names = append(names, n)
			}
		}
		slices.SortFunc(names, func(a, b string) int { return strings.Compare(a, b) })
		for _, n := range names {
			out = append(out, Completion{Text: n, Help: "Aggregated interface"})
		}
	}
	return append(out[:1], filter(out[1:], partial)...)
}

func (sh *Shell) lacpBundles(c *call) ([]lacp.BundleStatus, error) {
	var only string
	switch len(c.args) {
	case 0:
	case 1:
		if !schema.IsAE(c.args[0].Text) {
			return nil, &posError{pos: c.argPos(0), msg: "expecting an aggregated interface (aeN)"}
		}
		only = c.args[0].Text
	default:
		return nil, &posError{pos: c.argPos(1), msg: "syntax error"}
	}
	if sh.env.Ops == nil {
		return nil, errors.New("LACP information is not available")
	}
	bs, err := sh.env.Ops.LACP()
	if err := partial(c, err); err != nil {
		return nil, err
	}
	for i := range bs {
		bs[i].Ports = slices.DeleteFunc(bs[i].Ports, func(p lacp.PortStatus) bool { return !c.shows(bs[i].PortNames[p.Name]) })
	}
	if only != "" {
		bs = slices.DeleteFunc(bs, func(b lacp.BundleStatus) bool { return b.Name != only })
		if len(bs) == 0 {
			return nil, fmt.Errorf("%s: no LACP (not configured, no member port, or no LACP)", only)
		}
	}
	return bs, nil
}

func yesNo(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

func lacpStateRow(name, role string, s lacp.State) string {
	timeout, activity := "Slow", "Passive"
	if s.Has(lacp.Timeout) {
		timeout = "Fast"
	}
	if s.Has(lacp.Activity) {
		activity = "Active"
	}
	return fmt.Sprintf("      %-12s %8s %5s %5s %5s %4s %4s %5s %8s %9s\n", name, role, yesNo(s.Has(lacp.Expired)), yesNo(s.Has(lacp.Defaulted)),
		yesNo(s.Has(lacp.Distributing)), yesNo(s.Has(lacp.Collecting)), yesNo(s.Has(lacp.Sync)), yesNo(s.Has(lacp.Aggregation)), timeout, activity)
}

// showLACP implements "show lacp interfaces [aeN]" (Junos layout).
func (sh *Shell) showLACP(c *call) error {
	bs, err := sh.lacpBundles(c)
	if err != nil {
		return err
	}
	if len(bs) == 0 {
		c.out.WriteString("No LACP bundles.\n")
		return nil
	}
	for i, b := range bs {
		if i > 0 {
			c.out.WriteString("\n")
		}
		fmt.Fprintf(c.out, "Aggregated interface: %s\n", b.Name)
		// One system for the stack (reference 5.3.2): any port that
		// announces another one is named.
		if len(b.Ports) > 0 {
			a := b.Ports[0].Actor
			fmt.Fprintf(c.out, "    Actor system: %s, key %d\n", a.System, a.Key)
			for _, p := range b.Ports[1:] {
				if p.Actor.System != a.System || p.Actor.Key != a.Key {
					fmt.Fprintf(c.out, "    Actor system of %s: %s, key %d (differs!)\n", b.PortNames[p.Name], p.Actor.System, p.Actor.Key)
				}
			}
		}
		for _, p := range b.Ports {
			if p.Held {
				fmt.Fprintf(c.out, "    %s: MACsec: negotiating (the port joins the bundle once MKA has secured it)\n", b.PortNames[p.Name])
			}
			if p.PartnerDeaf {
				fmt.Fprintf(c.out, "    Warning: the partner of %s does not receive our LACPDUs (its LACPDUs do not name this port); check the cable, this port's transmit path and the partner's port\n", b.PortNames[p.Name])
			}
			if p.SlowPartner {
				fmt.Fprintf(c.out, "    Warning: the partner of %s sends LACPDUs less often than 'periodic fast' needs; configure 'lacp periodic slow'\n", b.PortNames[p.Name])
			}
		}
		fmt.Fprintf(c.out, "    LACP state:       %8s %5s %5s %5s %4s %4s %5s %8s %9s\n", "Role", "Exp", "Def", "Dist", "Col", "Syn", "Aggr", "Timeout", "Activity")
		for _, p := range b.Ports {
			n := b.PortNames[p.Name]
			c.out.WriteString(lacpStateRow(n, "Actor", p.Actor.State))
			c.out.WriteString(lacpStateRow(n, "Partner", p.Partner.State))
		}
		fmt.Fprintf(c.out, "    LACP protocol:        %13s %15s %23s  %s\n", "Receive State", "Transmit State", "Mux State", "Partner")
		for _, p := range b.Ports {
			tx := "No periodic"
			if p.Actor.State.Has(lacp.Activity) || p.Partner.State.Has(lacp.Activity) {
				tx = "Slow periodic"
				if p.Partner.State.Has(lacp.Timeout) {
					tx = "Fast periodic"
				}
			}
			mux := p.Mux.String()
			if !p.Selected && p.Rx == lacp.RxCurrent {
				mux = "Detached (not selected)"
			}
			partner := "-"
			if p.Partner.System.MAC != [6]byte{} {
				partner = fmt.Sprintf("%s, key %d, port %d", p.Partner.System, p.Partner.Key, p.Partner.Port)
			}
			fmt.Fprintf(c.out, "      %-12s %19s %15s %23s  %s\n", b.PortNames[p.Name], p.Rx, tx, mux, partner)
		}
	}
	return nil
}

// showLACPStats implements "show lacp statistics interfaces [aeN]".
func (sh *Shell) showLACPStats(c *call) error {
	bs, err := sh.lacpBundles(c)
	if err != nil {
		return err
	}
	if len(bs) == 0 {
		c.out.WriteString("No LACP bundles.\n")
		return nil
	}
	for i, b := range bs {
		if i > 0 {
			c.out.WriteString("\n")
		}
		fmt.Fprintf(c.out, "Aggregated interface: %s\n", b.Name)
		fmt.Fprintf(c.out, "    %-14s %10s %10s %12s %12s\n", "Port", "LACP Rx", "LACP Tx", "Unknown Rx", "Illegal Rx")
		for _, p := range b.Ports {
			fmt.Fprintf(c.out, "      %-12s %10d %10d %12d %12d\n", b.PortNames[p.Name], p.Stats.RxPDUs, p.Stats.TxPDUs, 0, p.Stats.RxErrors)
		}
	}
	return nil
}

// showMCLAG implements "show mclag [consistency]": every MC-LAG pair of the
// stack (reference 5.6), from both members' views; "member <id>" narrows it
// to that member's pair.
func (sh *Shell) showMCLAG(c *call) error {
	consistency := false
	switch {
	case len(c.args) == 1 && prefixOf(c.args[0].Text, "consistency"):
		consistency = true
	default:
		if err := noArgs(c); err != nil {
			return err
		}
	}
	if sh.env.Ops == nil {
		return errors.New("MC-LAG information is not available")
	}
	sts, err := sh.env.Ops.MCLAG()
	if err := partial(c, err); err != nil {
		return err
	}
	pairs := mclagPairs(sts, c.only)
	if len(pairs) == 0 {
		c.out.WriteString("No MC-LAG bundles (an MC-LAG is a bundle with ports on two members).\n")
		return nil
	}
	for i, p := range pairs {
		if i > 0 {
			c.out.WriteString("\n")
		}
		if consistency {
			showMCLAGConsistency(c, p)
		} else {
			showMCLAGPair(c, p)
		}
	}
	return nil
}

// mclagPair is one pair with what each of its members reported (nil: it
// did not answer).
type mclagPair struct {
	a, b   int // members, a < b
	st     map[int]*MCLAGStatus
	bundle []string
}

func mclagPairs(sts []MCLAGStatus, only []int) []*mclagPair {
	by := map[int]*mclagPair{}
	for i := range sts {
		s := &sts[i]
		if s.Pair == 0 || (only != nil && !slices.Contains(only, s.Member)) {
			continue
		}
		p := by[s.Pair]
		if p == nil {
			p = &mclagPair{a: min(s.Member, s.Peer), b: max(s.Member, s.Peer), st: map[int]*MCLAGStatus{}}
			by[s.Pair] = p
		}
		p.st[s.Member] = s
		for _, bd := range s.Bundles {
			if !slices.Contains(p.bundle, bd.Name) {
				p.bundle = append(p.bundle, bd.Name)
			}
		}
	}
	var out []*mclagPair
	for _, id := range slices.Sorted(maps.Keys(by)) {
		p := by[id]
		slices.SortFunc(p.bundle, func(x, y string) int { return strings.Compare(x, y) })
		out = append(out, p)
	}
	return out
}

// leg is member m's leg of bundle b: from m itself, else as its peer saw it.
func (p *mclagPair) leg(m int, b string) (MCLAGBundle, string) {
	if s := p.st[m]; s != nil {
		for _, x := range s.Bundles {
			if x.Name == b {
				return x, upDown(x.LocalUp)
			}
		}
		return MCLAGBundle{}, "-"
	}
	o := p.a + p.b - m
	if s := p.st[o]; s != nil {
		for _, x := range s.Bundles {
			if x.Name == b && x.PeerKnown {
				return MCLAGBundle{}, upDown(x.PeerUp) + " (peer's view)"
			}
		}
	}
	return MCLAGBundle{}, "unknown"
}

func showMCLAGPair(c *call, p *mclagPair) {
	role := func(m int) string {
		if s := p.st[m]; s != nil {
			if s.Primary {
				return "primary"
			}
			return "secondary"
		}
		if s := p.st[p.a+p.b-m]; s != nil {
			if s.Primary {
				return "secondary"
			}
			return "primary"
		}
		return "?"
	}
	fmt.Fprintf(c.out, "MC-LAG pair: member %d (%s), member %d (%s)\n", p.a, role(p.a), p.b, role(p.b))
	for _, m := range []int{p.a, p.b} {
		s := p.st[m]
		if s == nil {
			fmt.Fprintf(c.out, "  Member %d: did not answer\n", m)
			continue
		}
		reach := "reaches its peer over the stack"
		if !s.PeerReachable {
			reach = "does NOT reach its peer over the stack"
		}
		legs := "peer's leg states received " + fmtDuration(time.Since(s.PeerSeen)) + " ago"
		if !s.PeerKnown {
			legs = "no leg states from the peer yet (split horizon assumes its legs are up)"
		}
		fmt.Fprintf(c.out, "  Member %d: %s (stack members reached: %d of %d); %s\n", m, reach, s.Reach, s.Members, legs)
	}
	ma, mb := fmt.Sprintf("Member %d", p.a), fmt.Sprintf("Member %d", p.b)
	fmt.Fprintf(c.out, "\n  %-10s %-20s %-20s %-14s %s\n", "Bundle", ma, mb, "Split horizon", "Hold")
	for _, b := range p.bundle {
		la, sa := p.leg(p.a, b)
		lb, sb := p.leg(p.b, b)
		var split, hold []string
		for _, x := range []struct {
			m int
			l MCLAGBundle
		}{{p.a, la}, {p.b, lb}} {
			if p.st[x.m] == nil {
				continue
			}
			on := "off"
			if x.l.SplitHorizon {
				on = "on"
			}
			split = append(split, fmt.Sprintf("%d:%s", x.m, on))
			if x.l.Hold != "" {
				hold = append(hold, fmt.Sprintf("%d: %s", x.m, x.l.Hold))
			}
		}
		h := "-"
		if len(hold) > 0 {
			h = strings.Join(hold, "; ")
		}
		fmt.Fprintf(c.out, "  %-10s %-20s %-20s %-14s %s\n", b, sa, sb, strings.Join(split, " "), h)
	}
}

func showMCLAGConsistency(c *call, p *mclagPair) {
	fmt.Fprintf(c.out, "MC-LAG pair: member %d, member %d\n", p.a, p.b)
	for _, b := range p.bundle {
		facts := map[int]string{}
		var differs time.Time
		for m, s := range p.st {
			for _, x := range s.Bundles {
				if x.Name != b {
					continue
				}
				facts[m] = x.Facts
				if x.PeerFacts != "" && facts[s.Peer] == "" {
					facts[s.Peer] = x.PeerFacts
				}
				if !x.DiffersSince.IsZero() && (differs.IsZero() || x.DiffersSince.Before(differs)) {
					differs = x.DiffersSince
				}
			}
		}
		state := "consistent"
		switch {
		case facts[p.a] == "" || facts[p.b] == "":
			state = "unknown (one member's view is missing)"
		case !differs.IsZero():
			state = "inconsistent for " + fmtDuration(time.Since(differs))
		case facts[p.a] != facts[p.b]:
			state = "differs (a commit on its way?)"
		}
		fmt.Fprintf(c.out, "  %s: %s\n", b, state)
		for _, m := range []int{p.a, p.b} {
			fmt.Fprintf(c.out, "    Member %d: %s\n", m, orDash(facts[m]))
		}
	}
}
