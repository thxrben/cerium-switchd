package daemon

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/alarms"
	"github.com/thxrben/cerium-switchd/internal/memslots"
	"github.com/thxrben/cerium-switchd/internal/supervise"
)

func TestMemoryPlanStaticAndLimits(t *testing.T) {
	dir := t.TempDir()
	m := &memoryCtl{member: 1, log: slog.New(slog.DiscardHandler), alarms: &alarms.Set{}, ports: func() []string { return nil },
		file: filepath.Join(dir, "memory-slots.json"), sysRoot: dir, run: func(string, ...string) error { return nil }}
	// A kept plan (switchd restarted): used as it is, not recomputed.
	cfg := memslots.Config{Alloc: map[memslots.Purpose]memslots.Amount{memslots.BGPv4: {Slots: 100}, memslots.ARP: {Slots: 2}},
		UpdateSize: memslots.DefaultUpdateSize, MgmtReserve: memslots.DefaultMgmtReserve}
	plan := memslots.Compute(memslots.Hardware{RAM: 8 << 30, Daemons: 300 << 20}, cfg)
	m.mu.Lock()
	m.applied = &appliedMemory{Config: cfg, Plan: plan}
	m.stack = memslots.Min([]memslots.Plan{plan})
	m.mu.Unlock()
	if got := m.Capacity(memslots.BGPv4); got != 100*memslots.PerSlot(memslots.BGPv4) {
		t.Fatalf("capacity %d", got)
	}
	if m.Capacity(memslots.MAC) != 0 {
		t.Fatal("an unallocated purpose is limited")
	}
	if m.UpdateRoom() != 512<<20 {
		t.Fatalf("update room %d", m.UpdateRoom())
	}
	l := m.limits("cer-bgpd")
	if l.GoLimit <= memslots.DaemonBase["cer-bgpd"] || l.Max != l.GoLimit*3/2 || l.Min != 0 {
		t.Fatalf("bgpd limits %+v", l)
	}
	if l := m.limits("cer-lacpd"); l.Min != memslots.DaemonBase["cer-lacpd"] {
		t.Fatalf("lacpd limits %+v", l)
	}
	// The stack's smallest member decides the capacity, not the limits.
	small := memslots.Compute(memslots.Hardware{RAM: 2 << 30, Daemons: 300 << 20},
		memslots.Config{Alloc: map[memslots.Purpose]memslots.Amount{memslots.BGPv4: {Slots: 10}}})
	changed := false
	m.onChange = func() { changed = true }
	m.setStack([]memslots.Plan{plan, small})
	if !changed || m.Capacity(memslots.BGPv4) != 10*memslots.PerSlot(memslots.BGPv4) {
		t.Fatalf("stack capacity %d", m.Capacity(memslots.BGPv4))
	}
	if got := m.limits("cer-bgpd"); got.GoLimit != l.GoLimit {
		t.Fatal("the stack changed a daemon's limits")
	}
	// Without slots: dynamic, no limits.
	m.mu.Lock()
	m.applied = &appliedMemory{Plan: plan}
	m.mu.Unlock()
	if m.Capacity(memslots.BGPv4) != 0 || m.limits("cer-bgpd") != (supervise.Limits{}) {
		t.Fatal("limits without slots")
	}
}
