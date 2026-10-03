package ospf

import (
	"bytes"
	"net/netip"
	"slices"
	"time"
)

// Origination of this router's LSAs (RFC 2328 §12.4, RFC 5340 §4.4.3).
// The wanted LSAs are computed from the interfaces, neighbours and routes
// every time something changes; an LSA is originated again only when its
// contents differ, at most every MinLSInterval.

// wantSet is the wanted own LSAs of every scope.
type wantSet map[*scope]map[LSRef]*LSA

func (w wantSet) add(s *scope, l *LSA) {
	if w[s] == nil {
		w[s] = map[LSRef]*LSA{}
	}
	w[s][l.Ref()] = l
}

func (r *Router) template(t LSType, id ID) *LSA {
	l := &LSA{V: r.v, LSAHeader: LSAHeader{Type: t, ID: id, AdvRtr: r.rid}}
	if r.v == V2 {
		l.Options = OptE
	}
	return l
}

// linkMetric is the metric of a transit or point-to-point link (maximum
// while overloaded, RFC 6987).
func (r *Router) linkMetric(i *iface) uint16 {
	if r.cfg.Overload {
		return MaxMetric
	}
	return max(i.cfg.Cost, 1)
}

// transit reports whether a broadcast interface is a transit network: it
// is the DR with a full neighbour, or it is fully adjacent to the DR
// (RFC 2328 §12.4.1.2).
func (i *iface) transit() bool {
	if i.cfg.P2P || i.state == IfDown || i.state == IfPassive || i.state == IfWaiting || i.dr == 0 {
		return false
	}
	if i.dr == i.self() {
		return len(i.fullNbrs()) > 0
	}
	n := i.drNeighbor()
	return n != nil && n.state == NbrFull
}

// drIfID is the OSPFv3 interface id of the DR on a transit link.
func (i *iface) drIfID() uint32 {
	if i.dr == i.r.rid {
		return i.cfg.ID
	}
	if n := i.drNeighbor(); n != nil {
		return n.ifID
	}
	return 0
}

// desired computes every LSA this router wants in the routing domain.
func (r *Router) desired() wantSet {
	w := wantSet{}
	if r.rid == 0 {
		return w
	}
	for _, a := range r.sortedAreas() {
		if r.v == V2 {
			r.desiredV2(w, a)
		} else {
			r.desiredV3(w, a)
		}
		if r.isABR() {
			r.desiredSummaries(w, a)
		}
	}
	r.desiredExternals(w)
	return w
}

func (r *Router) routerFlags() uint8 {
	var f uint8
	if r.isABR() {
		f |= FlagB
	}
	if r.isASBR() {
		f |= FlagE
	}
	return f
}

func sortedIfs(a *area) []*iface {
	out := slices.Clone(a.ifaces)
	slices.SortFunc(out, func(x, y *iface) int {
		if x.cfg.Name < y.cfg.Name {
			return -1
		}
		if x.cfg.Name > y.cfg.Name {
			return 1
		}
		return 0
	})
	return out
}

func (r *Router) desiredV2(w wantSet, a *area) {
	rl := r.template(V2Router, r.rid)
	rl.Flags = r.routerFlags()
	stub := func(p netip.Prefix, cost uint16) {
		p = p.Masked()
		rl.Links = append(rl.Links, RouterLink{Type: LinkStub, ID: IDFrom(p.Addr()), Data: IDFrom(Mask(p.Bits())), Metric: max(cost, 1)})
	}
	for _, i := range sortedIfs(a) {
		if i.state == IfDown || len(i.cfg.Prefixes) == 0 {
			continue
		}
		switch {
		case i.state == IfPassive:
			for _, p := range i.cfg.Prefixes {
				stub(p, i.cfg.Cost)
			}
		case i.cfg.P2P:
			for _, n := range i.fullNbrs() {
				rl.Links = append(rl.Links, RouterLink{Type: LinkP2P, ID: n.id, Data: IDFrom(i.cfg.Addr), Metric: r.linkMetric(i)})
			}
			for _, p := range i.cfg.Prefixes {
				stub(p, i.cfg.Cost)
			}
		default:
			if i.transit() {
				rl.Links = append(rl.Links, RouterLink{Type: LinkTransit, ID: i.dr, Data: IDFrom(i.cfg.Addr), Metric: r.linkMetric(i)})
			} else {
				stub(i.cfg.Prefixes[0], i.cfg.Cost)
			}
			for _, p := range i.cfg.Prefixes[1:] {
				stub(p, i.cfg.Cost)
			}
		}
		// Network LSA: the DR of a network with full neighbours.
		if !i.cfg.P2P && i.state == IfDR && len(i.fullNbrs()) > 0 {
			nl := r.template(V2Network, IDFrom(i.cfg.Addr))
			nl.MaskBits = i.prefixBits()
			nl.Attached = []ID{r.rid}
			for _, n := range i.fullNbrs() {
				nl.Attached = append(nl.Attached, n.id)
			}
			w.add(a.sc, nl)
		}
	}
	w.add(a.sc, rl)
}

func (r *Router) desiredV3(w wantSet, a *area) {
	rl := r.template(V3Router, 0)
	rl.Flags, rl.Options = r.routerFlags(), r.options()
	ip := r.template(V3IntraAreaPrefix, 0)
	ip.RefType, ip.RefID, ip.RefAdvRtr = V3Router, 0, r.rid
	addPrefixes := func(l *LSA, i *iface, metric uint16) {
		for _, p := range i.cfg.Prefixes {
			l.Prefixes = append(l.Prefixes, PrefixEntry{Prefix: p.Masked(), Metric: metric})
		}
	}
	for _, i := range sortedIfs(a) {
		if i.state == IfDown {
			continue
		}
		// Link LSA (link scope; not on passive interfaces: no neighbours).
		if i.state != IfPassive {
			ll := r.template(V3Link, ID(i.cfg.ID))
			ll.Priority, ll.Options, ll.LinkLocal = i.cfg.Priority, r.options(), i.cfg.Addr
			for _, p := range i.cfg.Prefixes {
				ll.Prefixes = append(ll.Prefixes, PrefixEntry{Prefix: p.Masked()})
			}
			w.add(i.sc, ll)
		}
		switch {
		case i.state == IfPassive:
			addPrefixes(ip, i, max(i.cfg.Cost, 1))
		case i.cfg.P2P:
			for _, n := range i.fullNbrs() {
				rl.Links = append(rl.Links, RouterLink{Type: LinkP2P, Metric: r.linkMetric(i), IfID: i.cfg.ID, NbrIfID: n.ifID, NbrRouter: n.id})
			}
			addPrefixes(ip, i, max(i.cfg.Cost, 1))
		case i.transit():
			rl.Links = append(rl.Links, RouterLink{Type: LinkTransit, Metric: r.linkMetric(i), IfID: i.cfg.ID,
				NbrIfID: i.drIfID(), NbrRouter: i.dr})
			if i.state == IfDR {
				r.desiredV3Network(w, a, i)
			}
		default:
			addPrefixes(ip, i, max(i.cfg.Cost, 1))
		}
	}
	w.add(a.sc, rl)
	if len(ip.Prefixes) > 0 {
		w.add(a.sc, ip)
	}
}

// desiredV3Network: the DR's network LSA and the intra-area prefix LSA
// with the link's prefixes from the attached routers' link LSAs (RFC 5340
// §4.4.3.8).
func (r *Router) desiredV3Network(w wantSet, a *area, i *iface) {
	nl := r.template(V3Network, ID(i.cfg.ID))
	nl.Options = r.options()
	nl.Attached = []ID{r.rid}
	seen := map[netip.Prefix]bool{}
	ip := r.template(V3IntraAreaPrefix, ID(i.cfg.ID))
	ip.RefType, ip.RefID, ip.RefAdvRtr = V3Network, ID(i.cfg.ID), r.rid
	add := func(p netip.Prefix, opts uint8) {
		if opts&(PrefixNU|PrefixLA) != 0 || p.Addr().IsLinkLocalUnicast() || seen[p] {
			return
		}
		seen[p] = true
		ip.Prefixes = append(ip.Prefixes, PrefixEntry{Prefix: p})
	}
	for _, p := range i.cfg.Prefixes {
		add(p.Masked(), 0)
	}
	for _, n := range i.fullNbrs() {
		nl.Attached = append(nl.Attached, n.id)
		if ll := i.sc.db.Get(LSRef{Type: V3Link, ID: ID(n.ifID), AdvRtr: n.id}, r.now); ll != nil && ll.Age < MaxAge {
			nl.Options |= ll.Options
			for _, p := range ll.Prefixes {
				add(p.Prefix, p.Options)
			}
		}
	}
	slices.SortFunc(ip.Prefixes, func(x, y PrefixEntry) int { return comparePrefix(x.Prefix, y.Prefix) })
	w.add(a.sc, nl)
	if len(ip.Prefixes) > 0 {
		w.add(a.sc, ip)
	}
}

func comparePrefix(a, b netip.Prefix) int {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c
	}
	return a.Bits() - b.Bits()
}

// lsID returns the LS id for a prefix-keyed LSA: OSPFv2 the network
// address (RFC 2328 Appendix E is not needed for the prefixes a switch
// announces), OSPFv3 a number kept per prefix.
func (r *Router) lsID(t LSType, p netip.Prefix) ID {
	if r.v == V2 {
		return IDFrom(p.Masked().Addr())
	}
	k := prefixKey{t, p}
	if id, ok := r.prefixIDs[k]; ok {
		return id
	}
	used := map[ID]bool{}
	for kk, id := range r.prefixIDs {
		if kk.t == t {
			used[id] = true
		}
	}
	id := ID(1)
	for used[id] {
		id++
	}
	r.prefixIDs[k] = id
	return id
}

// desiredSummaries: an ABR announces into area a the intra-area routes of
// its other areas and, into non-backbone areas, the inter-area routes of
// the backbone; and the ASBRs it reaches (RFC 2328 §12.4.3).
func (r *Router) desiredSummaries(w wantSet, a *area) {
	for _, rt := range r.routes {
		if rt.Area == a.id || rt.Cost >= LSInfinity {
			continue
		}
		switch {
		case rt.Type == IntraArea:
		case rt.Type == InterArea && rt.Area == Backbone && a.id != Backbone:
		default:
			continue
		}
		if r.v == V2 {
			l := r.template(V2Summary, r.lsID(V2Summary, rt.Prefix))
			l.Prefix, l.Metric = rt.Prefix, rt.Cost
			w.add(a.sc, l)
		} else {
			l := r.template(V3InterAreaPrefix, r.lsID(V3InterAreaPrefix, rt.Prefix))
			l.Prefix, l.Metric = rt.Prefix, rt.Cost
			w.add(a.sc, l)
		}
	}
	for _, x := range r.sortedAreas() {
		if x == a || x.spf == nil {
			continue
		}
		for _, id := range x.spf.asbrIDs() {
			v := x.spf.routers[id]
			if r.v == V2 {
				l := r.template(V2ASBRSummary, id)
				l.DestRouter, l.Metric = id, v.dist
				w.add(a.sc, l)
			} else {
				l := r.template(V3InterAreaRouter, r.lsID(V3InterAreaRouter, netip.PrefixFrom(id.Addr(), 32)))
				l.Options, l.DestRouter, l.Metric = r.options(), id, v.dist
				w.add(a.sc, l)
			}
		}
	}
}

func (r *Router) desiredExternals(w wantSet) {
	for _, e := range r.cfg.Externals {
		p := e.Prefix.Masked()
		if p.Addr().Is4() != (r.v == V2) {
			continue
		}
		t := V2External
		if r.v == V3 {
			t = V3External
		}
		l := r.template(t, r.lsID(t, p))
		l.Prefix, l.Metric, l.E2, l.Tag = p, min(e.Metric, LSInfinity-1), !e.Type1, e.Tag
		l.HasTag = r.v == V2 || e.Tag != 0
		if e.Forward.IsValid() && !e.Forward.IsUnspecified() {
			l.Forward = e.Forward
		}
		w.add(r.as, l)
	}
}

// ---- installing ----

// sameContent: the database copy has the wanted contents.
func sameContent(have, want *LSA) bool {
	c := *want
	c.Seq, c.Age = have.Seq, have.Age
	c.Encode()
	return c.Options == have.Options && bytes.Equal(c.Raw[lsaHeaderLen:], have.Raw[lsaHeaderLen:])
}

// originateAll originates what changed and flushes own LSAs that are no
// longer wanted.
func (r *Router) originateAll() {
	w := r.desired()
	for _, s := range r.scopes() {
		for _, ref := range sortedRefs(w[s]) {
			r.originate(s, w[s][ref])
		}
		for _, l := range s.db.All(r.now) {
			if l.AdvRtr == r.rid && l.Age < MaxAge && w[s][l.Ref()] == nil {
				r.flush(s, l)
			}
		}
	}
}

func sortedRefs(m map[LSRef]*LSA) []LSRef {
	out := make([]LSRef, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, compareRef)
	return out
}

func (r *Router) originate(s *scope, want *LSA) {
	ref := want.Ref()
	have := s.db.Get(ref, r.now)
	if have != nil && have.Age < MaxAge && sameContent(have, want) {
		return
	}
	if t, ok := r.origAt[ref]; ok && r.now.Sub(t) < r.hold[ref] {
		r.deferred[ref] = true
		return
	}
	prev := InitialSeq - 1
	if have != nil {
		if have.Seq == MaxSeq {
			// The sequence number wraps: flush first (RFC 2328 §12.1.6).
			if have.Age < MaxAge {
				r.flush(s, have)
			}
			r.deferred[ref] = true
			return
		}
		prev = have.Seq
	}
	r.install(s, want, prev)
}

// install originates want with the sequence number after prev and floods
// it.
func (r *Router) install(s *scope, want *LSA, prev int32) {
	l := *want
	l.Age, l.Seq = 0, prev+1
	if l.Seq < InitialSeq {
		l.Seq = InitialSeq
	}
	l.Encode()
	if s.db.Install(&l, r.now) {
		r.scheduleSPF()
	}
	delete(s.maxAgeFlooded, l.Ref())
	r.throttle(l.Ref())
	delete(r.deferred, l.Ref())
	r.dropFromRetrans(s, l.Ref())
	r.flood(s, &l, nil, nil)
}

// Origination throttling: an LSA that changed after a quiet period goes
// out at once; while it keeps changing, the hold time before the next
// instance starts at holdMin and doubles up to holdMax (MinLSInterval).
// holdMin is above the neighbours' MinLSArrival, so no instance is dropped.
const (
	holdMin = MinLSArrival*time.Second + 500*time.Millisecond // margin for the time in flight
	holdMax = MinLSInterval * time.Second
)

func (r *Router) throttle(ref LSRef) {
	last, ok := r.origAt[ref]
	switch {
	case !ok || r.now.Sub(last) >= 2*holdMax || r.hold[ref] == 0:
		// After a quiet period a change goes out at once (the check in
		// originate passes); the next one waits at least holdMin.
		r.hold[ref] = holdMin
	default:
		r.hold[ref] = min(2*r.hold[ref], holdMax)
	}
	r.origAt[ref] = r.now
}

// wanted returns the wanted LSA of a scope (nil: not wanted).
func (r *Router) wanted(s *scope, ref LSRef) *LSA {
	return r.desired()[s][ref]
}

// originateDeferred re-runs origination once a deferred LSA may go.
func (r *Router) originateDeferred() {
	for ref := range r.deferred {
		if t, ok := r.origAt[ref]; !ok || r.now.Sub(t) >= r.hold[ref] {
			r.dirty = true
			return
		}
	}
}

// refreshOwn originates own LSAs again every LSRefreshTime.
func (r *Router) refreshOwn() {
	for _, s := range r.scopes() {
		for _, l := range s.db.All(r.now) {
			if l.AdvRtr == r.rid && l.Age >= LSRefreshTime && l.Age < MaxAge {
				r.install(s, l, l.Seq)
			}
		}
	}
}
