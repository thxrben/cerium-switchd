package bgpd

import (
	"net/netip"
	"testing"

	"github.com/thxrben/cerium-switchd/lib/bgp"
	"github.com/thxrben/cerium-switchd/lib/conf/memslots"
)

// cer-bgpd keeps no copy of the table for cer-ribd and the members (PLAN
// 15b): what it holds while sending is one piece of deltaChunk prefixes,
// part of its base memory (memslots.DaemonBase, reference 5.1).
func TestMemoryPerPiece(t *testing.T) {
	if testing.Short() {
		t.Skip("measures the heap")
	}
	peer := netip.MustParseAddr("10.0.0.1")
	per := memslots.Measure(deltaChunk, func() any {
		pp := make([]bgp.PrefixPaths, deltaChunk)
		for i := range pp {
			// Every route its own AS path: the worst case.
			p := bgp.Path{Prefix: memslots.TestPrefix(i), Peer: peer, PeerAS: 65001,
				Attrs: bgp.Attrs{ASPath: []bgp.Segment{{ASNs: []uint32{65001, uint32(64512 + i), 65003}}}, NextHop: peer}}
			pp[i] = bgp.PrefixPaths{Prefix: p.Prefix, Routes: []bgp.Route{{Path: p}}}
		}
		return [2]any{pp, prefixRoutes(pp)}
	})
	piece := per * deltaChunk
	t.Logf("one piece of %d prefixes: %d KiB", deltaChunk, piece>>10)
	if piece > 8<<20 {
		t.Errorf("a piece takes %d MiB; the base memory allows 8", piece>>20)
	}
}
