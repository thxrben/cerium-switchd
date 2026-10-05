package memslots

import (
	"strings"
	"testing"
)

const gib = 1 << 30

func TestCompute6G(t *testing.T) {
	hw := Hardware{RAM: 6 * gib, MinFree: 64 << 20, NICRings: 256 << 20, Daemons: 300 << 20,
		System: SystemArea(Facts{Interfaces: 64, VLANs: 100, Members: 2, Ports: 48, ConfigBytes: 40 << 10})}
	p := Compute(hw, Config{Alloc: map[Purpose]Amount{BGPv4: {Percent: 30}, ARP: {Slots: 4}, MAC: {Slots: 8}}})
	if p.Problem != "" {
		t.Fatal(p.Problem)
	}
	fixed := p.Kernel + p.NICRings + p.Daemons + p.Mgmt + p.Margin
	if got := uint64(p.Slots)*SlotSize + fixed; got > hw.RAM || hw.RAM-got >= SlotSize {
		t.Fatalf("slots %d + fixed %d do not divide the RAM %d", p.Slots, fixed, hw.RAM)
	}
	if p.Update != 129 || p.UpdateBytes() != 512<<20 {
		t.Fatalf("update slots %d", p.Update)
	}
	if p.Allocatable != p.Slots-p.System-p.Update {
		t.Fatal("allocatable")
	}
	bgp := p.Purposes[BGPv4]
	if bgp.Slots != p.Allocatable*30/100 || bgp.Capacity != bgp.Slots*PerSlot(BGPv4) {
		t.Fatalf("bgp %+v of %d", bgp, p.Allocatable)
	}
	if p.Capacity(ARP) != 4*8192 || p.Capacity(MAC) != 8*PerSlot(MAC) {
		t.Fatalf("arp %d mac %d", p.Capacity(ARP), p.Capacity(MAC))
	}
	if p.Dynamic != p.Allocatable-bgp.Slots-12 {
		t.Fatalf("dynamic %d", p.Dynamic)
	}
	t.Logf("6 GiB: %d slots (system %d, update %d, allocatable %d); 30%% BGP = %d routes, all slots = %d routes",
		p.Slots, p.System, p.Update, p.Allocatable, bgp.Capacity, p.AllFor(BGPv4))
}

func TestComputeDoesNotFit(t *testing.T) {
	small := Hardware{RAM: 1 * gib, Daemons: 300 << 20}
	p := Compute(small, Config{Alloc: map[Purpose]Amount{ARP: {Slots: 1}}})
	if !strings.Contains(p.Problem, "need more than") {
		t.Fatalf("1 GiB: %q", p.Problem)
	}
	big := Compute(Hardware{RAM: 8 * gib}, Config{Alloc: map[Purpose]Amount{MAC: {Slots: 100000}}})
	if !strings.Contains(big.Problem, "needs 100000 slots") || big.Dynamic != 0 {
		t.Fatalf("too many slots: %q", big.Problem)
	}
}

func TestMin(t *testing.T) {
	cfg := Config{Alloc: map[Purpose]Amount{BGPv4: {Percent: 50}}}
	a := Compute(Hardware{RAM: 8 * gib}, cfg)
	b := Compute(Hardware{RAM: 4 * gib}, cfg)
	broken := Compute(Hardware{RAM: gib / 2}, cfg)
	m := Min([]Plan{a, b, broken})
	if m[BGPv4] != b.Capacity(BGPv4) || b.Capacity(BGPv4) >= a.Capacity(BGPv4) {
		t.Fatalf("min %v (a %d b %d)", m, a.Capacity(BGPv4), b.Capacity(BGPv4))
	}
}

func TestCostsWhole(t *testing.T) {
	for _, p := range Purposes {
		if Costs[p] <= 0 || PerSlot(p) < 1 {
			t.Errorf("%s: cost %d", p, Costs[p])
		}
	}
	if (Config{}).Enabled() {
		t.Fatal("empty config enabled")
	}
}
