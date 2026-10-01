package rib

import (
	"math/rand/v2"
	"net/netip"
	"slices"
	"testing"
	"time"
)

var (
	p10  = netip.MustParsePrefix("10.0.0.0/8")
	p24  = netip.MustParsePrefix("10.1.1.0/24")
	pdef = netip.MustParsePrefix("0.0.0.0/0")
	p6   = netip.MustParsePrefix("2001:db8::/32")
	gwA  = netip.MustParseAddr("192.0.2.1")
	gwB  = netip.MustParseAddr("192.0.2.2")
	gwC  = netip.MustParseAddr("192.0.2.3")
)

func nh(gw netip.Addr, ifc string) NextHop { return NextHop{Gateway: gw, Interface: ifc} }

func clock() func() time.Time {
	t := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Second); return t }
}

func TestPreferenceSelection(t *testing.T) {
	r := New(clock())
	r.Set("", Static, "", []Route{{Prefix: pdef, Preference: PrefStatic, NextHops: []NextHop{nh(gwA, "1/0/5.0")}}})
	r.Set("", OSPF, "", []Route{{Prefix: pdef, Preference: PrefOSPFExternal, Metric: 10, NextHops: []NextHop{nh(gwB, "1/0/6.0")}}})
	ch := r.Changes()
	if len(ch) != 1 || ch[0].Route.Protocol != Static || ch[0].Route.NextHops[0].Gateway != gwA {
		t.Fatalf("changes %+v", ch)
	}
	// The static route goes away: OSPF takes over, one change.
	r.Set("", Static, "", nil)
	ch = r.Changes()
	if len(ch) != 1 || ch[0].Route.Protocol != OSPF {
		t.Fatalf("changes %+v", ch)
	}
	// A floating static (preference 200) does not take over.
	r.Set("", Static, "", []Route{{Prefix: pdef, Preference: 200, NextHops: []NextHop{nh(gwA, "1/0/5.0")}}})
	if ch := r.Changes(); len(ch) != 0 {
		t.Fatalf("floating static changed the active route: %+v", ch)
	}
	// OSPF withdraws: the floating static is active.
	r.Set("", OSPF, "", nil)
	ch = r.Changes()
	if len(ch) != 1 || ch[0].Route.Protocol != Static || ch[0].Route.Preference != 200 {
		t.Fatalf("changes %+v", ch)
	}
	// Everything gone: a removal.
	r.Set("", Static, "", nil)
	ch = r.Changes()
	if len(ch) != 1 || ch[0].Route != nil {
		t.Fatalf("removal %+v", ch)
	}
	if e := r.Lookup(Query{}); len(e) != 0 {
		t.Fatalf("empty destinations kept: %+v", e)
	}
}

func TestNoChangeNoReport(t *testing.T) {
	r := New(clock())
	rs := []Route{{Prefix: p24, Preference: PrefStatic, NextHops: []NextHop{nh(gwB, "x"), nh(gwA, "x")}}}
	r.Set("", Static, "", rs)
	r.Changes()
	since := r.Lookup(Query{})[0].Routes[0].Since
	// Same route, next hops in another order: nothing to install.
	r.Set("", Static, "", []Route{{Prefix: p24, Preference: PrefStatic, NextHops: []NextHop{nh(gwA, "x"), nh(gwB, "x")}}})
	if ch := r.Changes(); len(ch) != 0 {
		t.Fatalf("unchanged route reported: %+v", ch)
	}
	if got := r.Lookup(Query{})[0].Routes[0].Since; got != since {
		t.Fatal("age reset by an unchanged Set")
	}
	// A metric change of the active route does not touch the kernel.
	r.Set("", OSPF, "", []Route{{Prefix: p10, Preference: PrefOSPF, Metric: 5, NextHops: []NextHop{nh(gwC, "y")}}})
	r.Changes()
	r.Set("", OSPF, "", []Route{{Prefix: p10, Preference: PrefOSPF, Metric: 7, NextHops: []NextHop{nh(gwC, "y")}}})
	if ch := r.Changes(); len(ch) != 0 {
		t.Fatalf("metric-only change reported: %+v", ch)
	}
}

func TestECMPMerge(t *testing.T) {
	r := New(clock())
	// Two BGP paths with the same rank (multipath) are merged.
	r.Set("", BGP, "n1", []Route{{Prefix: p24, Preference: PrefBGP, NextHops: []NextHop{nh(gwA, "a")}}})
	r.Set("", BGP, "n2", []Route{{Prefix: p24, Preference: PrefBGP, NextHops: []NextHop{nh(gwB, "b")}}})
	ch := r.Changes()
	if len(ch) != 1 || len(ch[0].Route.NextHops) != 2 {
		t.Fatalf("ecmp %+v", ch)
	}
	// n2 ranked lower (not multipath): only n1.
	r.Set("", BGP, "n2", []Route{{Prefix: p24, Preference: PrefBGP, Rank: 1, NextHops: []NextHop{nh(gwB, "b")}}})
	ch = r.Changes()
	if len(ch) != 1 || len(ch[0].Route.NextHops) != 1 || ch[0].Route.NextHops[0].Gateway != gwA {
		t.Fatalf("after rank change %+v", ch)
	}
	// MaxPaths limits ECMP.
	r.MaxPaths = 2
	var rs []Route
	for i := range 5 {
		rs = append(rs, Route{Prefix: p10, Preference: PrefOSPF, NextHops: []NextHop{nh(netip.AddrFrom4([4]byte{192, 0, 2, byte(10 + i)}), "z")}})
	}
	for i, rt := range rs {
		r.Set("", OSPF, string(rune('a'+i)), []Route{rt})
	}
	for _, c := range r.Changes() {
		if c.Prefix == p10 && len(c.Route.NextHops) != 2 {
			t.Fatalf("max paths %+v", c.Route.NextHops)
		}
	}
}

func TestTablesAndInstances(t *testing.T) {
	r := New(clock())
	r.Set("", Static, "", []Route{{Prefix: p24, Preference: 5, Discard: true}, {Prefix: p6, Preference: 5, Discard: true}})
	r.Set("red", Static, "", []Route{{Prefix: p24, Preference: 5, Discard: true}})
	ch := r.Changes()
	if len(ch) != 3 || ch[0].Table.Name() != "inet.0" || ch[1].Table.Name() != "inet6.0" || ch[2].Table.Name() != "red.inet.0" {
		t.Fatalf("changes %+v", ch)
	}
	// Setting the default instance does not touch red.
	r.Set("", Static, "", nil)
	ch = r.Changes()
	if len(ch) != 2 || ch[0].Table.Instance != "" || ch[1].Table.Instance != "" {
		t.Fatalf("changes %+v", ch)
	}
	r.DropInstance("red")
	if ch := r.Changes(); len(ch) != 1 || ch[0].Route != nil || ch[0].Table.Instance != "red" {
		t.Fatalf("drop %+v", ch)
	}
}

func TestStaleAndSweep(t *testing.T) {
	r := New(clock())
	r.Set("", BGP, "n1", []Route{{Prefix: p24, Preference: PrefBGP, NextHops: []NextHop{nh(gwA, "a")}},
		{Prefix: p10, Preference: PrefBGP, NextHops: []NextHop{nh(gwA, "a")}}})
	r.Changes()
	r.MarkStale("", BGP)
	// The restarted session re-learns only p24.
	r.Set("", BGP, "n1", []Route{{Prefix: p24, Preference: PrefBGP, NextHops: []NextHop{nh(gwA, "a")}}})
	ch := r.Changes()
	if len(ch) != 1 || ch[0].Prefix != p10 || ch[0].Route != nil {
		// Set withdrew p10 already (it replaces the source's set); the
		// stale flag matters for sources that refill gradually.
		t.Fatalf("changes %+v", ch)
	}
	r.Set("", BGP, "n2", []Route{{Prefix: p10, Preference: PrefBGP, NextHops: []NextHop{nh(gwB, "b")}}})
	r.Changes()
	r.MarkStale("", BGP)
	if ch := r.Changes(); len(ch) != 0 {
		t.Fatalf("marking stale changed the FIB: %+v", ch)
	}
	r.Set("", BGP, "n2", []Route{{Prefix: p10, Preference: PrefBGP, NextHops: []NextHop{nh(gwB, "b")}}}) // refreshed
	r.Sweep("", BGP)
	ch = r.Changes()
	if len(ch) != 1 || ch[0].Prefix != p24 || ch[0].Route != nil {
		t.Fatalf("sweep %+v", ch)
	}
}

func TestLookup(t *testing.T) {
	r := New(clock())
	r.Set("", Direct, "", []Route{{Prefix: p24, NextHops: []NextHop{nh(netip.Addr{}, "1/0/5.0")}}})
	r.Set("", Static, "", []Route{{Prefix: p10, Preference: 5, NextHops: []NextHop{nh(gwA, "1/0/5.0")}},
		{Prefix: pdef, Preference: 5, NextHops: []NextHop{nh(gwB, "1/0/5.0")}}})
	r.Set("", OSPF, "", []Route{{Prefix: p10, Preference: 10, NextHops: []NextHop{nh(gwC, "1/0/6.0")}}})
	r.Changes()
	host := netip.PrefixFrom(netip.MustParseAddr("10.1.1.7"), 32)
	if e := r.Lookup(Query{Prefix: host}); len(e) != 1 || e[0].Prefix != p24 {
		t.Fatalf("longest match %+v", e)
	}
	if e := r.Lookup(Query{Prefix: p10}); len(e) != 2 {
		t.Fatalf("prefix and inside %+v", e)
	}
	if e := r.Lookup(Query{Prefix: p10, Match: "exact"}); len(e) != 1 || len(e[0].Routes) != 2 || e[0].Active != 0 || e[0].Routes[0].Protocol != Static {
		t.Fatalf("exact %+v", e)
	}
	if e := r.Lookup(Query{Prefix: p10, Match: "longer"}); len(e) != 1 || e[0].Prefix != p24 {
		t.Fatalf("longer %+v", e)
	}
	o := OSPF
	if e := r.Lookup(Query{Protocol: &o}); len(e) != 1 || e[0].Active != -1 {
		t.Fatalf("protocol filter (inactive OSPF) %+v", e)
	}
	if e := r.Lookup(Query{Active: true}); len(e) != 3 {
		t.Fatalf("active %+v", e)
	}
	if e := r.Lookup(Query{NextHop: gwB}); len(e) != 1 || e[0].Prefix != pdef {
		t.Fatalf("next hop %+v", e)
	}
	s := r.Summaries()
	if len(s) != 1 || s[0].Destinations != 3 || s[0].Routes != 4 || s[0].Active != 3 || s[0].PerProtocol[OSPF] != [2]int{1, 0} {
		t.Fatalf("summary %+v", s)
	}
}

// TestKernelModel feeds random Sets and checks that applying the reported
// changes to a model kernel always gives exactly the active routes, and
// that a change is reported only when the kernel's view differs.
func TestKernelModel(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	prefixes := []netip.Prefix{p10, p24, pdef, p6, netip.MustParsePrefix("10.1.0.0/16")}
	gws := []netip.Addr{gwA, gwB, gwC}
	protos := []Protocol{Static, OSPF, BGP}
	r := New(clock())
	kernel := map[netip.Prefix]*Route{}
	for step := 0; step < 3000; step++ {
		proto := protos[rnd.IntN(len(protos))]
		src := []string{"", "n1", "n2"}[rnd.IntN(3)]
		var rs []Route
		for _, p := range prefixes {
			if rnd.IntN(2) == 0 {
				continue
			}
			pref := map[Protocol]int{Static: 5, OSPF: 10, BGP: 170}[proto]
			if rnd.IntN(4) == 0 {
				pref = 200
			}
			n := 1 + rnd.IntN(2)
			var hs []NextHop
			for range n {
				hs = append(hs, nh(gws[rnd.IntN(len(gws))], "i"))
			}
			rs = append(rs, Route{Prefix: p, Preference: pref, Metric: uint32(rnd.IntN(3)), NextHops: hs})
		}
		r.Set("", proto, src, rs)
		for _, c := range r.Changes() {
			if c.Route == nil {
				if kernel[c.Prefix] == nil {
					t.Fatalf("step %d: removal of %s that is not installed", step, c.Prefix)
				}
				delete(kernel, c.Prefix)
				continue
			}
			if old := kernel[c.Prefix]; old != nil && sameActive(old, c.Route) {
				t.Fatalf("step %d: change for %s that changes nothing in the kernel", step, c.Prefix)
			}
			kernel[c.Prefix] = c.Route
		}
		// The kernel holds exactly the active routes.
		active := map[netip.Prefix]*Route{}
		for _, e := range r.Lookup(Query{Active: true}) {
			rt := e.Routes[e.Active]
			active[e.Prefix] = &rt
		}
		if len(active) != len(kernel) {
			t.Fatalf("step %d: kernel has %d routes, RIB %d active", step, len(kernel), len(active))
		}
		for p, k := range kernel {
			a := active[p]
			if a == nil || a.Protocol != k.Protocol {
				t.Fatalf("step %d: %s kernel %v, RIB %v", step, p, k, a)
			}
			if !slices.IsSortedFunc(k.NextHops, compareNH) {
				t.Fatalf("next hops not sorted")
			}
		}
	}
}
