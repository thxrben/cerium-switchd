package cli

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"time"

	"mclag/internal/commit"
	"mclag/internal/config"
	"mclag/internal/lldp"
)

// lldpCommand is "show lldp …" (reference 5.5).
func lldpCommand() *command {
	return &command{name: "lldp", help: "Show LLDP information", class: commit.ReadOnly, sub: []*command{
		{name: "neighbors", help: "Show the LLDP neighbours of the ports", class: commit.ReadOnly, run: (*Shell).showLLDPNeighbors,
			complete: words(Completion{Text: "interface", Help: "Every TLV of one port's neighbours"})},
		{name: "local-information", help: "Show what the stack announces", class: commit.ReadOnly, run: (*Shell).showLLDPLocal},
		{name: "statistics", help: "Show LLDPDU counters per port", class: commit.ReadOnly, run: (*Shell).showLLDPStats},
	}}
}

func (sh *Shell) lldpStatus(c *call) (LLDPStatus, error) {
	if sh.env.Ops == nil {
		return LLDPStatus{}, errors.New("LLDP information is not available")
	}
	st, err := sh.env.Ops.LLDP()
	if err := partial(c, err); err != nil {
		return st, err
	}
	st.Neighbors = slices.DeleteFunc(st.Neighbors, func(n lldp.Neighbor) bool { return !c.shows(n.Port) })
	st.Ports = slices.DeleteFunc(st.Ports, func(p lldp.PortSpec) bool { return !c.shows(p.Name) })
	st.Stats = slices.DeleteFunc(st.Stats, func(s lldp.Stats) bool { return !c.shows(s.Port) })
	sort.SliceStable(st.Neighbors, func(i, j int) bool { return config.NaturalLess(st.Neighbors[i].Port, st.Neighbors[j].Port) })
	sort.SliceStable(st.Ports, func(i, j int) bool { return config.NaturalLess(st.Ports[i].Name, st.Ports[j].Name) })
	sort.SliceStable(st.Stats, func(i, j int) bool { return config.NaturalLess(st.Stats[i].Port, st.Stats[j].Port) })
	return st, nil
}

// showLLDPNeighbors is "show lldp neighbors [interface <interface>]".
func (sh *Shell) showLLDPNeighbors(c *call) error {
	port := ""
	switch {
	case len(c.args) == 0:
	case len(c.args) == 2 && prefixOf(c.args[0].Text, "interface"):
		port = c.args[1].Text
	default:
		return &posError{pos: c.argPos(0), msg: "syntax error, expecting 'interface <interface>'"}
	}
	st, err := sh.lldpStatus(c)
	if err != nil {
		return err
	}
	if !st.Running {
		c.out.WriteString("LLDP is not running ('set protocols lldp').\n")
		return nil
	}
	now := time.Now()
	ttl := func(n lldp.Neighbor) string {
		return fmt.Sprintf("%ds", max(0, int(n.Expires.Sub(now).Round(time.Second)/time.Second)))
	}
	if port == "" {
		fmt.Fprintf(c.out, "%-10s %-19s %-22s %-20s %-20s %s\n", "Interface", "Chassis ID", "Port ID", "Port description", "System name", "TTL")
		for _, n := range st.Neighbors {
			fmt.Fprintf(c.out, "%-10s %-19s %-22s %-20s %-20s %s\n", n.Port, n.Chassis, trunc(n.PortID, 22), trunc(n.PortDesc, 20), trunc(n.SysName, 20), ttl(n))
		}
		fmt.Fprintf(c.out, "%d neighbours\n", len(st.Neighbors))
		return nil
	}
	found := false
	for _, n := range st.Neighbors {
		if n.Port != port {
			continue
		}
		if found {
			c.out.WriteString("\n")
		}
		found = true
		fmt.Fprintf(c.out, "Interface: %s\n  Chassis ID: %s\n  Port ID: %s\n  Port description: %s\n", n.Port, n.Chassis, n.PortID, orDash(n.PortDesc))
		fmt.Fprintf(c.out, "  System name: %s\n  System description: %s\n", orDash(n.SysName), orDash(n.SysDesc))
		fmt.Fprintf(c.out, "  Capabilities: %s (enabled: %s)\n", lldp.CapString(n.Caps), lldp.CapString(n.Enabled))
		if len(n.Mgmt) > 0 {
			fmt.Fprintf(c.out, "  Management addresses: %s\n", strings.Join(n.Mgmt, ", "))
		}
		if n.PVID != 0 {
			fmt.Fprintf(c.out, "  Port VLAN ID: %d\n", n.PVID)
		}
		if n.MaxFrame != 0 {
			fmt.Fprintf(c.out, "  Maximum frame size: %d\n", n.MaxFrame)
		}
		fmt.Fprintf(c.out, "  Time to live: %s, known since %s\n", ttl(n), n.FirstSeen.Local().Format("2006-01-02 15:04:05"))
	}
	if !found {
		c.out.WriteString("No LLDP neighbours on " + port + ".\n")
	}
	return nil
}

// showLLDPLocal is "show lldp local-information".
func (sh *Shell) showLLDPLocal(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	st, err := sh.lldpStatus(c)
	if err != nil {
		return err
	}
	if !st.Running {
		c.out.WriteString("LLDP is not running ('set protocols lldp').\n")
		return nil
	}
	s := st.System
	fmt.Fprintf(c.out, "Chassis ID: %s\nSystem name: %s\nSystem description: %s\n", net.HardwareAddr(s.ChassisMAC), orDash(s.Name), s.Desc)
	fmt.Fprintf(c.out, "Capabilities: %s (enabled: %s)\n", lldp.CapString(s.Caps), lldp.CapString(s.Enabled))
	var mgmt []string
	for _, a := range s.Mgmt {
		mgmt = append(mgmt, a.String())
	}
	fmt.Fprintf(c.out, "Management addresses: %s\nInterval %ds, time to live %ds\n\n", orDash(strings.Join(mgmt, ", ")), s.Interval, s.Interval*s.Hold)
	fmt.Fprintf(c.out, "%-10s %-24s %-5s %-6s %s\n", "Interface", "Port description", "PVID", "Frame", "Aggregation")
	for _, p := range st.Ports {
		desc, pvid, agg := p.Desc, "-", "-"
		if desc == "" {
			desc = p.Name
		}
		if p.PVID != 0 {
			pvid = fmt.Sprint(p.PVID)
		}
		if p.Bundle != "" {
			agg = p.Bundle
		}
		fmt.Fprintf(c.out, "%-10s %-24s %-5s %-6d %s\n", p.Name, trunc(desc, 24), pvid, p.MaxFrame, agg)
	}
	return nil
}

// showLLDPStats is "show lldp statistics".
func (sh *Shell) showLLDPStats(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	st, err := sh.lldpStatus(c)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%-10s %10s %10s %10s %10s\n", "Interface", "Sent", "Received", "Discarded", "Aged out")
	for _, s := range st.Stats {
		fmt.Fprintf(c.out, "%-10s %10d %10d %10d %10d\n", s.Port, s.Sent, s.Received, s.Discarded, s.AgedOut)
	}
	return nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
