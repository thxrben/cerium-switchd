package rib

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/memslots"
)

// The bytes a route takes in the RIB are part of the memory slots' costs
// (system memory, reference 5.1): this fails when they drift.
func TestMemoryPerRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("measures the heap")
	}
	const n = 100000
	gw := netip.MustParseAddr("10.0.0.1")
	lp := uint32(100)
	for _, c := range []struct {
		name  string
		proto Protocol
		route func(i int) Route
	}{
		{"rib/bgp", BGP, func(i int) Route {
			return Route{Prefix: memslots.TestPrefix(i), Protocol: BGP, Preference: 170, NextHops: []NextHop{{Gateway: gw}},
				// Every route its own AS path: the worst case (no sharing),
				// which the capacities must hold.
				Attrs: &Attrs{Peer: "10.0.0.1", PeerAS: 65001, ASPath: fmt.Sprintf("65001 %d 65003 I", 64512+i), LocalPref: &lp}}
		}},
		{"rib/ospf", OSPF, func(i int) Route {
			return Route{Prefix: memslots.TestPrefix(i), Protocol: OSPF, Preference: 10, Metric: 20,
				NextHops: []NextHop{{Gateway: gw, Interface: "1/0/1.0"}}, Attrs: &Attrs{Area: "0.0.0.0", PathType: "Intra"}}
		}},
	} {
		got := memslots.Measure(n, func() any {
			r := New(nil)
			rs := make([]Route, 0, n)
			for i := range n {
				rs = append(rs, c.route(i))
			}
			r.Set("", c.proto, "10.0.0.1", rs)
			r.Changes()
			return r
		})
		t.Logf("%s: %d bytes per route", c.name, got)
		if err := memslots.Drift(c.name, got); err != nil {
			t.Error(err)
		}
	}
}
