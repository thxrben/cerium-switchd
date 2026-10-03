package cli

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/schema"
)

// STPStatus is the stack's spanning tree as the RSTP owner sees it.
type STPStatus struct {
	Running                         bool
	Error                           string // configured but not running: why
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
	if !st.Running && st.Error != "" {
		return st, errors.New("RSTP is configured but does not run: " + st.Error)
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
		// Say why (reference 5.5: RSTP runs on switch ports and bundles).
		cfg := sh.activeModel()
		switch i := cfg.Interfaces[name]; {
		case i == nil:
			return fmt.Errorf("%s is not configured, so it runs no RSTP", name)
		case i.Parent != "":
			return fmt.Errorf("%s is a member of %s; RSTP runs on %s (see show spanning-tree interface %s)", name, i.Parent, i.Parent, i.Parent)
		case !i.Switching:
			return fmt.Errorf("%s is not a switch port (no 'unit 0 family ethernet-switching'); RSTP runs on switch ports only", name)
		}
		return fmt.Errorf("%s is not an RSTP port", name)
	}
	if name == "" && !found {
		c.out.WriteString("No RSTP ports: RSTP runs on switch ports (unit 0 family ethernet-switching) and bundles.\n")
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

// DHCPBinding is one DHCP client of this member.
type DHCPBinding struct {
	Unit, State, Address, Server, Router, Instance string
	DNS                                            []string
	Domain                                         string
	Lease                                          time.Duration
	Renew, Expires                                 time.Time
}

// showDHCPBinding is "show dhcp client binding [<interface>]".
func (sh *Shell) showDHCPBinding(c *call) error {
	name := ""
	switch len(c.args) {
	case 0:
	case 1:
		name = c.args[0].Text
	default:
		return &posError{pos: c.argPos(1), msg: "syntax error, expecting one interface"}
	}
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	bs, err := sh.env.Ops.DHCPBindings()
	if err := partial(c, err); err != nil {
		return err
	}
	bs = slices.DeleteFunc(bs, func(b DHCPBinding) bool { return !c.shows(b.Unit) })
	now := time.Now()
	n := 0
	for _, b := range bs {
		if name != "" && b.Unit != name {
			continue
		}
		n++
		inst := ""
		if b.Instance != "" {
			inst = " (instance " + b.Instance + ")"
		}
		fmt.Fprintf(c.out, "%s%s: %s\n", b.Unit, inst, b.State)
		if b.Address == "" {
			continue
		}
		fmt.Fprintf(c.out, "  address %s from server %s, router %s\n", b.Address, orDash(b.Server), orDash(b.Router))
		if len(b.DNS) > 0 || b.Domain != "" {
			fmt.Fprintf(c.out, "  DNS servers %s, domain %s\n", orDash(strings.Join(b.DNS, " ")), orDash(b.Domain))
		}
		fmt.Fprintf(c.out, "  lease %s, renewal in %s, expires in %s\n", b.Lease, b.Renew.Sub(now).Round(time.Second), b.Expires.Sub(now).Round(time.Second))
	}
	if n == 0 {
		if name != "" {
			return fmt.Errorf("%s does not use DHCP", name)
		}
		c.out.WriteString("no interface uses DHCP\n")
	}
	return nil
}

// CardStatus is a known card of this member.
type CardStatus struct {
	Member    int
	Number    int
	Key       string
	Present   bool
	Driver    string
	Ports     int
	LastSeen  time.Time
	Note      string
	MovedFrom int // -1: none
}

// showCards is the card part of "show chassis hardware".
func (sh *Shell) showCards(c *call) {
	cards, err := sh.env.Ops.Cards()
	if err != nil && !errors.As(err, new(*PartialError)) {
		return
	}
	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].Member != cards[j].Member {
			return cards[i].Member < cards[j].Member
		}
		return cards[i].Number < cards[j].Number
	})
	var absent, notes []string
	for _, cd := range cards {
		member := cd.Member
		if c.only != nil && !slices.Contains(c.only, member) {
			continue
		}
		name := fmt.Sprintf("%d/%d", member, cd.Number)
		if !cd.Present {
			seen := "never"
			if !cd.LastSeen.IsZero() {
				seen = cd.LastSeen.Local().Format("2006-01-02 15:04")
			}
			absent = append(absent, fmt.Sprintf("  %-7s %-16s %-12s %d ports, last seen %s", name, cd.Key, orDash(cd.Driver), cd.Ports, seen))
		}
		if cd.Note != "" {
			notes = append(notes, fmt.Sprintf("  card %s (%s) %s", name, cd.Key, cd.Note))
		}
		if cd.MovedFrom >= 0 {
			notes = append(notes, fmt.Sprintf("  card %s (%s) has the MAC addresses of absent card %d/%d: it moved slots; "+
				"'request chassis card %d renumber %d' gives it its old number", name, cd.Key, member, cd.MovedFrom, cd.Number, cd.MovedFrom))
		}
	}
	if len(absent) > 0 {
		c.out.WriteString("\nCards not present (numbers reserved):\n" + strings.Join(absent, "\n") + "\n")
	}
	if len(notes) > 0 {
		c.out.WriteString("\nCard changes:\n" + strings.Join(notes, "\n") + "\n")
	}
}

// chassisCard is "request chassis card <n> renumber <m>" and "... forget".
func (sh *Shell) chassisCard(c *call) error {
	num := func(i int) (int, error) {
		v, err := strconv.Atoi(c.args[i].Text)
		if err != nil || v < 0 || v > schema.MaxCard {
			return 0, &posError{pos: c.argPos(i), msg: fmt.Sprintf("expecting a card number (0-%d)", schema.MaxCard)}
		}
		return v, nil
	}
	var from, to int
	var err error
	switch {
	case len(c.args) == 3 && prefixOf(c.args[1].Text, "renumber"):
		if from, err = num(0); err != nil {
			return err
		}
		if to, err = num(2); err != nil {
			return err
		}
	case len(c.args) == 2 && prefixOf(c.args[1].Text, "forget"):
		if from, err = num(0); err != nil {
			return err
		}
		to = -1
	default:
		return &posError{pos: c.argPos(0), msg: "syntax error, expecting '<card> renumber <card>' or '<card> forget'"}
	}
	if sh.env.Ops == nil {
		return errors.New("not available")
	}
	nums := []int{from}
	if to >= 0 {
		nums = append(nums, to)
	}
	affected := sh.env.Ops.CardInterfaces(nums...)
	q := fmt.Sprintf("Release the number of absent card %d", from)
	if to >= 0 {
		q = fmt.Sprintf("Renumber card %d to %d (its ports are renamed)", from, to)
	}
	if len(affected) > 0 {
		q += "; configured interfaces affected: " + strings.Join(affected, ", ")
	}
	a, err := c.term.Ask(q+" ? [yes,no] (no) ", true)
	if err != nil || !isYes(a) {
		return nil
	}
	if to >= 0 {
		err = sh.env.Ops.CardRenumber(from, to, sh.env.User)
	} else {
		err = sh.env.Ops.CardForget(from, sh.env.User)
	}
	if err != nil {
		return err
	}
	c.out.WriteString("done\n")
	return nil
}
