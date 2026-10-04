// Package rib is switchd's routing table (reference 5.8): every route a
// source offers, per routing instance and family, the active route per
// prefix by preference, and the changes to install into the kernel.
//
// The package is pure: no kernel access. Sources (connected, static,
// OSPF, BGP) replace their routes with Set; Changes reports the active
// routes that differ from what was reported before, so the FIB is only
// touched for prefixes that really changed.
package rib

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// Protocol is a route source.
type Protocol int

const (
	Direct Protocol = iota
	Local
	Static
	OSPF
	OSPF3
	BGP
	// DHCP: the default route of a lease (family inet dhcp).
	DHCP
)

var protoNames = [...]string{"Direct", "Local", "Static", "OSPF", "OSPF3", "BGP", "DHCP"}

func (p Protocol) String() string {
	if int(p) < len(protoNames) {
		return protoNames[p]
	}
	return fmt.Sprintf("proto%d", int(p))
}

// ParseProtocol reads a protocol name (case-insensitive).
func ParseProtocol(s string) (Protocol, bool) {
	for i, n := range protoNames {
		if strings.EqualFold(n, s) {
			return Protocol(i), true
		}
	}
	return 0, false
}

// Default preferences (reference 5.8).
const (
	PrefDirect       = 0
	PrefLocal        = 0
	PrefStatic       = 5
	PrefOSPF         = 10
	PrefOSPFExternal = 150
	PrefBGP          = 170
	// PrefDHCP: a lease's default route is used only when no other source
	// offers one (reference 5.3.2).
	PrefDHCP = 200
)

// NextHop is one next hop of a route.
type NextHop struct {
	Gateway   netip.Addr // invalid: directly connected
	Interface string     // switch name of the interface ("irb.10", "1/0/5.0")
}

func (h NextHop) String() string {
	switch {
	case h.Gateway.IsValid() && h.Interface != "":
		return "to " + h.Gateway.String() + " via " + h.Interface
	case h.Gateway.IsValid():
		return "to " + h.Gateway.String()
	default:
		return "via " + h.Interface
	}
}

func compareNH(a, b NextHop) int {
	if c := a.Gateway.Compare(b.Gateway); c != 0 {
		return c
	}
	return strings.Compare(a.Interface, b.Interface)
}

// Route is one route offered by a source.
type Route struct {
	Prefix     netip.Prefix
	Protocol   Protocol
	Preference int
	// Metric is the protocol's metric (OSPF cost, BGP MED); Metric2 a
	// second one (OSPF external type 2 cost). They order routes of the
	// same protocol and preference: lower wins.
	Metric, Metric2 uint32
	NextHops        []NextHop
	Discard         bool // blackhole
	// Rank orders routes of the same protocol and preference that the
	// protocol compares itself (BGP best path: the protocol sets 0 for its
	// best path, higher for the others).
	Rank int
	// Attrs are protocol details for "show route detail" (nil: none).
	Attrs *Attrs `json:",omitempty"`
	// Since is when the route was learned (set by Set when zero).
	Since time.Time
	// Source distinguishes several routes of one protocol for a prefix
	// (e.g. the BGP neighbour); routes are unique per (protocol, source).
	Source string
	// Stale: kept during a graceful restart, not refreshed yet.
	Stale bool
	// Hidden: not usable (a BGP next hop that cannot be resolved): never
	// active, shown by "show route hidden" only. Set by the RIB's owner
	// (SetHidden), kept across Set.
	Hidden bool `json:",omitempty"`
}

// Attrs are a route's protocol details ("show route detail"); the RIB
// does not look at them.
type Attrs struct {
	// OSPF: the area, the path type (intra, inter, ext1, ext2) and the tag.
	Area     string `json:"area,omitempty"`
	PathType string `json:"path_type,omitempty"`
	Tag      uint32 `json:"tag,omitempty"`
	// BGP.
	Peer           string   `json:"peer,omitempty"`
	PeerAS         uint32   `json:"peer_as,omitempty"`
	ASPath         string   `json:"as_path,omitempty"` // as shown: "65001 65002 I"
	LocalPref      *uint32  `json:"local_pref,omitempty"`
	Communities    []string `json:"communities,omitempty"`
	Originator     string   `json:"originator,omitempty"`
	ClusterList    []string `json:"cluster_list,omitempty"`
	InactiveReason string   `json:"inactive_reason,omitempty"` // the protocol's own reason (BGP best path)
}

// Table names a routing table: an instance and a family.
type Table struct {
	Instance string // "" default
	V6       bool
}

// Name is the Junos table name: inet.0, inet6.0, red.inet.0.
func (t Table) Name() string {
	n := "inet.0"
	if t.V6 {
		n = "inet6.0"
	}
	if t.Instance != "" {
		return t.Instance + "." + n
	}
	return n
}

// TableOf returns the table of a prefix in an instance.
func TableOf(instance string, p netip.Prefix) Table {
	return Table{Instance: instance, V6: !p.Addr().Is4()}
}

// Change is one change of an active route: Route nil means the prefix has
// no active route any more (remove it from the kernel).
type Change struct {
	Table  Table
	Prefix netip.Prefix
	Route  *Route
}

type key struct {
	proto  Protocol
	source string
}

type dest struct {
	routes map[key]*Route
	active *Route // the last reported active route (nil: none)
}

// RIB is the routing table of all instances.
type RIB struct {
	mu     sync.Mutex
	now    func() time.Time
	tables map[Table]map[netip.Prefix]*dest
	// dirty prefixes since the last Changes.
	dirty map[Table]map[netip.Prefix]bool
	// MaxPaths bounds ECMP (default 16).
	MaxPaths int
}

// New returns an empty RIB; now is the clock (time.Now if nil).
func New(now func() time.Time) *RIB {
	if now == nil {
		now = time.Now
	}
	return &RIB{now: now, tables: map[Table]map[netip.Prefix]*dest{}, dirty: map[Table]map[netip.Prefix]bool{}, MaxPaths: 16}
}

func (r *RIB) markDirty(t Table, p netip.Prefix) {
	m := r.dirty[t]
	if m == nil {
		m = map[netip.Prefix]bool{}
		r.dirty[t] = m
	}
	m[p] = true
}

// Set replaces every route of (protocol, source) in the instance with
// routes: routes not in the list are withdrawn. Unchanged routes keep
// their Since time.
func (r *RIB) Set(instance string, proto Protocol, source string, routes []Route) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key{proto, source}
	want := map[Table]map[netip.Prefix]*Route{}
	for i := range routes {
		rt := routes[i]
		rt.Protocol, rt.Source = proto, source
		rt.Prefix = rt.Prefix.Masked()
		rt.NextHops = slices.Clone(rt.NextHops)
		slices.SortFunc(rt.NextHops, compareNH)
		rt.NextHops = slices.CompactFunc(rt.NextHops, func(a, b NextHop) bool { return compareNH(a, b) == 0 })
		t := TableOf(instance, rt.Prefix)
		if want[t] == nil {
			want[t] = map[netip.Prefix]*Route{}
		}
		want[t][rt.Prefix] = &rt
	}
	// Withdraw what is gone, in every table of the instance.
	for t, ds := range r.tables {
		if t.Instance != instance {
			continue
		}
		for p, d := range ds {
			if _, ok := d.routes[k]; ok && want[t][p] == nil {
				delete(d.routes, k)
				r.markDirty(t, p)
			}
		}
	}
	for t, rs := range want {
		ds := r.tables[t]
		if ds == nil {
			ds = map[netip.Prefix]*dest{}
			r.tables[t] = ds
		}
		for p, rt := range rs {
			d := ds[p]
			if d == nil {
				d = &dest{routes: map[key]*Route{}}
				ds[p] = d
			}
			if old := d.routes[k]; old != nil && sameRoute(old, rt) {
				old.Attrs, old.Stale = rt.Attrs, rt.Stale
				continue
			} else if old != nil {
				rt.Hidden = old.Hidden // the owner re-evaluates it
			}
			if rt.Since.IsZero() {
				rt.Since = r.now()
			}
			d.routes[k] = rt
			r.markDirty(t, p)
		}
	}
}

// MarkStale marks every route of the protocol in the instance stale
// (graceful restart); Set clears it, Sweep removes what is still stale.
func (r *RIB) MarkStale(instance string, proto Protocol) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for t, ds := range r.tables {
		if t.Instance != instance {
			continue
		}
		for _, d := range ds {
			for k, rt := range d.routes {
				if k.proto == proto {
					rt.Stale = true
				}
			}
		}
	}
}

// Sweep removes the protocol's routes that are still stale.
func (r *RIB) Sweep(instance string, proto Protocol) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for t, ds := range r.tables {
		if t.Instance != instance {
			continue
		}
		for p, d := range ds {
			for k, rt := range d.routes {
				if k.proto == proto && rt.Stale {
					delete(d.routes, k)
					r.markDirty(t, p)
				}
			}
		}
	}
}

// DropInstance removes every route of an instance (the instance is gone).
func (r *RIB) DropInstance(instance string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for t, ds := range r.tables {
		if t.Instance != instance {
			continue
		}
		for p, d := range ds {
			if len(d.routes) > 0 {
				clear(d.routes)
				r.markDirty(t, p)
			}
		}
	}
}

func sameRoute(a, b *Route) bool {
	return a.Preference == b.Preference && a.Metric == b.Metric && a.Metric2 == b.Metric2 && a.Rank == b.Rank &&
		a.Discard == b.Discard && slices.Equal(a.NextHops, b.NextHops)
}

// better orders candidate routes: preference, then metric, metric2, rank,
// then protocol and source for a stable result.
func better(a, b *Route) int {
	return cmp.Or(
		cmp.Compare(a.Preference, b.Preference),
		cmp.Compare(a.Metric, b.Metric),
		cmp.Compare(a.Metric2, b.Metric2),
		cmp.Compare(a.Rank, b.Rank),
		cmp.Compare(a.Protocol, b.Protocol),
		strings.Compare(a.Source, b.Source),
	)
}

// candidates returns the destination's usable routes, best first.
func (d *dest) candidates() []*Route {
	out := make([]*Route, 0, len(d.routes))
	for _, rt := range d.routes {
		if !rt.Hidden {
			out = append(out, rt)
		}
	}
	slices.SortFunc(out, better)
	return out
}

// selectActive returns the active route: the best one, with the next hops
// of every equal route of the same protocol merged (ECMP, e.g. BGP
// multipath sets Rank 0 on every multipath route).
func (r *RIB) selectActive(d *dest) *Route {
	cs := d.candidates()
	if len(cs) == 0 {
		return nil
	}
	best := *cs[0]
	best.NextHops = slices.Clone(best.NextHops)
	for _, c := range cs[1:] {
		if c.Protocol != best.Protocol || c.Preference != best.Preference || c.Metric != best.Metric ||
			c.Metric2 != best.Metric2 || c.Rank != best.Rank || c.Discard || best.Discard {
			break
		}
		best.NextHops = append(best.NextHops, c.NextHops...)
	}
	slices.SortFunc(best.NextHops, compareNH)
	best.NextHops = slices.CompactFunc(best.NextHops, func(a, b NextHop) bool { return compareNH(a, b) == 0 })
	if len(best.NextHops) > r.MaxPaths {
		best.NextHops = best.NextHops[:r.MaxPaths]
	}
	return &best
}

func sameActive(a, b *Route) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Protocol == b.Protocol && a.Discard == b.Discard && slices.Equal(a.NextHops, b.NextHops)
}

// Changes returns the active routes that changed since the last call, as
// the kernel must see them (protocol, next hops, discard), sorted by table
// and prefix. A change of metric or attributes only is not reported.
func (r *RIB) Changes() []Change {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Change
	for t, ps := range r.dirty {
		ds := r.tables[t]
		for p := range ps {
			d := ds[p]
			if d == nil {
				continue
			}
			act := r.selectActive(d)
			if !sameActive(act, d.active) {
				out = append(out, Change{Table: t, Prefix: p, Route: act})
			}
			d.active = act
			if len(d.routes) == 0 {
				delete(ds, p)
			}
		}
	}
	clear(r.dirty)
	slices.SortFunc(out, func(a, b Change) int {
		return cmp.Or(strings.Compare(a.Table.Instance, b.Table.Instance), cmpBool(a.Table.V6, b.Table.V6),
			a.Prefix.Addr().Compare(b.Prefix.Addr()), cmp.Compare(a.Prefix.Bits(), b.Prefix.Bits()))
	})
	return out
}

// Resync returns every active route (for a full reconciliation with the
// kernel, e.g. at start); it also clears the pending changes.
func (r *RIB) Resync() []Change {
	r.Changes()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Change
	for t, ds := range r.tables {
		for p, d := range ds {
			if d.active != nil {
				out = append(out, Change{Table: t, Prefix: p, Route: d.active})
			}
		}
	}
	slices.SortFunc(out, func(a, b Change) int {
		return cmp.Or(strings.Compare(a.Table.Instance, b.Table.Instance), cmpBool(a.Table.V6, b.Table.V6),
			a.Prefix.Addr().Compare(b.Prefix.Addr()), cmp.Compare(a.Prefix.Bits(), b.Prefix.Bits()))
	})
	return out
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

// Entry is a destination in "show route": every route, best first, with
// the active one marked.
type Entry struct {
	Table  Table
	Prefix netip.Prefix
	Routes []Route
	// Active is the index of the active route in Routes (-1: none).
	Active int
}

// Query selects routes for "show route".
type Query struct {
	Tables   []Table // empty: all tables
	Prefix   netip.Prefix
	Match    string // "": longest match for a host address, else the prefix and everything inside; "exact", "longer"
	Protocol *Protocol
	NextHop  netip.Addr
	Active   bool // active routes only
	Hidden   bool // the hidden routes instead of the usable ones
}

// Lookup returns the destinations matching q, sorted.
func (r *RIB) Lookup(q Query) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Entry
	for t, ds := range r.tables {
		if len(q.Tables) > 0 && !slices.Contains(q.Tables, t) {
			continue
		}
		var keys []netip.Prefix
		if q.Prefix.IsValid() && q.Match == "" && q.Prefix.IsSingleIP() {
			// Longest match.
			var best netip.Prefix
			for p := range ds {
				if p.Contains(q.Prefix.Addr()) && (!best.IsValid() || p.Bits() > best.Bits()) {
					best = p
				}
			}
			if best.IsValid() {
				keys = append(keys, best)
			}
		} else {
			for p := range ds {
				if q.Prefix.IsValid() {
					if p.Addr().Is4() != q.Prefix.Addr().Is4() {
						continue
					}
					inside := q.Prefix.Bits() <= p.Bits() && q.Prefix.Contains(p.Addr())
					switch q.Match {
					case "exact":
						if p != q.Prefix.Masked() {
							continue
						}
					case "longer":
						if !inside || p.Bits() == q.Prefix.Bits() {
							continue
						}
					default:
						if !inside {
							continue
						}
					}
				}
				keys = append(keys, p)
			}
		}
		for _, p := range keys {
			d := ds[p]
			e := Entry{Table: t, Prefix: p, Active: -1}
			cands := d.candidates()
			if q.Hidden {
				cands = nil
				for _, rt := range d.routes {
					if rt.Hidden {
						cands = append(cands, rt)
					}
				}
				slices.SortFunc(cands, better)
			}
			for i, c := range cands {
				if q.Protocol != nil && c.Protocol != *q.Protocol {
					continue
				}
				if q.NextHop.IsValid() && !slices.ContainsFunc(c.NextHops, func(h NextHop) bool { return h.Gateway == q.NextHop }) {
					continue
				}
				isActive := i == 0 && d.active != nil && !q.Hidden
				if q.Active && !isActive {
					continue
				}
				if isActive {
					e.Active = len(e.Routes)
				}
				e.Routes = append(e.Routes, *c)
			}
			if len(e.Routes) > 0 {
				out = append(out, e)
			}
		}
	}
	slices.SortFunc(out, func(a, b Entry) int {
		return cmp.Or(strings.Compare(a.Table.Instance, b.Table.Instance), cmpBool(a.Table.V6, b.Table.V6),
			a.Prefix.Addr().Compare(b.Prefix.Addr()), cmp.Compare(a.Prefix.Bits(), b.Prefix.Bits()))
	})
	return out
}

// SetHidden marks a route (by protocol and source) hidden or usable; it
// reports whether that changed.
func (r *RIB) SetHidden(t Table, p netip.Prefix, proto Protocol, source string, hidden bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.tables[t][p]
	if d == nil {
		return false
	}
	rt := d.routes[key{proto, source}]
	if rt == nil || rt.Hidden == hidden {
		return false
	}
	rt.Hidden = hidden
	r.markDirty(t, p)
	return true
}

// Each calls f for every route of a protocol (a copy; under the lock: f
// must not call the RIB).
func (r *RIB) Each(proto Protocol, f func(t Table, rt Route)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for t, ds := range r.tables {
		for _, d := range ds {
			for k, rt := range d.routes {
				if k.proto == proto {
					f(t, *rt)
				}
			}
		}
	}
}

// Summary counts destinations and routes per table and protocol.
type Summary struct {
	Table        Table
	Destinations int
	Routes       int
	Active       int
	Hidden       int
	PerProtocol  map[Protocol][2]int // routes, active
}

// Summaries returns one Summary per table, sorted by name.
func (r *RIB) Summaries() []Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Summary
	for t, ds := range r.tables {
		s := Summary{Table: t, PerProtocol: map[Protocol][2]int{}}
		for _, d := range ds {
			if len(d.routes) == 0 {
				continue
			}
			s.Destinations++
			var best *Route
			if cs := d.candidates(); len(cs) > 0 {
				best = cs[0]
			}
			for _, rt := range d.routes {
				s.Routes++
				if rt.Hidden {
					s.Hidden++
				}
				c := s.PerProtocol[rt.Protocol]
				c[0]++
				if rt == best && d.active != nil {
					c[1]++
					s.Active++
				}
				s.PerProtocol[rt.Protocol] = c
			}
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Summary) int { return strings.Compare(a.Table.Name(), b.Table.Name()) })
	return out
}
