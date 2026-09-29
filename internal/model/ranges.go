package model

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"mclag/internal/config"
	"mclag/internal/schema"
)

// maxRangePorts bounds member-range expansion.
const maxRangePorts = 4096

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
	for id := 1; id <= 16; id++ {
		ports, known := b.inv.Ports(id)
		if !known {
			continue
		}
		for _, linux := range sortedKeys(ports) {
			if ports[linux].StackPort || b.reservedPort(id, linux) {
				continue // never swallowed by wildcards
			}
			for _, p := range patterns {
				mem, glob, _ := strings.Cut(p, "/")
				if mem != "*" && mem != strconv.Itoa(id) {
					continue
				}
				if ok, _ := path.Match(glob, linux); ok {
					add(fmt.Sprintf("%d/%s", id, linux))
				}
			}
		}
	}
	return out
}

// reservedPort reports whether a port is a member's dedicated management or
// underlay port.
func (b *builder) reservedPort(member int, linux string) bool {
	for _, e := range b.root.Get("stack").Entries("member") {
		if e.Key != strconv.Itoa(member) {
			continue
		}
		return e.Leaf("management", "interface") == linux || e.Leaf("underlay", "interface") == linux
	}
	return false
}

// expandMemberRange expands "1/eth0" .. "1/eth23".
func expandMemberRange(from, to string) ([]string, error) {
	if to == "" {
		return nil, fmt.Errorf("'to' is required")
	}
	m1, l1, _ := schema.SplitPhysical(from)
	m2, l2, _ := schema.SplitPhysical(to)
	if m1 != m2 {
		return nil, fmt.Errorf("both ends must be on the same member")
	}
	p1, n1, w1, ok1 := splitNum(l1)
	p2, n2, _, ok2 := splitNum(l2)
	if !ok1 || !ok2 || p1 != p2 {
		return nil, fmt.Errorf("%s and %s must share a name prefix and end in a number", from, to)
	}
	if n2 < n1 {
		return nil, fmt.Errorf("%s comes before %s", to, from)
	}
	if n2-n1+1 > maxRangePorts {
		return nil, fmt.Errorf("range covers more than %d ports", maxRangePorts)
	}
	var out []string
	for n := n1; n <= n2; n++ {
		num := strconv.Itoa(n)
		// Keep zero padding such as eth00..eth23.
		if w1 > len(strconv.Itoa(n1)) {
			num = fmt.Sprintf("%0*d", w1, n)
		}
		name := p1 + num
		if len(name) > 15 {
			return nil, fmt.Errorf("%s is not a valid Linux interface name", name)
		}
		out = append(out, fmt.Sprintf("%d/%s", m1, name))
	}
	return out, nil
}

func splitNum(s string) (prefix string, n, width int, ok bool) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return "", 0, 0, false
	}
	v, err := strconv.Atoi(s[i:])
	if err != nil {
		return "", 0, 0, false
	}
	return s[:i], v, len(s) - i, true
}
