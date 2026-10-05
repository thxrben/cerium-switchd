package ospf

import (
	"cmp"
	"net/netip"
	"slices"
	"time"
)

// The routing table calculation (RFC 2328 §16, RFC 5340 §4.8).

// vkey names an SPF vertex: a router (net false, id the router id) or a
// transit network (OSPFv2: id the network LSA's LS id; OSPFv3: id the DR's
// router id and ifID its interface id).
type vkey struct {
	net  bool
	id   ID
	ifID uint32
}

type vertex struct {
	key      vkey
	lsa      *LSA
	dist     uint32
	nexthops []NextHop
	parents  []*vertex
	done     bool
	// attached: a network directly attached to the root (its next hops
	// are the interface itself).
	attached bool
	ifc      *iface // attached networks and routers adjacent to the root
}

type spfResult struct {
	routers map[ID]*vertex
	nets    map[vkey]*vertex
}

// asbrIDs returns the reachable AS boundary routers of the area (not this
// router), sorted.
func (s *spfResult) asbrIDs() []ID {
	var out []ID
	for id, v := range s.routers {
		if v.dist > 0 && v.lsa != nil && v.lsa.Flags&FlagE != 0 {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (r *Router) runSPF() {
	start := time.Now()
	r.Stats.SPFRuns++
	r.Stats.LastSPF = r.now
	table := map[netip.Prefix]*Route{}
	for _, a := range r.sortedAreas() {
		a.spf = r.spfArea(a)
		r.intraArea(a, table)
	}
	r.interArea(table)
	r.externals(table)
	routes := make([]Route, 0, len(table))
	for _, rt := range table {
		slices.SortFunc(rt.NextHops, func(a, b NextHop) int {
			if c := cmp.Compare(a.Iface, b.Iface); c != 0 {
				return c
			}
			return a.Gateway.Compare(b.Gateway)
		})
		routes = append(routes, *rt)
	}
	slices.SortFunc(routes, func(a, b Route) int { return comparePrefix(a.Prefix, b.Prefix) })
	r.Stats.SPFDuration = time.Since(start)
	changed := !slices.EqualFunc(routes, r.routes, routeEqual)
	r.routes = routes
	if changed {
		r.dirty = true                             // summaries follow the routes
		if r.OnRoutes != nil && r.restart == nil { // restarting: the old routes stay
			r.OnRoutes(slices.Clone(routes))
		}
	}
}

func routeEqual(a, b Route) bool {
	return a.Prefix == b.Prefix && a.Type == b.Type && a.Area == b.Area && a.Cost == b.Cost && a.Cost2 == b.Cost2 &&
		a.Tag == b.Tag && a.Direct == b.Direct && slices.Equal(a.NextHops, b.NextHops)
}

// Routes returns the routing table of the last SPF.
func (r *Router) Routes() []Route { return slices.Clone(r.routes) }

// ---- the shortest path tree of an area ----

func (r *Router) lsaOf(a *area, k vkey) *LSA {
	var l *LSA
	switch {
	case !k.net && r.v == V2:
		l = a.sc.db.Get(LSRef{Type: V2Router, ID: k.id, AdvRtr: k.id}, r.now)
	case !k.net:
		// OSPFv3: a router may originate several router LSAs; the first
		// (LS id 0) is the one a switch has.
		l = r.v3RouterLSA(a, k.id)
	case r.v == V2:
		for _, n := range a.sc.db.OfType(V2Network, r.now) {
			if n.ID == k.id && n.Age < MaxAge {
				return n
			}
		}
	default:
		l = a.sc.db.Get(LSRef{Type: V3Network, ID: ID(k.ifID), AdvRtr: k.id}, r.now)
	}
	if l == nil || l.Age >= MaxAge {
		return nil
	}
	return l
}

// v3RouterLSA merges the router LSAs of a router (RFC 5340 §4.8.1: all of
// them describe the router together).
func (r *Router) v3RouterLSA(a *area, id ID) *LSA {
	var out *LSA
	for _, l := range a.sc.db.OfType(V3Router, r.now) {
		if l.AdvRtr != id || l.Age >= MaxAge {
			continue
		}
		if out == nil {
			c := *l
			c.Links = slices.Clone(l.Links)
			out = &c
			continue
		}
		out.Links = append(out.Links, l.Links...)
	}
	return out
}

// linksBack reports whether w's LSA links back to v (RFC 2328 §16.1 step
// 2b), and for a router w reached from a network the link (its interface
// address on that network, v2).
func (r *Router) linksBack(v, w *vertex) (RouterLink, bool) {
	if w.key.net {
		return RouterLink{}, slices.Contains(w.lsa.Attached, v.key.id)
	}
	for _, k := range w.lsa.Links {
		switch {
		case !v.key.net && k.Type == LinkP2P:
			if (r.v == V2 && k.ID == v.key.id) || (r.v == V3 && k.NbrRouter == v.key.id) {
				return k, true
			}
		case v.key.net && k.Type == LinkTransit:
			if (r.v == V2 && k.ID == v.key.id) || (r.v == V3 && k.NbrRouter == v.key.id && k.NbrIfID == v.key.ifID) {
				return k, true
			}
		}
	}
	return RouterLink{}, false
}

// rootIface finds the root's interface of a link of its own router LSA.
func (r *Router) rootIface(a *area, k RouterLink) *iface {
	for _, i := range a.ifaces {
		if i.state == IfDown {
			continue
		}
		if (r.v == V2 && IDFrom(i.cfg.Addr) == k.Data) || (r.v == V3 && i.cfg.ID == k.IfID) {
			return i
		}
	}
	return nil
}

func addNextHops(dst []NextHop, add ...NextHop) []NextHop {
	for _, h := range add {
		if len(dst) >= MaxECMP {
			break
		}
		if !slices.Contains(dst, h) {
			dst = append(dst, h)
		}
	}
	return dst
}

func (r *Router) spfArea(a *area) *spfResult {
	res := &spfResult{routers: map[ID]*vertex{}, nets: map[vkey]*vertex{}}
	rootLSA := r.lsaOf(a, vkey{id: r.rid})
	if rootLSA == nil {
		return res
	}
	root := &vertex{key: vkey{id: r.rid}, lsa: rootLSA}
	all := map[vkey]*vertex{root.key: root}
	var cand []*vertex
	v := root
	for v != nil {
		v.done = true
		if v.key.net {
			res.nets[v.key] = v
		} else {
			res.routers[v.key.id] = v
		}
		r.relax(a, v, root, all, &cand)
		// The next vertex: the closest candidate, networks before routers
		// on a tie (RFC 2328 §16.1 step 3).
		v = nil
		best := -1
		for k, c := range cand {
			if best < 0 || c.dist < cand[best].dist || (c.dist == cand[best].dist && c.key.net && !cand[best].key.net) {
				best = k
			}
		}
		if best >= 0 {
			v = cand[best]
			cand = slices.Delete(cand, best, best+1)
		}
	}
	return res
}

// relax examines the links of vertex v (RFC 2328 §16.1 step 2).
func (r *Router) relax(a *area, v, root *vertex, all map[vkey]*vertex, cand *[]*vertex) {
	type edge struct {
		key    vkey
		cost   uint32
		link   RouterLink // the link of v's router LSA
		viaNet bool
	}
	var edges []edge
	if v.key.net {
		for _, id := range v.lsa.Attached {
			edges = append(edges, edge{key: vkey{id: id}, viaNet: true})
		}
	} else {
		for _, k := range v.lsa.Links {
			switch k.Type {
			case LinkP2P:
				id := k.ID
				if r.v == V3 {
					id = k.NbrRouter
				}
				edges = append(edges, edge{key: vkey{id: id}, cost: uint32(k.Metric), link: k})
			case LinkTransit:
				nk := vkey{net: true, id: k.ID}
				if r.v == V3 {
					nk = vkey{net: true, id: k.NbrRouter, ifID: k.NbrIfID}
				}
				edges = append(edges, edge{key: nk, cost: uint32(k.Metric), link: k})
			}
		}
	}
	for _, e := range edges {
		w := all[e.key]
		if w != nil && w.done {
			continue
		}
		if w == nil {
			l := r.lsaOf(a, e.key)
			if l == nil {
				continue
			}
			w = &vertex{key: e.key, lsa: l, dist: ^uint32(0)}
		}
		back, ok := r.linksBack(v, w)
		if !ok {
			continue
		}
		d := v.dist + e.cost
		if d > w.dist {
			continue
		}
		hops, ifc, attached := r.nextHops(a, v, w, root, e.link, back)
		if hops == nil && !attached {
			continue
		}
		if d < w.dist {
			w.dist, w.nexthops, w.parents = d, nil, nil
			w.ifc, w.attached = ifc, attached
			if all[e.key] == nil {
				all[e.key] = w
				*cand = append(*cand, w)
			}
		}
		w.parents = append(w.parents, v)
		w.nexthops = addNextHops(w.nexthops, hops...)
	}
}

// nextHops computes the next hops of w reached from v (RFC 2328 §16.1.1).
func (r *Router) nextHops(a *area, v, w, root *vertex, link, back RouterLink) (hops []NextHop, ifc *iface, attached bool) {
	switch {
	case v == root && w.key.net:
		i := r.rootIface(a, link)
		if i == nil {
			return nil, nil, false
		}
		return []NextHop{{Iface: i.cfg.Name}}, i, true
	case v == root:
		i := r.rootIface(a, link)
		if i == nil {
			return nil, nil, false
		}
		for _, n := range i.nbrs {
			if n.id == w.key.id && n.adjacent() {
				return []NextHop{{Iface: i.cfg.Name, Gateway: n.addr}}, i, false
			}
		}
		return nil, nil, false
	case v.key.net && v.attached:
		i := v.ifc
		gw := netip.Addr{}
		for _, n := range i.nbrs {
			if n.id == w.key.id {
				gw = n.addr
			}
		}
		if !gw.IsValid() && r.v == V2 {
			gw = back.Data.Addr()
		}
		if !gw.IsValid() && r.v == V3 {
			if ll := i.sc.db.Get(LSRef{Type: V3Link, ID: ID(back.IfID), AdvRtr: w.key.id}, r.now); ll != nil {
				gw = ll.LinkLocal
			}
		}
		if !gw.IsValid() {
			return nil, nil, false
		}
		return []NextHop{{Iface: i.Name(), Gateway: gw}}, i, false
	}
	return slices.Clone(v.nexthops), v.ifc, false
}

// Name returns the interface's name.
func (i *iface) Name() string { return i.cfg.Name }

// ---- routes ----

// own reports whether a prefix belongs to one of this router's interfaces
// (a direct route, not installed).
func (r *Router) own(p netip.Prefix) *iface {
	for _, i := range r.sortedIfaces() {
		if i.state == IfDown {
			continue
		}
		for _, q := range i.cfg.Prefixes {
			if q.Masked() == p {
				return i
			}
		}
	}
	return nil
}

// offer puts a route into the table: a better one replaces, an equal one
// adds its next hops (ECMP).
func offer(table map[netip.Prefix]*Route, rt *Route) {
	have := table[rt.Prefix]
	switch {
	case have == nil || rt.better(have):
		c := *rt
		c.NextHops = slices.Clone(rt.NextHops)
		table[rt.Prefix] = &c
	case rt.equal(have) && !have.Direct:
		have.NextHops = addNextHops(have.NextHops, rt.NextHops...)
	}
}

func (r *Router) intraArea(a *area, table map[netip.Prefix]*Route) {
	add := func(p netip.Prefix, cost uint32, v *vertex) {
		p = p.Masked()
		rt := &Route{Prefix: p, Type: IntraArea, Area: a.id, Cost: cost, NextHops: v.nexthops}
		if i := r.own(p); i != nil {
			rt.Direct, rt.NextHops, rt.Cost = true, []NextHop{{Iface: i.cfg.Name}}, uint32(max(i.cfg.Cost, 1))
		} else if len(v.nexthops) == 0 {
			return
		}
		if rt.Cost >= LSInfinity {
			return
		}
		offer(table, rt)
	}
	if r.v == V2 {
		for _, v := range a.spf.routers {
			for _, k := range v.lsa.Links {
				if k.Type == LinkStub {
					add(netip.PrefixFrom(k.ID.Addr(), MaskBits(k.Data.Addr())), v.dist+uint32(k.Metric), v)
				}
			}
		}
		for _, v := range a.spf.nets {
			add(netip.PrefixFrom(v.lsa.ID.Addr(), v.lsa.MaskBits), v.dist, v)
		}
		return
	}
	for _, l := range a.sc.db.OfType(V3IntraAreaPrefix, r.now) {
		if l.Age >= MaxAge {
			continue
		}
		var v *vertex
		switch l.RefType {
		case V3Router:
			v = a.spf.routers[l.RefAdvRtr]
		case V3Network:
			v = a.spf.nets[vkey{net: true, id: l.RefAdvRtr, ifID: uint32(l.RefID)}]
		}
		if v == nil || l.AdvRtr != l.RefAdvRtr {
			continue
		}
		for _, p := range l.Prefixes {
			if p.Options&PrefixNU != 0 {
				continue
			}
			add(p.Prefix, v.dist+uint32(p.Metric), v)
		}
	}
}

// interArea adds the summaries (RFC 2328 §16.2): an ABR looks at the
// backbone's only, other routers at their areas'.
func (r *Router) interArea(table map[netip.Prefix]*Route) {
	t := V2Summary
	if r.v == V3 {
		t = V3InterAreaPrefix
	}
	for _, a := range r.sortedAreas() {
		if a.spf == nil || (r.isABR() && a.id != Backbone) {
			continue
		}
		for _, l := range a.sc.db.OfType(t, r.now) {
			if l.Age >= MaxAge || l.AdvRtr == r.rid || l.Metric >= LSInfinity {
				continue
			}
			if r.v == V3 && l.PrefixOptions&PrefixNU != 0 {
				continue
			}
			abr := a.spf.routers[l.AdvRtr]
			if abr == nil || abr.lsa.Flags&FlagB == 0 || len(abr.nexthops) == 0 {
				continue
			}
			offer(table, &Route{Prefix: l.Prefix.Masked(), Type: InterArea, Area: a.id, Cost: abr.dist + l.Metric, NextHops: abr.nexthops})
		}
	}
}

// asbrRoute finds the best route to an AS boundary router: intra-area in
// any area, or through an ASBR summary (RFC 2328 §16.2, §16.4).
func (r *Router) asbrRoute(id ID) (cost uint32, hops []NextHop, ok bool) {
	best := ^uint32(0)
	for _, a := range r.sortedAreas() {
		if a.spf == nil {
			continue
		}
		if v := a.spf.routers[id]; v != nil && v.lsa.Flags&FlagE != 0 && len(v.nexthops) > 0 {
			if v.dist < best {
				best, hops = v.dist, slices.Clone(v.nexthops)
			} else if v.dist == best {
				hops = addNextHops(hops, v.nexthops...)
			}
		}
	}
	if best != ^uint32(0) {
		return best, hops, true
	}
	t := V2ASBRSummary
	if r.v == V3 {
		t = V3InterAreaRouter
	}
	for _, a := range r.sortedAreas() {
		if a.spf == nil || (r.isABR() && a.id != Backbone) {
			continue
		}
		for _, l := range a.sc.db.OfType(t, r.now) {
			if l.Age >= MaxAge || l.AdvRtr == r.rid || l.DestRouter != id || l.Metric >= LSInfinity {
				continue
			}
			abr := a.spf.routers[l.AdvRtr]
			if abr == nil || len(abr.nexthops) == 0 {
				continue
			}
			if c := abr.dist + l.Metric; c < best {
				best, hops = c, slices.Clone(abr.nexthops)
			} else if c == best {
				hops = addNextHops(hops, abr.nexthops...)
			}
		}
	}
	return best, hops, best != ^uint32(0)
}

// lookup is a longest-prefix match among the intra- and inter-area routes
// (for forwarding addresses).
func lookup(table map[netip.Prefix]*Route, a netip.Addr) *Route {
	var best *Route
	for p, rt := range table {
		if rt.Type > InterArea || !p.Contains(a) {
			continue
		}
		if best == nil || p.Bits() > best.Prefix.Bits() {
			best = rt
		}
	}
	return best
}

// externals adds the AS-external routes (RFC 2328 §16.4).
func (r *Router) externals(table map[netip.Prefix]*Route) {
	intra := map[netip.Prefix]*Route{}
	for p, rt := range table {
		intra[p] = rt
	}
	for _, l := range r.as.db.All(r.now) {
		if (l.Type != V2External && l.Type != V3External) || l.Age >= MaxAge || l.AdvRtr == r.rid || l.Metric >= LSInfinity {
			continue
		}
		dist, hops, ok := r.asbrRoute(l.AdvRtr)
		if !ok {
			continue
		}
		if l.Forward.IsValid() && !l.Forward.IsUnspecified() {
			fr := lookup(intra, l.Forward)
			if fr == nil || fr.Direct && len(fr.NextHops) == 0 {
				continue
			}
			dist, hops = fr.Cost, fr.NextHops
			if fr.Direct {
				// The forwarding address is on an attached network: the next
				// hop is the address itself.
				hops = []NextHop{{Iface: fr.NextHops[0].Iface, Gateway: l.Forward}}
			}
		}
		rt := &Route{Prefix: l.Prefix.Masked(), Tag: l.Tag, NextHops: hops}
		if l.E2 {
			rt.Type, rt.Cost, rt.Cost2 = External2, dist, l.Metric
		} else {
			rt.Type, rt.Cost = External1, dist+l.Metric
		}
		offer(table, rt)
	}
}
