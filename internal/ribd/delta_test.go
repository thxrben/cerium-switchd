package ribd

import (
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/rib"
)

func bgpRoute(p, src, nh string) rib.Route {
	return rib.Route{Prefix: pfx(p), Protocol: rib.BGP, Preference: rib.PrefBGP, Source: src,
		NextHops: []rib.NextHop{{Gateway: ip(nh)}}}
}

// Deltas in order apply; a gap asks for a sync and changes nothing; a
// sync replaces the table without withdrawing what stays; BGP next hops
// are resolved for the changed prefixes.
func TestDelta(t *testing.T) {
	f := &fakeKernel{}
	s := New(f.install, quiet, time.Unix(1000, 0))
	s.SetConfig(&Config{Instances: map[string]Instance{
		"": {Devices: map[string]string{"irb.10": "irb.10"}, Routes: []rib.Route{
			{Prefix: pfx("10.1.0.0/24"), Protocol: rib.Direct, NextHops: []rib.NextHop{{Interface: "irb.10"}}}}},
	}})
	d := func(seq uint64, sync string, ps ...PrefixRoutes) DeltaReply {
		return s.Delta(RoutesDelta{Protocol: rib.BGP, Seq: seq, Sync: sync, Prefixes: ps, Full: true})
	}
	at := func(p string, rs ...rib.Route) PrefixRoutes { return PrefixRoutes{Prefix: pfx(p), Routes: rs} }
	if r := d(1, "begin", at("203.0.113.0/24", bgpRoute("203.0.113.0/24", "a", "10.1.0.5"), bgpRoute("203.0.113.0/24", "b", "10.1.0.6")),
		at("198.51.100.0/24", bgpRoute("198.51.100.0/24", "a", "192.0.2.1"))); r.Resync {
		t.Fatal("begin refused")
	}
	if r := d(2, "end"); r.Resync {
		t.Fatal("end refused")
	}
	es := s.Lookup(rib.Query{})
	if len(es) != 2 { // the direct route and 203.0.113.0/24 (198.51.100.0/24 is hidden)
		t.Fatalf("entries %+v", es)
	}
	if h := s.Lookup(rib.Query{Hidden: true}); len(h) != 1 || h[0].Prefix != pfx("198.51.100.0/24") {
		t.Fatalf("unresolvable next hop not hidden: %+v", h)
	}
	// Neighbour b withdraws 203.0.113.0/24; a's path stays.
	d(3, "", at("203.0.113.0/24", bgpRoute("203.0.113.0/24", "a", "10.1.0.5")))
	if es := s.Lookup(rib.Query{Prefix: pfx("203.0.113.0/24"), Match: "exact"}); len(es) != 1 || len(es[0].Routes) != 1 || es[0].Routes[0].Source != "a" {
		t.Fatalf("after b's withdrawal: %+v", es)
	}
	// A lost delta (4): 5 is refused and changes nothing.
	if r := d(5, "", at("203.0.113.0/24")); !r.Resync {
		t.Fatal("gap not noticed")
	}
	if es := s.Lookup(rib.Query{Prefix: pfx("203.0.113.0/24"), Match: "exact"}); len(es) != 1 {
		t.Fatal("a refused delta was applied")
	}
	// The sync: 198.51.100.0/24 is gone, 203.0.113.0/24 unchanged (its
	// time stays: not withdrawn and added again).
	since := s.Lookup(rib.Query{Prefix: pfx("203.0.113.0/24"), Match: "exact"})[0].Routes[0].Since
	d(10, "begin", at("203.0.113.0/24", bgpRoute("203.0.113.0/24", "a", "10.1.0.5")))
	if h := s.Lookup(rib.Query{Hidden: true}); len(h) != 1 {
		t.Fatal("withdrawn before the sync ended")
	}
	d(11, "end")
	if h := s.Lookup(rib.Query{Hidden: true}); len(h) != 0 {
		t.Fatalf("not swept: %+v", h)
	}
	if es := s.Lookup(rib.Query{Prefix: pfx("203.0.113.0/24"), Match: "exact"}); len(es) != 1 || !es[0].Routes[0].Since.Equal(since) {
		t.Fatalf("the unchanged route was replaced: %+v", es)
	}
}
