package rib

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"
)

// dump is a RIB's content without times (for comparing two RIBs).
func dump(r *RIB) []string {
	var out []string
	for _, e := range r.Lookup(Query{}) {
		for _, rt := range e.Routes {
			out = append(out, fmt.Sprintf("%s %s %s %d %v rank %d", e.Table.Instance, e.Prefix, rt.Source, rt.Protocol, rt.NextHops, rt.Rank))
		}
	}
	slices.Sort(out)
	return out
}

// Random BGP churn: the changed prefixes given with Replace end in the
// same RIB as every neighbour's whole table given with Set; a sync after
// lost changes repairs it without touching what was right.
func TestReplaceEqualsSet(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	clock := func() time.Time { return time.Unix(1000, 0) }
	sources := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	prefixes := make([]netip.Prefix, 200)
	for i := range prefixes {
		prefixes[i] = netip.PrefixFrom(netip.AddrFrom4([4]byte{100, byte(i / 256), byte(i % 256), 0}), 24)
	}
	// truth[source][prefix] = next hop
	truth := map[string]map[netip.Prefix]netip.Addr{}
	for _, s := range sources {
		truth[s] = map[netip.Prefix]netip.Addr{}
	}
	routesAt := func(p netip.Prefix) []Route {
		var rs []Route
		for i, s := range sources {
			if nh, ok := truth[s][p]; ok {
				rs = append(rs, Route{Prefix: p, Source: s, Preference: PrefBGP, Rank: i, NextHops: []NextHop{{Gateway: nh}}})
			}
		}
		return rs
	}
	a, b := New(clock), New(clock)
	for round := 0; round < 50; round++ {
		changed := map[netip.Prefix]bool{}
		for range 30 {
			s := sources[rng.IntN(len(sources))]
			p := prefixes[rng.IntN(len(prefixes))]
			if rng.IntN(3) == 0 {
				delete(truth[s], p)
			} else {
				truth[s][p] = netip.AddrFrom4([4]byte{10, 0, 0, byte(1 + rng.IntN(4))})
			}
			changed[p] = true
		}
		for _, s := range sources {
			var rs []Route
			for p, nh := range truth[s] {
				i := slices.Index(sources, s)
				rs = append(rs, Route{Prefix: p, Preference: PrefBGP, Rank: i, NextHops: []NextHop{{Gateway: nh}}})
			}
			a.Set("default", BGP, s, rs)
		}
		lost := round%10 == 9 // this round's changes do not reach b
		if !lost {
			for p := range changed {
				b.Replace("default", BGP, p, routesAt(p))
			}
		}
		a.Changes()
		b.Changes()
		if lost {
			continue
		}
		if round%10 == 0 && round > 0 {
			// The round after a loss: a sync repairs b.
			b.BeginGen("default", BGP)
			for _, p := range prefixes {
				b.Replace("default", BGP, p, routesAt(p))
			}
			b.SweepGen("default", BGP)
			b.Changes()
		}
		if da, db := dump(a), dump(b); !reflect.DeepEqual(da, db) {
			t.Fatalf("round %d: Set and Replace differ:\nSet:     %d routes\nReplace: %d routes\nfirst Set %v\nfirst Replace %v",
				round, len(da), len(db), head(da), head(db))
		}
		if na, _ := a.Count(BGP); func() bool { nb, _ := b.Count(BGP); return na != nb }() {
			t.Fatalf("round %d: counts differ", round)
		}
	}
}

func head(s []string) []string {
	if len(s) > 3 {
		return s[:3]
	}
	return s
}

// A sync keeps the routes it sets again (no withdrawal and re-add: their
// Since stays) and removes only what it does not set.
func TestSyncKeepsUnchanged(t *testing.T) {
	now := time.Unix(1000, 0)
	r := New(func() time.Time { return now })
	p1, p2 := netip.MustParsePrefix("100.0.1.0/24"), netip.MustParsePrefix("100.0.2.0/24")
	nh := []NextHop{{Gateway: netip.MustParseAddr("10.0.0.1")}}
	r.Replace("default", BGP, p1, []Route{{Source: "a", Preference: PrefBGP, NextHops: nh}})
	r.Replace("default", BGP, p2, []Route{{Source: "a", Preference: PrefBGP, NextHops: nh}})
	r.Changes()
	now = now.Add(time.Hour)
	r.BeginGen("default", BGP)
	r.Replace("default", BGP, p1, []Route{{Source: "a", Preference: PrefBGP, NextHops: nh}})
	r.SweepGen("default", BGP)
	ch := r.Changes()
	if len(ch) != 1 || ch[0].Prefix != p2 || ch[0].Route != nil {
		t.Fatalf("changes %+v: only %s may go", ch, p2)
	}
	es := r.Lookup(Query{})
	if len(es) != 1 || !es[0].Routes[0].Since.Equal(time.Unix(1000, 0)) {
		t.Fatalf("entries %+v", es)
	}
}

// The limit refuses new routes in Replace as in Set.
func TestReplaceLimit(t *testing.T) {
	r := New(nil)
	r.SetLimit(BGP, 1)
	nh := []NextHop{{Gateway: netip.MustParseAddr("10.0.0.1")}}
	if n := r.Replace("default", BGP, netip.MustParsePrefix("100.0.1.0/24"), []Route{{Source: "a", NextHops: nh}}); n != 0 {
		t.Fatal("first refused")
	}
	if n := r.Replace("default", BGP, netip.MustParsePrefix("100.0.2.0/24"), []Route{{Source: "a", NextHops: nh}}); n != 1 {
		t.Fatal("second accepted beyond the limit")
	}
	if routes, refused := r.Count(BGP); routes != 1 || refused != 1 {
		t.Fatalf("count %d refused %d", routes, refused)
	}
}
