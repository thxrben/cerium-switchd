package rib

import (
	"net/netip"
	"slices"
)

// Protocols that report by prefix (BGP, reference 5.1 memory slots: no
// whole tables in flight) use Replace for the prefixes that changed, and a
// sync (BeginGen, Replace of every prefix, SweepGen) when the receiver may
// have missed something.

type genKey struct {
	instance string
	proto    Protocol
}

// Replace sets every route of proto at one prefix of the instance: the
// routes given (their Source tells them apart) replace the protocol's
// routes there; routes of other sources of proto at the prefix are
// withdrawn (no routes: the prefix is gone for proto). Unchanged routes
// keep their Since time. It reports how many routes the protocol's limit
// refused.
func (r *RIB) Replace(instance string, proto Protocol, prefix netip.Prefix, routes []Route) (refused int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix = prefix.Masked()
	t := TableOf(instance, prefix)
	gen := r.gens[genKey{instance, proto}]
	ds := r.tables[t]
	d := ds[prefix]
	keep := make(map[string]bool, len(routes))
	for _, rt := range routes {
		keep[rt.Source] = true
	}
	if d != nil {
		n := len(d.routes)
		d.routes = slices.DeleteFunc(d.routes, func(old *Route) bool { return old.Protocol == proto && !keep[old.Source] })
		if gone := n - len(d.routes); gone > 0 {
			r.count(proto, -gone)
			r.markDirty(t, prefix)
		}
	}
	for i := range routes {
		rt := r.normalize(routes[i], proto, routes[i].Source)
		rt.Prefix, rt.gen = prefix, gen
		k := key{proto, rt.Source}
		if ds == nil {
			ds = map[netip.Prefix]*dest{}
			r.tables[t] = ds
		}
		if d == nil {
			d = ds[prefix]
		}
		var old *Route
		if d != nil {
			old = d.get(k)
		}
		if old == nil && r.limits[proto] > 0 && r.counts[proto] >= r.limits[proto] {
			refused++
			continue
		}
		if d == nil {
			d = &dest{}
			ds[prefix] = d
		}
		if old == nil {
			r.count(proto, 1)
		} else if sameRoute(old, &rt) {
			old.Attrs, old.Stale, old.gen = rt.Attrs, rt.Stale, gen
			continue
		} else {
			rt.Hidden = old.Hidden // the owner re-evaluates it
		}
		if rt.Since.IsZero() {
			rt.Since = r.now()
		}
		d.put(&rt)
		r.markDirty(t, prefix)
	}
	if refused > 0 {
		if r.refused == nil {
			r.refused = map[Protocol]int{}
		}
		r.refused[proto] += refused
	}
	return refused
}

// BeginGen starts a sync of the protocol's routes in the instance: the
// routes Replace sets from now on carry a new generation; SweepGen then
// removes the ones the sync did not set. The refusal count starts again.
func (r *RIB) BeginGen(instance string, proto Protocol) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gens == nil {
		r.gens = map[genKey]uint32{}
	}
	r.gens[genKey{instance, proto}]++
	if r.refused != nil {
		r.refused[proto] = 0
	}
}

// SweepGen ends a sync: the protocol's routes in the instance that the
// sync did not set are withdrawn (nothing before the new table is
// complete: no route disappears and comes back).
func (r *RIB) SweepGen(instance string, proto Protocol) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gen := r.gens[genKey{instance, proto}]
	for t, ds := range r.tables {
		if t.Instance != instance {
			continue
		}
		for p, d := range ds {
			n := len(d.routes)
			d.routes = slices.DeleteFunc(d.routes, func(rt *Route) bool { return rt.Protocol == proto && rt.gen != gen })
			if gone := n - len(d.routes); gone > 0 {
				r.count(proto, -gone)
				r.markDirty(t, p)
			}
		}
	}
}

// EachAt calls f for the protocol's routes at one prefix of the instance
// (a copy; under the lock: f must not call the RIB).
func (r *RIB) EachAt(instance string, proto Protocol, prefix netip.Prefix, f func(t Table, rt Route)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix = prefix.Masked()
	t := TableOf(instance, prefix)
	if d := r.tables[t][prefix]; d != nil {
		for _, rt := range d.routes {
			if rt.Protocol == proto {
				f(t, *rt)
			}
		}
	}
}
