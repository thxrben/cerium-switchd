package rib

import (
	"net/netip"
	"testing"
)

func TestLimit(t *testing.T) {
	r := New(nil)
	r.SetLimit(OSPF, 3)
	gw := []NextHop{{Gateway: netip.MustParseAddr("10.0.0.1")}}
	routes := func(ps ...string) []Route {
		var out []Route
		for _, p := range ps {
			out = append(out, Route{Prefix: netip.MustParsePrefix(p), Preference: 10, NextHops: gw})
		}
		return out
	}
	r.Set("", OSPF, "", routes("10.4.0.0/16", "10.1.0.0/16", "10.2.0.0/16", "10.3.0.0/16", "10.5.0.0/16"))
	if n, refused := r.Count(OSPF); n != 3 || refused != 2 {
		t.Fatalf("count %d refused %d", n, refused)
	}
	// The lowest prefixes are kept (deterministic).
	if len(r.Lookup(Query{Prefix: netip.MustParsePrefix("10.4.0.0/16"), Match: "exact"})) != 0 {
		t.Fatal("10.4/16 stored beyond the limit")
	}
	// Existing routes stay when new ones come: 10.1-3 kept, 10.0 refused.
	r.Set("", OSPF, "", routes("10.0.0.0/16", "10.1.0.0/16", "10.2.0.0/16", "10.3.0.0/16"))
	if len(r.Lookup(Query{Prefix: netip.MustParsePrefix("10.1.0.0/16"), Match: "exact"})) == 0 {
		t.Fatal("an existing route was removed for a new one")
	}
	if n, refused := r.Count(OSPF); n != 3 || refused != 1 {
		t.Fatalf("count %d refused %d", n, refused)
	}
	// A route that goes away makes room.
	r.Set("", OSPF, "", routes("10.0.0.0/16", "10.1.0.0/16", "10.2.0.0/16"))
	if n, refused := r.Count(OSPF); n != 3 || refused != 0 {
		t.Fatalf("after withdraw: count %d refused %d", n, refused)
	}
	// Other protocols are not limited; DropInstance counts down.
	r.Set("", Static, "", routes("192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "10.9.0.0/16"))
	r.DropInstance("")
	if n, _ := r.Count(OSPF); n != 0 {
		t.Fatalf("after drop %d", n)
	}
}
