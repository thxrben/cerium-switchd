package model

import (
	"fmt"
	"slices"

	"github.com/thxrben/cerium-switchd/lib/conf/config"
	"github.com/thxrben/cerium-switchd/lib/conf/schema"
)

// effectiveInterfaces returns one node per interface: explicit entries
// under "interfaces", completed with the statements of the interface-range
// that matches them, plus interfaces that exist only through a range.
func (b *builder) effectiveInterfaces() []*config.Node {
	r := b.root
	out := map[string]*config.Node{}
	var order []string
	for _, e := range r.Entries("interfaces") {
		out[e.Key] = e
		order = append(order, e.Key)
	}
	claimed := map[string]string{}
	for _, rg := range r.Entries("interface-range") {
		rpath := "interface-range " + rg.Key
		targets := b.rangeTargets(rg, rpath)
		if len(targets) == 0 && (b.inv != nil || len(rg.List("member")) == 0) {
			b.warnf(rpath, "matches no interfaces")
		}
		// The template is the range body without its selectors.
		tmpl := &config.Node{Schema: rg.Schema}
		for _, k := range rg.Kids {
			if k.Schema.Name != "member" && k.Schema.Name != "member-range" {
				tmpl.Kids = append(tmpl.Kids, k)
			}
		}
		for _, t := range targets {
			if o, dup := claimed[t]; dup {
				b.errorf(rpath, "%s is already part of interface-range %s", t, o)
				continue
			}
			claimed[t] = rg.Key
			b.rangeOf[t] = rg.Key
			n, ok := out[t]
			if ok {
				// Never modify the configuration tree itself.
				n = cloneNode(n)
			} else {
				n = &config.Node{Schema: schema.Root().Child("interfaces"), Key: t}
				order = append(order, t)
			}
			n.MergeDefaults(tmpl)
			out[t] = n
		}
	}
	nodes := make([]*config.Node, 0, len(order))
	for _, k := range order {
		nodes = append(nodes, out[k])
	}
	return nodes
}

func cloneNode(n *config.Node) *config.Node {
	t := &config.Tree{Root: &config.Node{Schema: schema.Root(), Kids: []*config.Node{n}}}
	return t.Clone().Root.Kids[0]
}

// rangeTargets expands member-range entries and wildcard patterns.
func (b *builder) rangeTargets(rg *config.Node, rpath string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, mr := range rg.Entries("member-range") {
		names, err := expandMemberRange(mr.Key, mr.Leaf("to"))
		if err != nil {
			b.errorf(rpath+" member-range "+mr.Key, "%v", err)
			continue
		}
		for _, n := range names {
			add(n)
		}
	}
	patterns := rg.List("member")
	if len(patterns) == 0 {
		return out
	}
	if b.inv == nil {
		return out
	}
	var pats []schema.PortPattern
	for _, p := range patterns {
		pp, err := schema.ParsePortPattern(p)
		if err != nil {
			b.errorf(rpath+" member", "%v", err)
			continue
		}
		pats = append(pats, pp)
	}
	for id := 1; id <= 16; id++ {
		ports, known := b.inv.Ports(id)
		if !known {
			continue
		}
		for _, name := range sortedPortNames(ports) {
			if ports[name].StackPort || b.reservedPort(id, name) {
				continue // never swallowed by wildcards
			}
			port, ok := schema.ParsePhysical(name)
			if !ok {
				continue
			}
			for _, pp := range pats {
				if pp.Match(port) {
					add(name)
					break
				}
			}
		}
	}
	return out
}

// reservedPort reports whether a port is a management port.
func (b *builder) reservedPort(member int, name string) bool {
	if e := b.root.Entry("interfaces", name); e != nil && e.Has("management") {
		return true
	}
	return false
}

// sortedPortNames returns port names in numeric order.
func sortedPortNames(ports map[string]PortInfo) []string {
	names := sortedKeys(ports)
	slices.SortFunc(names, func(a, b string) int {
		if config.NaturalLess(a, b) {
			return -1
		}
		if config.NaturalLess(b, a) {
			return 1
		}
		return 0
	})
	return names
}

// expandMemberRange expands "1/0/0" .. "1/0/23".
func expandMemberRange(from, to string) ([]string, error) {
	if to == "" {
		return nil, fmt.Errorf("'to' is required")
	}
	a, ok1 := schema.ParsePhysical(from)
	z, ok2 := schema.ParsePhysical(to)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("both ends must be physical ports")
	}
	if a.Member != z.Member || a.Card != z.Card {
		return nil, fmt.Errorf("both ends must be on the same member and card")
	}
	if z.Port < a.Port {
		return nil, fmt.Errorf("%s comes before %s", to, from)
	}
	var out []string
	for n := a.Port; n <= z.Port; n++ {
		out = append(out, schema.Port{Member: a.Member, Card: a.Card, Port: n}.String())
	}
	return out, nil
}
