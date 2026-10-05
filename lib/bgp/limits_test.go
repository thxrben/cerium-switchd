package bgp

import (
	"net/netip"
	"sync"
	"testing"
)

func TestLimits(t *testing.T) {
	var mu sync.Mutex
	events := map[string]bool{}
	l := &Limits{Max4: 2, Max6: 1, MaxPaths: 1, OnFull: func(p string, full bool) {
		mu.Lock()
		events[p] = full
		mu.Unlock()
	}}
	s := New(nil, Policy{}, nil)
	s.Limits = l
	s.dirty = map[netip.Prefix]bool{}
	a := newPeer(s, Neighbor{Addr: netip.MustParseAddr("10.0.0.1")})
	b := newPeer(s, Neighbor{Addr: netip.MustParseAddr("10.0.0.2")})
	add := func(p *peer, pf string) bool {
		x := netip.MustParsePrefix(pf)
		p.addIn(x, &inPath{raw: &Path{Prefix: x}})
		_, ok := p.in[x]
		return ok
	}
	if !add(a, "10.1.0.0/16") || !add(a, "10.2.0.0/16") {
		t.Fatal("within the capacity")
	}
	if add(a, "10.3.0.0/16") {
		t.Fatal("a third IPv4 prefix was stored")
	}
	if !add(b, "10.1.0.0/16") { // a further path
		t.Fatal("first further path refused")
	}
	if add(b, "10.2.0.0/16") {
		t.Fatal("a second further path was stored")
	}
	if !add(a, "2001:db8::/32") || add(b, "2001:db8:1::/48") {
		t.Fatal("IPv6 capacity")
	}
	if v4, v6, paths := l.Counts(); v4 != 2 || v6 != 1 || paths != 1 {
		t.Fatalf("counts %d %d %d", v4, v6, paths)
	}
	// a's 10.1/16 goes: b's path is the prefix's only one now.
	a.delIn(netip.MustParsePrefix("10.1.0.0/16"))
	if v4, _, paths := l.Counts(); v4 != 2 || paths != 0 {
		t.Fatalf("after withdraw: %d %d", v4, paths)
	}
	b.dropPaths()
	if v4, _, _ := l.Counts(); v4 != 1 {
		t.Fatalf("after drop: %d", v4)
	}
	if !add(a, "10.3.0.0/16") {
		t.Fatal("room again")
	}
	// Re-announcing a stored prefix takes nothing.
	if !add(a, "10.3.0.0/16") {
		t.Fatal("replace refused")
	}
	if v4, _, _ := l.Counts(); v4 != 2 {
		t.Fatalf("replace counted: %d", v4)
	}
}
