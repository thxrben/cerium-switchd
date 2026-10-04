package memslots

import (
	"fmt"
	"net/netip"
	"runtime"
)

// Go is the bytes our programs keep per entry, by component. The tests of
// the packages that own them (pkg/rib, pkg/bgp, internal/bgpd) measure
// them and fail when the code drifts more than 10 % from these values;
// those marked as estimates are not measured.
var Go = map[string]int{
	"rib/bgp":     711, // cer-ribd: a BGP route with its own attributes (none shared: the worst case)
	"rib/ospf":    470, // cer-ribd: an OSPF route
	"bgp/prefix":  966, // cer-bgpd: Adj-RIB-In (received, and accepted after a policy that changed it) and the decision
	"bgp/path":    870, // cer-bgpd: a further path to a prefix (likewise)
	"bgpd/export": 440, // cer-bgpd: the copy of the table it gives cer-ribd and the members
	// Estimates: an OSPF route's share of cer-ospfd's database and SPF
	// result; a MAC address's copy in cer-mclagd.
	"ospf/route": 420,
	"mclag/mac":  122,
}

// Kernel is the kernel's bytes per entry: a route is a FIB alias and its
// share of the trie (IPv4), or a fib6 node and a route (IPv6); a
// neighbour an entry in a 512-byte slab; a MAC address a bridge FDB
// entry; a membership a port group and its share of the group.
var Kernel = map[Purpose]int{
	BGPv4:     104,
	BGPv6:     384,
	BGPPaths:  56,
	OSPF:      104,
	ARP:       512,
	NDP:       512,
	MAC:       128,
	Multicast: 300,
}

// Costs is the bytes one entry of each purpose takes in this release:
// everything it holds (protocol, routing table, kernel).
var Costs = map[Purpose]int{
	BGPv4:     Go["bgp/prefix"] + Go["bgpd/export"] + Go["rib/bgp"] + Kernel[BGPv4],
	BGPv6:     Go["bgp/prefix"] + Go["bgpd/export"] + Go["rib/bgp"] + Kernel[BGPv6] + 2*16, // longer addresses
	BGPPaths:  Go["bgp/path"] + Go["bgpd/export"] + Go["rib/bgp"]/2 + Kernel[BGPPaths],
	OSPF:      Go["ospf/route"] + Go["rib/ospf"] + Kernel[OSPF],
	ARP:       Kernel[ARP],
	NDP:       Kernel[NDP],
	MAC:       Go["mclag/mac"] + Kernel[MAC],
	Multicast: Kernel[Multicast],
}

// Measure is the heap one of n entries takes: build makes a structure of
// n entries and returns it; what it frees on the way does not count.
func Measure(n int, build func() any) int {
	before := heap()
	x := build()
	after := heap()
	runtime.KeepAlive(x)
	if after < before {
		return 0
	}
	return int((after - before) / uint64(n))
}

func heap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// Drift reports a measured value more than 10 % away from Go[name].
func Drift(name string, measured int) error {
	want, ok := Go[name]
	if !ok {
		return fmt.Errorf("memslots.Go has no %q", name)
	}
	if measured*10 < want*9 || measured*10 > want*11 {
		return fmt.Errorf("%s takes %d bytes per entry, memslots.Go says %d: update internal/memslots/costs.go (the slots' capacities depend on it)", name, measured, want)
	}
	return nil
}

// TestPrefix is the i-th of distinct /24 prefixes (for measurements).
func TestPrefix(i int) netip.Prefix {
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(10 + i>>16), byte(i >> 8), byte(i), 0}), 24)
}
