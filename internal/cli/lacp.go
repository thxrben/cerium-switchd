package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"mclag/internal/config"
	"mclag/internal/lacp"
	"mclag/internal/schema"
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
	if err != nil {
		return nil, err
	}
	if only != "" {
		bs = slices.DeleteFunc(bs, func(b lacp.BundleStatus) bool { return b.Name != only })
		if len(bs) == 0 {
			return nil, fmt.Errorf("%s: no LACP on this member (not configured, no member port here, or no LACP)", only)
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
		c.out.WriteString("No LACP bundles on this member.\n")
		return nil
	}
	for i, b := range bs {
		if i > 0 {
			c.out.WriteString("\n")
		}
		fmt.Fprintf(c.out, "Aggregated interface: %s\n", b.Name)
		if len(b.Ports) > 0 {
			a := b.Ports[0].Actor
			fmt.Fprintf(c.out, "    Actor system: %s, key %d\n", a.System, a.Key)
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
		c.out.WriteString("No LACP bundles on this member.\n")
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

// showMCLAG implements "show mclag".
func (sh *Shell) showMCLAG(c *call) error {
	consistency := false
	switch {
	case len(c.args) == 1 && c.args[0].Text == "consistency":
		consistency = true
	default:
		if err := noArgs(c); err != nil {
			return err
		}
	}
	if sh.env.Ops == nil {
		return errors.New("MC-LAG information is not available")
	}
	st, err := sh.env.Ops.MCLAG()
	if err != nil {
		return err
	}
	if st.Domain == 0 {
		c.out.WriteString("This member is not in an MC-LAG domain.\n")
		return nil
	}
	if consistency {
		return showMCLAGConsistency(c, st)
	}
	role := "secondary"
	if st.Primary {
		role = "primary"
	}
	stack := "reachable"
	if !st.PeerReachable {
		stack = "not reachable"
	}
	fmt.Fprintf(c.out, "MC-LAG domain %d: member %d (%s), peer member %d\n", st.Domain, st.Member, role, st.Peer)
	fmt.Fprintf(c.out, "  Peer and its stack tunnel vc-%d: %s\n", st.Peer, stack)
	if st.Members > 2 {
		fmt.Fprintf(c.out, "  Stack members reached: %d of %d\n", st.Reach, st.Members)
	}
	if st.PeerKnown {
		fmt.Fprintf(c.out, "  Peer leg states: received %s ago\n", fmtDuration(time.Since(st.PeerSeen)))
	} else {
		c.out.WriteString("  Peer leg states: not received yet (split horizon assumes the peer's legs are up)\n")
	}
	if len(st.Bundles) == 0 {
		c.out.WriteString("\nNo MC-LAG bundles with a leg on this member.\n")
		return nil
	}
	fmt.Fprintf(c.out, "\n  %-10s %-6s %-8s %-14s %s\n", "Bundle", "Local", "Peer", "Split horizon", "Hold")
	for _, b := range st.Bundles {
		peer := upDown(b.PeerUp)
		if !b.PeerKnown {
			peer = "unknown"
		}
		split := "off"
		if b.SplitHorizon {
			split = "on"
		}
		hold := "-"
		if b.Hold != "" {
			hold = b.Hold
		}
		fmt.Fprintf(c.out, "  %-10s %-6s %-8s %-14s %s\n", b.Name, upDown(b.LocalUp), peer, split, hold)
	}
	return nil
}

func showMCLAGConsistency(c *call, st MCLAGStatus) error {
	if len(st.Bundles) == 0 {
		c.out.WriteString("No MC-LAG bundles with a leg on this member.\n")
		return nil
	}
	for i, b := range st.Bundles {
		if i > 0 {
			c.out.WriteString("\n")
		}
		state := "consistent"
		switch {
		case b.PeerFacts == "":
			state = "unknown (nothing received from the peer)"
		case !b.DiffersSince.IsZero():
			state = "inconsistent for " + fmtDuration(time.Since(b.DiffersSince))
		}
		fmt.Fprintf(c.out, "%s: %s\n", b.Name, state)
		fmt.Fprintf(c.out, "  Member %d: %s\n", st.Member, b.Facts)
		if b.PeerFacts != "" {
			fmt.Fprintf(c.out, "  Member %d: %s\n", st.Peer, b.PeerFacts)
		}
	}
	return nil
}
