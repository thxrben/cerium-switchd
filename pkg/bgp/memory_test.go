package bgp

import (
	"net/netip"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/memslots"
)

// received adds what one received path takes: Adj-RIB-In (as received
// and accepted, through importPath) and the decision's entry. Every path
// has its own attributes (one prefix per UPDATE): the worst case.
func received(s *Speaker, p *peer, prefix netip.Prefix) {
	a := Attrs{Origin: 0, ASPath: []Segment{{ASNs: []uint32{65001, 65002, 65003}}}, NextHop: p.n.Addr, Communities: []uint32{1}}
	raw := &Path{Prefix: prefix, Attrs: a, Peer: p.n.Addr}
	acc := p.importPath(raw)
	p.in[prefix] = &inPath{raw: raw, accepted: acc}
	d := s.best[prefix]
	if d == nil {
		d = &dest{used: 1}
		s.best[prefix] = d
	}
	d.paths = append(d.paths, acc)
}

// The bytes a prefix and a further path take in the speaker are part of
// the memory slots' costs (system memory, reference 5.1).
func TestMemoryPerPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("measures the heap")
	}
	const n = 100000
	// An import policy that changes something (local-pref): the accepted
	// path is a copy, the worst case the capacities must hold.
	s := New(nil, Policy{Import: func(_ *Neighbor, p *Path) bool {
		lp := uint32(200)
		p.LocalPref = &lp
		return true
	}}, nil)
	p1 := newPeer(s, Neighbor{Addr: netip.MustParseAddr("10.0.0.1")})
	p2 := newPeer(s, Neighbor{Addr: netip.MustParseAddr("10.0.0.2")})
	prefix := memslots.Measure(n, func() any {
		for i := range n {
			received(s, p1, memslots.TestPrefix(i))
		}
		return []any{s, p1, p2} // the peers hold Adj-RIB-In
	})
	path := memslots.Measure(n, func() any {
		for i := range n {
			received(s, p2, memslots.TestPrefix(i))
		}
		return []any{s, p1, p2}
	})
	t.Logf("bgp/prefix %d, bgp/path %d bytes", prefix, path)
	for name, got := range map[string]int{"bgp/prefix": prefix, "bgp/path": path} {
		if err := memslots.Drift(name, got); err != nil {
			t.Error(err)
		}
	}
}
