package cli

import (
	"errors"
	"fmt"
	"strings"

	"mclag/internal/commit"
	"mclag/internal/config"
)

// STPStatus is the stack's spanning tree as the RSTP owner sees it.
type STPStatus struct {
	Running                         bool
	Owner                           int
	BridgeID, RootID                string
	RootCost                        uint32
	RootPort                        string // "": this bridge is root
	HelloTime, MaxAge, ForwardDelay int
	Changes                         uint64
	Ports                           []STPPort
}

// STPPort is one RSTP port.
type STPPort struct {
	Name, Role, State                        string
	Cost                                     uint32
	PortID, DesignatedBridge, DesignatedPort string
	Edge, OperEdge, P2P, RSTP                bool
	RootInconsistent, Enabled                bool
	Rx, Tx                                   uint64
}

func (sh *Shell) stp() (STPStatus, error) {
	if sh.env.Ops == nil {
		return STPStatus{}, errors.New("not available")
	}
	st, err := sh.env.Ops.SpanningTree()
	if err != nil {
		return st, err
	}
	if !st.Running {
		return st, errors.New("RSTP is not running ('set protocols rstp' enables it)")
	}
	return st, nil
}

// showSTPBridge is "show spanning-tree bridge".
func (sh *Shell) showSTPBridge(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	st, err := sh.stp()
	if err != nil {
		return err
	}
	root := "this bridge"
	if st.RootPort != "" {
		root = fmt.Sprintf("%s, cost %d, via %s", st.RootID, st.RootCost, st.RootPort)
	}
	fmt.Fprintf(c.out, "Bridge ID          %s\n", st.BridgeID)
	fmt.Fprintf(c.out, "Root               %s\n", root)
	fmt.Fprintf(c.out, "Timers in use      hello %d s, max-age %d s, forward-delay %d s\n", st.HelloTime, st.MaxAge, st.ForwardDelay)
	fmt.Fprintf(c.out, "Topology changes   %d\n", st.Changes)
	fmt.Fprintf(c.out, "RSTP owner         member %d\n", st.Owner)
	return nil
}

// showSTPInterface is "show spanning-tree interface [<if>] [detail]".
func (sh *Shell) showSTPInterface(c *call) error {
	var name string
	detail := false
	for _, a := range c.args {
		switch {
		case prefixOf(a.Text, "detail") && !a.Quoted:
			detail = true
		case name == "":
			name = a.Text
		default:
			return &posError{pos: a.Pos, msg: "syntax error, expecting an interface or 'detail'"}
		}
	}
	st, err := sh.stp()
	if err != nil {
		return err
	}
	if !detail {
		fmt.Fprintf(c.out, "%-12s %-8s %-11s %-11s %-9s %-24s %s\n", "Interface", "Port ID", "Role", "State", "Cost", "Designated bridge", "Flags")
	}
	found := false
	for _, p := range st.Ports {
		if name != "" && p.Name != name {
			continue
		}
		found = true
		var flags []string
		if p.Edge {
			flags = append(flags, "edge")
		} else if p.OperEdge {
			flags = append(flags, "oper-edge")
		}
		if !p.P2P {
			flags = append(flags, "shared")
		}
		if !p.RSTP && p.Enabled {
			flags = append(flags, "stp")
		}
		if p.RootInconsistent {
			flags = append(flags, "root-inconsistent")
		}
		role := p.Role
		if !p.Enabled {
			role, p.State = "disabled", "down"
		}
		if !detail {
			fmt.Fprintf(c.out, "%-12s %-8s %-11s %-11s %-9d %-24s %s\n", p.Name, p.PortID, role, p.State, p.Cost, p.DesignatedBridge, strings.Join(flags, ","))
			continue
		}
		fmt.Fprintf(c.out, "%s: %s %s, port id %s, cost %d\n", p.Name, role, p.State, p.PortID, p.Cost)
		fmt.Fprintf(c.out, "  designated bridge %s port %s\n", p.DesignatedBridge, p.DesignatedPort)
		proto := "rstp"
		if !p.RSTP {
			proto = "stp (802.1D neighbour)"
		}
		link := "point-to-point"
		if !p.P2P {
			link = "shared"
		}
		fmt.Fprintf(c.out, "  protocol %s, link %s, flags %s\n", proto, link, orDash(strings.Join(flags, ",")))
		fmt.Fprintf(c.out, "  BPDUs received %d, sent %d\n", p.Rx, p.Tx)
	}
	if name != "" && !found {
		return fmt.Errorf("%s is not an RSTP port", name)
	}
	return nil
}


// showSTPStatistics is "show spanning-tree statistics".
func (sh *Shell) showSTPStatistics(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	st, err := sh.stp()
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Topology changes: %d\n\n%-12s %12s %12s\n", st.Changes, "Interface", "BPDUs rx", "BPDUs tx")
	for _, p := range st.Ports {
		fmt.Fprintf(c.out, "%-12s %12d %12d\n", p.Name, p.Rx, p.Tx)
	}
	return nil
}

func completeSTPInterface(sh *Shell, args []config.Token, partial string) []Completion {
	out := []Completion{enter}
	if len(args) == 0 {
		out = append(out, filter([]Completion{{Text: "detail", Help: "Protocol details and counters"}}, partial)...)
		if st, err := sh.stp(); err == nil {
			for _, p := range st.Ports {
				if strings.HasPrefix(p.Name, partial) {
					out = append(out, Completion{Text: p.Name, Help: p.Role + " " + p.State})
				}
			}
		}
	} else if len(args) == 1 {
		out = append(out, filter([]Completion{{Text: "detail", Help: "Protocol details and counters"}}, partial)...)
	}
	return out
}

func stpCommand() *command {
	return &command{name: "spanning-tree", help: "Show the spanning tree (RSTP)", class: commit.ReadOnly, sub: []*command{
		{name: "bridge", help: "Bridge and root", class: commit.ReadOnly, run: (*Shell).showSTPBridge},
		{name: "interface", help: "Port roles and states", class: commit.ReadOnly, run: (*Shell).showSTPInterface, complete: completeSTPInterface},
		{name: "statistics", help: "BPDU counters", class: commit.ReadOnly, run: (*Shell).showSTPStatistics},
	}}
}
