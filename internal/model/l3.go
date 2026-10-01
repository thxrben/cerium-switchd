package model

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"

	"mclag/internal/config"
	"mclag/internal/schema"
)

// L3Unit is a routed interface of the default instance: an irb unit, a
// routed port (unit 0) or a routed subinterface (reference 5.3.2, 5.3.3).
type L3Unit struct {
	Name        string // "irb.10", "1/0/6.0", "1/0/6.100", "ae1.0"
	Parent      string // "irb", "1/0/6", "ae1"
	Unit        int
	Member      int // physical parent's member (0: irb or ae, on every member that has it)
	Tag         int // 802.1Q tag of a subinterface (0 = untagged)
	VLAN        int // irb: the id of the VLAN it is attached to (0 = none)
	Disabled    bool
	Description string
	Addrs       []netip.Prefix // IPv4 and IPv6
	// AddrMember restricts an address to one member (irb units; missing:
	// every member).
	AddrMember map[netip.Prefix]int
	Instance   string // routing instance ("" = default)
	DHCP       bool   // family inet dhcp: the IPv4 address comes from a lease
}

// AddrsOn returns the addresses of u that exist on member m.
func (u *L3Unit) AddrsOn(m int) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range u.Addrs {
		if id, ok := u.AddrMember[p]; !ok || id == m {
			out = append(out, p)
		}
	}
	return out
}

// OnMember reports whether u exists on member m (for irb: it has an
// address there, or addresses for every member).
func (u *L3Unit) OnMember(m int) bool {
	if u.Member != 0 {
		return u.Member == m
	}
	return len(u.Addrs) == 0 || len(u.AddrsOn(m)) > 0
}

// RoutingInstance is a separate routing table (reference 5.9).
type RoutingInstance struct {
	Name        string
	Description string
	Units       []string
	Routes      []StaticRoute
}

// MgmtUnits returns the units of the management instance (reference 1.8).
func (c *Config) MgmtUnits() []*L3Unit {
	var out []*L3Unit
	if in := c.Instances[c.System.MgmtInstance]; in != nil {
		for _, n := range in.Units {
			if u := c.L3[n]; u != nil {
				out = append(out, u)
			}
		}
	}
	return out
}

// MgmtPorts returns the management ports of member m, sorted by name.
func (c *Config) MgmtPorts(m int) []string {
	var out []string
	for _, i := range c.Interfaces {
		if i.Management && i.Member == m {
			out = append(out, i.Name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return config.NaturalLess(out[i], out[j]) })
	return out
}

// IRB reports whether u is a VLAN IP interface.
func (u *L3Unit) IRB() bool { return u.Parent == "irb" }

// CME reports whether u is the chassis management interface.
func (u *L3Unit) CME() bool { return u.Parent == schema.CME }

// StaticRoute is a route of the default instance.
type StaticRoute struct {
	Prefix   netip.Prefix
	NextHops []netip.Addr
	Discard  bool
}

// buildUnits collects the routed units of one interface node (physical, ae
// or irb) and checks the unit rules.
func (b *builder) buildUnits(name string, e *config.Node, i *Interface) {
	path := "interfaces " + name
	tagging := e.Has("vlan-tagging")
	if i != nil {
		i.VlanTagging = tagging
	}
	tags := map[int]string{}
	for _, u := range e.Entries("unit") {
		n, _ := strconv.Atoi(u.Key)
		upath := path + " unit " + u.Key
		fam := u.Get("family")
		sw := fam.Get("ethernet-switching") != nil
		routed := fam.Get("inet") != nil || fam.Get("inet6") != nil
		tag := atoi(u.Leaf("vlan-id"), 0)
		switch {
		case name == "irb":
			if sw {
				b.errorf(upath+" family ethernet-switching", "irb units are IP interfaces; switching is configured on ports")
			}
			if tag != 0 {
				b.errorf(upath+" vlan-id", "an irb unit is attached to a VLAN with 'vlans <v> l3-interface irb.%d'", n)
			}
		case name == schema.CME:
			if n != 0 {
				b.errorf(upath, "cme has only unit 0")
			}
			if sw || tag != 0 {
				b.errorf(upath, "cme is an IP interface: only addresses (family inet|inet6 address) are valid")
			}
			if fam.Has("inet", "dhcp") {
				b.errorf(upath+" family inet dhcp", "cme takes static addresses only")
			}
		case sw && routed:
			b.errorf(upath+" family", "a unit is either switched (ethernet-switching) or routed (inet/inet6), not both")
		case sw && n != 0:
			b.errorf(upath+" family ethernet-switching", "ethernet-switching is only valid on unit 0")
		case sw && tagging:
			b.errorf(path+" vlan-tagging", "vlan-tagging is for routed subinterfaces; a switch port uses interface-mode trunk")
		case n != 0 && !tagging:
			b.errorf(upath, "units other than 0 are routed subinterfaces and need 'vlan-tagging' on the interface")
		case n != 0 && tag == 0:
			b.errorf(upath, "a routed subinterface needs a vlan-id")
		case tag != 0 && !tagging:
			b.errorf(upath+" vlan-id", "vlan-id needs 'vlan-tagging' on the interface")
		}
		if tag != 0 {
			if o, dup := tags[tag]; dup {
				b.errorf(upath+" vlan-id", "vlan-id %d is already used by unit %s", tag, o)
			}
			tags[tag] = u.Key
		}
		if !routed && name != "irb" {
			continue
		}
		if i != nil && i.Parent != "" {
			b.errorf(upath+" family", "%s is a member of %s; configure routing on %s", name, i.Parent, i.Parent)
			continue
		}
		l3 := &L3Unit{Name: fmt.Sprintf("%s.%d", name, n), Parent: name, Unit: n, Tag: tag,
			Disabled: u.Has("disable") || (i != nil && i.Disabled), Description: u.Leaf("description")}
		if i != nil {
			l3.Member = i.Member
		}
		for _, f := range []string{"inet", "inet6"} {
			for _, ae := range fam.Get(f).Entries("address") {
				a := ae.Key
				p, err := netip.ParsePrefix(a)
				if err != nil {
					continue // the schema already rejected it
				}
				if mid := ae.Leaf("member"); mid != "" {
					id := atoi(mid, 0)
					switch {
					case name != "irb":
						b.errorf(upath+" family "+f+" address "+a+" member", "member applies to irb addresses only (a port belongs to one member already)")
						continue
					case b.cfg.Members[id] == nil:
						b.errorf(upath+" family "+f+" address "+a+" member", "virtual-chassis member %d is not configured", id)
						continue
					}
					if l3.AddrMember == nil {
						l3.AddrMember = map[netip.Prefix]int{}
					}
					l3.AddrMember[p] = id
				}
				if (f == "inet") != p.Addr().Is4() {
					b.errorf(upath+" family "+f+" address", "%s is not an address of family %s", a, f)
					continue
				}
				if msg := hostAddressProblem(p); msg != "" {
					b.errorf(upath+" family "+f+" address", "%s %s", a, msg)
					continue
				}
				l3.Addrs = append(l3.Addrs, p)
			}
		}
		if fam.Has("inet", "dhcp") {
			l3.DHCP = true
			for _, p := range l3.Addrs {
				if p.Addr().Is4() {
					b.errorf(upath+" family inet dhcp", "use either 'dhcp' or a static IPv4 address, not both")
					break
				}
			}
		}
		b.cfg.L3[l3.Name] = l3
	}
}

// buildIRB builds the units of irb or cme. Only units exist on them.
func (b *builder) buildIRB(e *config.Node) {
	for _, k := range e.Kids {
		if k.Schema.Name != "unit" && k.Schema.Name != "description" {
			b.errorf("interfaces "+e.Key+" "+k.Schema.Name, "only 'unit' (and 'description') is valid on %s", e.Key)
		}
	}
	b.buildUnits(e.Key, e, nil)
}

// hostAddressProblem rejects network and broadcast addresses as interface
// addresses.
func hostAddressProblem(p netip.Prefix) string {
	bits := p.Addr().BitLen()
	if p.Bits() >= bits-1 {
		return "" // /31, /32, /127, /128: every address is usable
	}
	if p.Addr() == p.Masked().Addr() {
		return "is the network address of its subnet"
	}
	if p.Addr().Is4() {
		b := p.Addr().As4()
		v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		if host := uint32(1)<<(32-p.Bits()) - 1; v&host == host {
			return "is the broadcast address of its subnet"
		}
	}
	return ""
}

func (b *builder) buildRoutes(ro *config.Node, base string) []StaticRoute {
	var out []StaticRoute
	for _, e := range ro.Get("static").Entries("route") {
		path := base + " static route " + e.Key
		p, err := netip.ParsePrefix(e.Key)
		if err != nil {
			continue
		}
		r := StaticRoute{Prefix: p, Discard: e.Has("discard")}
		for _, h := range e.List("next-hop") {
			a, err := netip.ParseAddr(h)
			if err != nil {
				continue
			}
			if a.Is4() != p.Addr().Is4() {
				b.errorf(path+" next-hop", "next hop %s is not of the prefix's address family", h)
				continue
			}
			r.NextHops = append(r.NextHops, a)
		}
		switch {
		case r.Discard && len(e.List("next-hop")) > 0:
			b.errorf(path, "'discard' and 'next-hop' are mutually exclusive")
		case !r.Discard && len(e.List("next-hop")) == 0:
			b.errorf(path, "needs 'next-hop' or 'discard'")
		}
		out = append(out, r)
	}
	return out
}

// buildInstances builds the routing instances and assigns units to them.
func (b *builder) buildInstances() {
	c := b.cfg
	owner := map[string]string{}
	for _, e := range b.root.Entries("routing-instances") {
		path := "routing-instances " + e.Key
		in := &RoutingInstance{Name: e.Key, Description: e.Leaf("description")}
		in.Routes = b.buildRoutes(e.Get("routing-options"), path+" routing-options")
		for _, n := range e.List("interface") {
			u := c.L3[n]
			switch {
			case u == nil:
				b.errorf(path+" interface", "%s is not a routed interface (it needs family inet or inet6)", n)
				continue
			case owner[n] != "":
				b.errorf(path+" interface", "%s is already in routing instance %s", n, owner[n])
				continue
			}
			owner[n] = e.Key
			u.Instance = e.Key
			in.Units = append(in.Units, n)
		}
		c.Instances[e.Key] = in
		// The instance is a kernel VRF device of this name on every member.
		for _, id := range sortedKeys(c.Members) {
			ports, _ := b.portsOf(id)
			for name, p := range ports {
				if p.Linux == e.Key {
					b.errorf(path, "%s is the kernel name of port %s; choose another instance name", e.Key, name)
				}
			}
		}
	}
	// The management instance (reference 1.8, 5.1).
	mi := c.System.MgmtInstance
	in := c.Instances[mi]
	if mi != "" && in == nil {
		b.errorf("system management-instance", "routing instance %s is not configured", mi)
	}
	if u := c.L3[schema.CME+".0"]; u != nil && (mi == "" || u.Instance != mi) {
		b.errorf("interfaces cme", "cme.0 must be in the management instance (routing-instances <instance> interface cme.0 and system management-instance <instance>)")
	}
	if in != nil {
		if len(in.Units) == 0 {
			b.warnf("routing-instances "+mi, "the management instance has no interface; the stack has no management address")
		}
		for _, u := range c.MgmtUnits() {
			if len(u.AddrMember) > 0 {
				b.errorf(unitPath(u)+" address", "%s is in the management instance: its addresses belong to the master, 'member' is not valid", u.Name)
			}
		}
	}
	if u := c.L3[schema.CME+".0"]; u != nil && len(u.Addrs) > 0 {
		has := false
		for _, i := range c.Interfaces {
			has = has || i.Management
		}
		if !has {
			b.warnf("interfaces cme", "cme has addresses but no member has a management port (interfaces <port> management)")
		}
	}
}

// validateRouting checks irb attachment, address overlaps and next hops,
// per routing instance.
func (b *builder) validateRouting() {
	c := b.cfg
	// l3-interface on VLANs.
	usedBy := map[string]string{}
	for _, v := range sortedVLANs(c) {
		if v.L3 == "" {
			continue
		}
		path := "vlans " + v.Name + " l3-interface"
		u := c.L3[v.L3]
		switch {
		case u == nil:
			b.errorf(path, "%s is not configured (interfaces irb unit %s)", v.L3, v.L3[len("irb."):])
			continue
		case usedBy[v.L3] != "":
			b.errorf(path, "%s is already the l3-interface of vlan %s", v.L3, usedBy[v.L3])
			continue
		}
		usedBy[v.L3] = v.Name
		u.VLAN = v.ID
	}
	names := make([]string, 0, len(c.L3))
	for n := range c.L3 {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return config.NaturalLess(names[i], names[j]) })
	type owner struct {
		unit   string
		p      netip.Prefix
		member int // 0 = every member
	}
	seen := map[string][]owner{} // per instance
	for _, n := range names {
		u := c.L3[n]
		if u.IRB() && u.VLAN == 0 {
			b.warnf("interfaces irb unit "+strconv.Itoa(u.Unit), "%s is not the l3-interface of any VLAN; it has no effect", n)
		}
		for _, p := range u.Addrs {
			mem := u.AddrMember[p]
			if u.Member != 0 {
				mem = u.Member // a port's addresses exist on its member only
			}
			for _, o := range seen[u.Instance] {
				if o.member != 0 && mem != 0 && o.member != mem {
					if o.p.Addr() == p.Addr() {
						b.errorf(unitPath(u)+" address", "%s is also used on %s", p, o.unit)
					}
					continue // different members: overlap is the point (one subnet)
				}
				if o.p.Overlaps(p) && (o.unit != n || o.p.Masked() != p.Masked()) {
					b.errorf(unitPath(u)+" address", "%s overlaps %s on %s", p, o.p, o.unit)
				}
			}
			seen[u.Instance] = append(seen[u.Instance], owner{n, p, mem})
		}
	}
	// Next hops must be reachable through a connected subnet of the instance.
	check := func(inst, base string, routes []StaticRoute) {
		for _, r := range routes {
			for _, h := range r.NextHops {
				ok := false
				for _, o := range seen[inst] {
					if o.p.Masked().Contains(h) {
						ok = true
					}
				}
				if !ok {
					b.warnf(base+" static route "+r.Prefix.String()+" next-hop",
						"%s is not in a subnet of any routed interface of this instance; the route stays inactive until it is", h)
				}
			}
		}
	}
	check("", "routing-options", c.Routes)
	for _, in := range c.Instances {
		check(in.Name, "routing-instances "+in.Name+" routing-options", in.Routes)
	}
}

func unitPath(u *L3Unit) string {
	return fmt.Sprintf("interfaces %s unit %d family", u.Parent, u.Unit)
}

func sortedVLANs(c *Config) []*VLAN {
	vs := make([]*VLAN, 0, len(c.VLANs))
	for _, v := range c.VLANs {
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Name < vs[j].Name })
	return vs
}
