package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

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
