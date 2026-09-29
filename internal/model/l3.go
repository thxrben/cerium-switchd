package model

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"

	"mclag/internal/config"
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
}

// IRB reports whether u is a VLAN IP interface.
func (u *L3Unit) IRB() bool { return u.Parent == "irb" }

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
			for _, a := range fam.List(f, "address") {
				p, err := netip.ParsePrefix(a)
				if err != nil {
					continue // the schema already rejected it
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
		b.cfg.L3[l3.Name] = l3
	}
}

// buildIRB builds the irb units. Only units exist on irb.
func (b *builder) buildIRB(e *config.Node) {
	for _, k := range e.Kids {
		if k.Schema.Name != "unit" && k.Schema.Name != "description" {
			b.errorf("interfaces irb "+k.Schema.Name, "only 'unit' (and 'description') is valid on irb")
		}
	}
	b.buildUnits("irb", e, nil)
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

func (b *builder) buildRoutes() {
	for _, e := range b.root.Get("routing-options", "static").Entries("route") {
		path := "routing-options static route " + e.Key
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
		b.cfg.Routes = append(b.cfg.Routes, r)
	}
}

// validateRouting checks irb attachment, address overlaps and next hops.
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
		for _, id := range sortedMemberIDs(c) {
			if m := c.Members[id]; m.Mgmt.VLAN == v.ID {
				b.errorf(path, "vlan %s is the management VLAN of member %d; management and routed VLANs are kept apart", v.Name, id)
			}
		}
	}
	names := make([]string, 0, len(c.L3))
	for n := range c.L3 {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return config.NaturalLess(names[i], names[j]) })
	type owner struct {
		unit string
		p    netip.Prefix
	}
	var seen []owner
	for _, n := range names {
		u := c.L3[n]
		if u.IRB() && u.VLAN == 0 {
			b.warnf("interfaces irb unit "+strconv.Itoa(u.Unit), "%s is not the l3-interface of any VLAN; it has no effect", n)
		}
		for _, p := range u.Addrs {
			for _, o := range seen {
				if o.p.Overlaps(p) && (o.unit != n || o.p.Masked() != p.Masked()) {
					b.errorf(unitPath(u)+" address", "%s overlaps %s on %s", p, o.p, o.unit)
				}
			}
			seen = append(seen, owner{n, p})
		}
	}
	// Next hops must be reachable through a connected subnet.
	for _, r := range c.Routes {
		for _, h := range r.NextHops {
			ok := false
			for _, o := range seen {
				if o.p.Masked().Contains(h) {
					ok = true
				}
			}
			if !ok {
				b.warnf("routing-options static route "+r.Prefix.String()+" next-hop",
					"%s is not in a subnet of any routed interface; the route stays inactive until it is", h)
			}
		}
	}
	// OS-managed NICs with addresses stay in the default instance.
	if len(c.L3) == 0 || b.inv == nil {
		return
	}
	for _, id := range sortedMemberIDs(c) {
		if c.Members[id].Mgmt.Configured() {
			continue
		}
		ports, known := b.inv.Ports(id)
		if !known {
			continue
		}
		for _, name := range sortedPortNames(ports) {
			if ports[name].HasIP {
				if _, managed := c.Interfaces[name]; !managed || !c.Interfaces[name].Switching {
					b.warnf("interfaces", "member %d routes between its VLANs while its operating-system management port %s (%s) is in the same routing instance; "+
						"data VLANs can reach the management network. Configure 'stack member %d management' to separate them", id, name, ports[name].Linux, id)
					break
				}
			}
		}
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

func sortedMemberIDs(c *Config) []int {
	ids := make([]int, 0, len(c.Members))
	for id := range c.Members {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}
