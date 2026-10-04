package cli

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

// RIB is implemented by members that run cer-ribd: "show route" in full
// (reference 5.14), from the routing table rather than the kernel.
type RIB interface {
	RIB(q rib.Query) ([]rib.Entry, error)
	RIBSummary() ([]rib.Summary, error)
}

// routeArgs are the parsed arguments of "show route".
type routeArgs struct {
	q        rib.Query
	instance string // "": default; "all": every instance
	table    string // a table name ("": by instance)
	view     string // "", terse, detail, extensive, summary
	hidden   bool
}

// routeProtocols are the protocol names "show route protocol" takes.
var routeProtocols = []Completion{
	{Text: "direct", Help: "Directly connected networks"}, {Text: "local", Help: "The switch's own addresses"},
	{Text: "static", Help: "Static routes"}, {Text: "ospf", Help: "OSPF"}, {Text: "ospf3", Help: "OSPFv3"},
	{Text: "bgp", Help: "BGP"}, {Text: "dhcp", Help: "Default routes of DHCP leases"},
}

var routeWords = []Completion{
	{Text: "exact", Help: "Only this prefix"}, {Text: "longer", Help: "Only more specific prefixes"},
	{Text: "protocol", Help: "Routes of one protocol"}, {Text: "next-hop", Help: "Routes through a next hop"},
	{Text: "active-path", Help: "Active routes only"}, {Text: "hidden", Help: "Routes not usable (rejected by import policy, unresolvable next hop)"},
	{Text: "terse", Help: "One line per route"}, {Text: "detail", Help: "Everything about each route"},
	{Text: "extensive", Help: "Everything about each route, with internals"}, {Text: "summary", Help: "Routes per table and protocol"},
	{Text: "table", Help: "One routing table (inet.0, inet6.0, <instance>.inet.0)"},
	{Text: "instance", Help: "A routing instance's tables (all: every instance)"},
}

func parseRouteArgs(c *call) (routeArgs, error) {
	var a routeArgs
	views := []string{"terse", "detail", "extensive", "summary", "brief"}
	for i := 0; i < len(c.args); i++ {
		w := c.args[i].Text
		next := func(what string) (string, error) {
			if i+1 >= len(c.args) {
				return "", &posError{pos: c.argPos(i + 1), msg: "expecting " + what}
			}
			i++
			return c.args[i].Text, nil
		}
		switch {
		case w == "exact" || w == "longer":
			a.q.Match = w
		case prefixOf(w, "protocol"):
			v, err := next("a protocol (" + completionTexts(routeProtocols) + ")")
			if err != nil {
				return a, err
			}
			p, ok := rib.ParseProtocol(v)
			if !ok {
				return a, &posError{pos: c.argPos(i), msg: "unknown protocol, expecting " + completionTexts(routeProtocols)}
			}
			a.q.Protocol = &p
		case prefixOf(w, "next-hop"):
			v, err := next("an address")
			if err != nil {
				return a, err
			}
			ip, perr := netip.ParseAddr(v)
			if perr != nil {
				return a, &posError{pos: c.argPos(i), msg: "expecting an address"}
			}
			a.q.NextHop = ip
		case prefixOf(w, "active-path"):
			a.q.Active = true
		case prefixOf(w, "hidden"):
			a.hidden = true
		case slices.ContainsFunc(views, func(v string) bool { return prefixOf(w, v) && len(w) >= 2 }):
			for _, v := range views {
				if prefixOf(w, v) {
					a.view = v
					break
				}
			}
			if a.view == "brief" {
				a.view = ""
			}
		case prefixOf(w, "table"):
			v, err := next("a table name (inet.0, inet6.0, <instance>.inet.0)")
			if err != nil {
				return a, err
			}
			if _, ok := parseTable(v); !ok {
				return a, &posError{pos: c.argPos(i), msg: "unknown table name, expecting inet.0, inet6.0 or <instance>.inet.0/.inet6.0"}
			}
			a.table = v
		case prefixOf(w, "instance"):
			v, err := next("a routing instance name or all")
			if err != nil {
				return a, err
			}
			a.instance = v
		default:
			p, err := parseRoutePrefix(w)
			if err != nil || a.q.Prefix.IsValid() {
				return a, &posError{pos: c.argPos(i), msg: "syntax error, expecting a prefix or address, or one of " + completionTexts(routeWords)}
			}
			a.q.Prefix = p
		}
	}
	if a.q.Match != "" && !a.q.Prefix.IsValid() {
		return a, errors.New("'exact' and 'longer' need a prefix")
	}
	switch {
	case a.table != "":
		t, _ := parseTable(a.table)
		a.q.Tables = []rib.Table{t}
	case a.instance == "all":
	default:
		a.q.Tables = []rib.Table{{Instance: a.instance}, {Instance: a.instance, V6: true}}
	}
	return a, nil
}

// parseRoutePrefix reads an address (a host route lookup) or a prefix.
func parseRoutePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(ip, ip.BitLen()), nil
}

// parseTable reads a Junos table name.
func parseTable(s string) (rib.Table, bool) {
	inst, fam := "", s
	if i := strings.LastIndex(s, ".inet"); i > 0 {
		inst, fam = s[:i], s[i+1:]
	}
	switch fam {
	case "inet.0":
		return rib.Table{Instance: inst}, true
	case "inet6.0":
		return rib.Table{Instance: inst, V6: true}, true
	}
	return rib.Table{}, false
}

func completionTexts(cs []Completion) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Text
	}
	return strings.Join(out, ", ")
}

// showRoute implements "show route" (reference 5.14).
func (sh *Shell) showRoute(c *call) error {
	if len(c.args) == 1 && prefixOf(c.args[0].Text, "instance") {
		return sh.showRouteInstances(c)
	}
	r, ok := sh.env.Ops.(RIB)
	if sh.env.Ops == nil || !ok {
		return sh.showKernelRoutes(c)
	}
	a, err := parseRouteArgs(c)
	if err != nil {
		return err
	}
	sums, serr := r.RIBSummary()
	if a.view == "summary" {
		if serr != nil {
			return serr
		}
		writeRouteSummary(c.out, sums, a)
		return nil
	}
	if a.hidden {
		// Nothing is hidden yet: routes rejected by import policy or with an
		// unresolvable next hop come with BGP.
		return nil
	}
	es, err := r.RIB(a.q)
	if err != nil {
		return err
	}
	writeRoutes(c.out, es, sums, a.view, time.Now())
	return nil
}

// tableHeader is Junos' first line of a table.
func tableHeader(t rib.Table, sums []rib.Summary, es []rib.Entry) string {
	for _, s := range sums {
		if s.Table == t {
			return fmt.Sprintf("%s: %d destinations, %d routes (%d active, 0 holddown, 0 hidden)", t.Name(), s.Destinations, s.Routes, s.Active)
		}
	}
	routes, active := 0, 0
	for _, e := range es {
		routes += len(e.Routes)
		if e.Active >= 0 {
			active++
		}
	}
	return fmt.Sprintf("%s: %d destinations, %d routes (%d active, 0 holddown, 0 hidden)", t.Name(), len(es), routes, active)
}

// writeRoutes prints the entries table by table.
func writeRoutes(out *strings.Builder, es []rib.Entry, sums []rib.Summary, view string, now time.Time) {
	var tables []rib.Table
	by := map[rib.Table][]rib.Entry{}
	for _, e := range es {
		if _, ok := by[e.Table]; !ok {
			tables = append(tables, e.Table)
		}
		by[e.Table] = append(by[e.Table], e)
	}
	for i, t := range tables {
		if i > 0 {
			out.WriteString("\n")
		}
		fmt.Fprintf(out, "%s\n", tableHeader(t, sums, by[t]))
		if view != "detail" && view != "extensive" {
			out.WriteString("+ = Active Route, - = Last Active, * = Both\n")
		}
		out.WriteString("\n")
		switch view {
		case "terse":
			writeTerse(out, by[t])
		case "detail", "extensive":
			for _, e := range by[t] {
				writeDetail(out, e, view == "extensive", now)
			}
		default:
			for _, e := range by[t] {
				writeBrief(out, e, now)
			}
		}
	}
}

// protoName is the protocol as Junos shows it.
func protoName(p rib.Protocol) string { return p.String() }

// routeAge is Junos' age format: 00:10:00, 1d 02:03:04, 2w3d 04:05:06.
func routeAge(d time.Duration) string {
	d = max(d.Round(time.Second), 0)
	days := int(d.Hours()) / 24
	hms := fmt.Sprintf("%02d:%02d:%02d", int(d.Hours())%24, int(d.Minutes())%60, int(d.Seconds())%60)
	switch {
	case days >= 7:
		return fmt.Sprintf("%dw%dd %s", days/7, days%7, hms)
	case days > 0:
		return fmt.Sprintf("%dd %s", days, hms)
	}
	return hms
}

// nextHopLines are a route's next hop lines (">" marks the ones in use).
func nextHopLines(r rib.Route) []string {
	switch {
	case r.Discard:
		return []string{"   Discard"}
	case r.Protocol == rib.Local:
		var out []string
		for _, h := range r.NextHops {
			out = append(out, "   Local via "+h.Interface)
		}
		return out
	}
	var out []string
	for _, h := range r.NextHops {
		out = append(out, ">  "+h.String())
	}
	return out
}

func metricText(r rib.Route) string {
	var parts []string
	switch r.Protocol {
	case rib.Direct, rib.Local:
	case rib.Static:
		if r.Metric != 0 {
			parts = append(parts, fmt.Sprintf("metric %d", r.Metric))
		}
	case rib.BGP:
		if r.Metric != 0 {
			parts = append(parts, fmt.Sprintf("MED %d", r.Metric))
		}
		if a := r.Attrs; a != nil && a.LocalPref != nil {
			parts = append(parts, fmt.Sprintf("localpref %d", *a.LocalPref))
		}
	default:
		parts = append(parts, fmt.Sprintf("metric %d", r.Metric))
		if r.Metric2 != 0 {
			parts = append(parts, fmt.Sprintf("metric2 %d", r.Metric2))
		}
		if a := r.Attrs; a != nil && strings.HasPrefix(a.PathType, "Ext") {
			parts = append(parts, fmt.Sprintf("tag %d", a.Tag))
		}
	}
	return strings.Join(parts, ", ")
}

// writeBrief is the default view.
func writeBrief(out *strings.Builder, e rib.Entry, now time.Time) {
	dst := e.Prefix.String()
	const col = 19
	first := true
	for i, r := range e.Routes {
		mark := " "
		if i == e.Active {
			mark = "*"
		}
		line := fmt.Sprintf("%s[%s/%d] %s", mark, protoName(r.Protocol), r.Preference, routeAge(now.Sub(r.Since)))
		if m := metricText(r); m != "" {
			line += ", " + m
		}
		if first {
			if len(dst) >= col {
				fmt.Fprintf(out, "%s\n%*s%s\n", dst, col, "", line)
			} else {
				fmt.Fprintf(out, "%-*s%s\n", col, dst, line)
			}
			first = false
		} else {
			fmt.Fprintf(out, "%*s%s\n", col, "", line)
		}
		if a := r.Attrs; a != nil && a.ASPath != "" {
			fmt.Fprintf(out, "%*sAS path: %s\n", col+3, "", a.ASPath)
		}
		for _, h := range nextHopLines(r) {
			fmt.Fprintf(out, "%*s%s\n", col+1, "", h)
		}
	}
}

// terseLetter is the protocol column of "terse".
func terseLetter(p rib.Protocol) string {
	switch p {
	case rib.Direct:
		return "D"
	case rib.Local:
		return "L"
	case rib.Static:
		return "S"
	case rib.OSPF, rib.OSPF3:
		return "O"
	case rib.BGP:
		return "B"
	case rib.DHCP:
		return "A"
	}
	return "?"
}

func writeTerse(out *strings.Builder, es []rib.Entry) {
	fmt.Fprintf(out, "A V Destination        P Prf   Metric 1   Metric 2  Next hop        AS path\n")
	for _, e := range es {
		for i, r := range e.Routes {
			act, dst := " ", ""
			if i == e.Active {
				act = "*"
			}
			if i == 0 {
				dst = e.Prefix.String()
			}
			nh := ""
			switch {
			case r.Discard:
				nh = "Discard"
			case r.Protocol == rib.Local:
				nh = "Local"
			case len(r.NextHops) > 0:
				h := r.NextHops[0]
				nh = ">" + h.Interface
				if h.Gateway.IsValid() {
					nh = ">" + h.Gateway.String()
				}
			}
			m1, m2 := "", ""
			if r.Protocol != rib.Direct && r.Protocol != rib.Local {
				m1 = fmt.Sprint(r.Metric)
				if r.Metric2 != 0 {
					m2 = fmt.Sprint(r.Metric2)
				}
			}
			asPath := ""
			if r.Attrs != nil {
				asPath = r.Attrs.ASPath
			}
			line := fmt.Sprintf("%s ? %-18s %s %3d %10s %10s  %-15s %s", act, dst, terseLetter(r.Protocol), r.Preference, m1, m2, nh, asPath)
			out.WriteString(strings.TrimRight(line, " ") + "\n")
			for _, h := range r.NextHops[min(1, len(r.NextHops)):] {
				x := ">" + h.Interface
				if h.Gateway.IsValid() {
					x = ">" + h.Gateway.String()
				}
				fmt.Fprintf(out, "%51s%s\n", "", x)
			}
		}
	}
}

// inactiveReason explains why r lost against the active route act.
func inactiveReason(r, act rib.Route) string {
	switch {
	case r.Preference != act.Preference:
		return "Route Preference"
	case r.Protocol != act.Protocol:
		return "Route Preference" // equal preference: the lower protocol id wins
	case r.Attrs != nil && r.Attrs.InactiveReason != "":
		return r.Attrs.InactiveReason
	case r.Metric != act.Metric, r.Metric2 != act.Metric2:
		return "Route Metric or MED comparison"
	}
	return "Not Best in its group"
}

func writeDetail(out *strings.Builder, e rib.Entry, extensive bool, now time.Time) {
	announced := 0
	if e.Active >= 0 {
		announced = 1
	}
	fmt.Fprintf(out, "%s (%d entr%s, %d announced)\n", e.Prefix, len(e.Routes), map[bool]string{true: "y", false: "ies"}[len(e.Routes) == 1], announced)
	for i, r := range e.Routes {
		mark := " "
		if i == e.Active {
			mark = "*"
		}
		fmt.Fprintf(out, "        %s%-7s Preference: %d\n", mark, protoName(r.Protocol), r.Preference)
		ind := "                "
		switch {
		case r.Discard:
			fmt.Fprintf(out, "%sNext hop type: Discard\n", ind)
		case r.Protocol == rib.Local:
			fmt.Fprintf(out, "%sNext hop type: Local\n", ind)
		default:
			for _, h := range r.NextHops {
				sel := ""
				if i == e.Active {
					sel = ", selected"
				}
				if h.Gateway.IsValid() {
					fmt.Fprintf(out, "%sNext hop: %s via %s%s\n", ind, h.Gateway, h.Interface, sel)
				} else {
					fmt.Fprintf(out, "%sNext hop: via %s%s\n", ind, h.Interface, sel)
				}
			}
		}
		state := "<Active>"
		if i != e.Active {
			state = "<NotBest>"
		}
		if r.Stale {
			state = strings.TrimSuffix(state, ">") + " Stale>"
		}
		fmt.Fprintf(out, "%sState: %s\n", ind, state)
		if i != e.Active && e.Active >= 0 {
			fmt.Fprintf(out, "%sInactive reason: %s\n", ind, inactiveReason(r, e.Routes[e.Active]))
		}
		age := fmt.Sprintf("%sAge: %s", ind, routeAge(now.Sub(r.Since)))
		if r.Protocol != rib.Direct && r.Protocol != rib.Local {
			age += fmt.Sprintf("    Metric: %d", r.Metric)
			if r.Metric2 != 0 {
				age += fmt.Sprintf("    Metric2: %d", r.Metric2)
			}
		}
		out.WriteString(age + "\n")
		if a := r.Attrs; a != nil {
			if a.Area != "" {
				fmt.Fprintf(out, "%sArea: %s\n", ind, a.Area)
			}
			if a.PathType != "" {
				fmt.Fprintf(out, "%sPath type: %s\n", ind, a.PathType)
				if strings.HasPrefix(a.PathType, "Ext") {
					fmt.Fprintf(out, "%sTag: %d\n", ind, a.Tag)
				}
			}
			if a.Peer != "" {
				fmt.Fprintf(out, "%sSource: %s\n", ind, a.Peer)
			}
			if a.PeerAS != 0 {
				fmt.Fprintf(out, "%sPeer AS: %d\n", ind, a.PeerAS)
			}
			if a.ASPath != "" {
				fmt.Fprintf(out, "%sAS path: %s\n", ind, a.ASPath)
			}
			if a.LocalPref != nil {
				fmt.Fprintf(out, "%sLocalpref: %d\n", ind, *a.LocalPref)
			}
			if len(a.Communities) > 0 {
				fmt.Fprintf(out, "%sCommunities: %s\n", ind, strings.Join(a.Communities, " "))
			}
			if a.Originator != "" {
				fmt.Fprintf(out, "%sOriginator ID: %s\n", ind, a.Originator)
			}
			if len(a.ClusterList) > 0 {
				fmt.Fprintf(out, "%sCluster list: %s\n", ind, strings.Join(a.ClusterList, " "))
			}
		}
		if extensive && r.Source != "" {
			fmt.Fprintf(out, "%sRoute source: %s\n", ind, r.Source)
		}
	}
	out.WriteString("\n")
}

func writeRouteSummary(out *strings.Builder, sums []rib.Summary, a routeArgs) {
	for _, s := range sums {
		if len(a.q.Tables) > 0 && !slices.Contains(a.q.Tables, s.Table) {
			continue
		}
		fmt.Fprintf(out, "\n%s: %d destinations, %d routes (%d active, 0 holddown, 0 hidden)\n", s.Table.Name(), s.Destinations, s.Routes, s.Active)
		protos := make([]rib.Protocol, 0, len(s.PerProtocol))
		for p := range s.PerProtocol {
			protos = append(protos, p)
		}
		slices.Sort(protos)
		for _, p := range protos {
			n := s.PerProtocol[p]
			fmt.Fprintf(out, "%20s: %6d routes, %6d active\n", protoName(p), n[0], n[1])
		}
	}
}

func completeRoute(sh *Shell, args []config.Token, partial string) []Completion {
	if n := len(args); n > 0 {
		switch prev := args[n-1].Text; {
		case prefixOf(prev, "protocol"):
			return filter(routeProtocols, partial)
		case prefixOf(prev, "instance"):
			out := []Completion{{Text: "all", Help: "Every routing instance"}}
			for _, e := range sh.env.Engine.Active().Root.Entries("routing-instances") {
				out = append(out, Completion{Text: e.Key, Help: "Routing instance"})
			}
			return filter(out, partial)
		case prefixOf(prev, "table"):
			out := []Completion{{Text: "inet.0", Help: "IPv4, default instance"}, {Text: "inet6.0", Help: "IPv6, default instance"}}
			for _, e := range sh.env.Engine.Active().Root.Entries("routing-instances") {
				out = append(out, Completion{Text: e.Key + ".inet.0", Help: "IPv4 of " + e.Key},
					Completion{Text: e.Key + ".inet6.0", Help: "IPv6 of " + e.Key})
			}
			return filter(out, partial)
		case prefixOf(prev, "next-hop"):
			return []Completion{{Text: "<address>", Help: "Next hop address"}}
		}
	}
	out := append([]Completion{enter, {Text: "<prefix>", Help: "An address (longest match) or a prefix"}}, routeWords...)
	return filter(out, partial)
}
