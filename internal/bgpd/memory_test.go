package bgpd

import (
	"net/netip"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/memslots"
	"github.com/thxrben/cerium-switchd/pkg/bgp"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

// cer-bgpd keeps the last table it gave cer-ribd (for the members): its
// bytes per route are part of the memory slots' costs (reference 5.1).
func TestMemoryPerExport(t *testing.T) {
	if testing.Short() {
		t.Skip("measures the heap")
	}
	const n = 100000
	peer := netip.MustParseAddr("10.0.0.1")
	got := memslots.Measure(n, func() any {
		by := map[string][]rib.Route{}
		for i := range n {
			// Every route its own AS path: the worst case.
			p := &bgp.Path{Prefix: memslots.TestPrefix(i), Peer: peer, PeerAS: 65001,
				Attrs: bgp.Attrs{ASPath: []bgp.Segment{{ASNs: []uint32{65001, uint32(64512 + i), 65003}}}, NextHop: peer}}
			src := peer.String()
			by[src] = append(by[src], ribRoute(bgp.Route{Path: *p}))
		}
		return by
	})
	t.Logf("bgpd/export: %d bytes per route", got)
	if err := memslots.Drift("bgpd/export", got); err != nil {
		t.Error(err)
	}
}
