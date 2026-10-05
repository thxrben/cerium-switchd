package cli

import (
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
)

type memOps struct {
	fakeOps
	m MemoryStatus
}

func (o *memOps) Memory() (MemoryStatus, error) { return o.m, nil }

func TestMemorySetup(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "root", commit.SuperUser)
	ts.sh.env.Ops = &memOps{m: MemoryStatus{Member: 1, Slots: 1167, System: 9, Update: 129, Allocatable: 1000,
		Purposes: []MemoryPurpose{{Name: "bgp-ipv4", Bytes: 2100, PerSlot: 1997}, {Name: "arp", Bytes: 512, PerSlot: 8192},
			{Name: "mac", Bytes: 250, PerSlot: 16777}}}}
	ts.term.answers = []string{"?", "30%", "abc", "4", "-", "yes"}
	out := ts.ok("request system memory setup")
	for _, want := range []string{"1000 slots of 4 MiB to allocate", "-> 300 slots, 599100 entries; 700 of 1000 slots left",
		"expecting a percentage", "-> 4 slots, 32768 entries; 696 of 1000 slots left",
		"set system memory allocation bgp-ipv4 percent 30", "set system memory allocation arp slots 4", "Written into the candidate"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "allocation mac") {
		t.Errorf("mac was removed:\n%s", out)
	}
	// The shared candidate has it now.
	if got := ts.ok("show configuration system memory"); got != "" {
		t.Errorf("the active configuration changed: %q", got)
	}
	ts.ok("configure")
	if got := ts.ok("show system memory"); !strings.Contains(got, "allocation bgp-ipv4") {
		t.Errorf("candidate:\n%s", got)
	}
	// Too much: nothing written.
	ts.ok("exit")
	ts.term.answers = []string{"90%", "200", ""}
	if out := ts.ok("request system memory setup"); !strings.Contains(out, "nothing written") {
		t.Errorf("over-allocation:\n%s", out)
	}
}

// memStackOps is a stack of two members; member 2 has fewer slots.
type memStackOps struct {
	memOps
	all []MemoryStatus
}

func (o *memStackOps) MemoryAll() ([]MemoryStatus, error) { return o.all, nil }

// In a stack the allocation is computed for the smallest member (any
// member can become master, reference 5.1).
func TestMemorySetupSmallestMember(t *testing.T) {
	ts := newTester(t, newEngine(t), "root", commit.SuperUser)
	purposes := []MemoryPurpose{{Name: "bgp-ipv4", Bytes: 2100, PerSlot: 1997}}
	big := MemoryStatus{Member: 1, Allocatable: 1000, Purposes: purposes}
	small := MemoryStatus{Member: 2, Allocatable: 400, Purposes: purposes}
	ts.sh.env.Ops = &memStackOps{memOps: memOps{m: big}, all: []MemoryStatus{big, small}}
	ts.term.answers = []string{"50%", "yes"}
	out := ts.ok("request system memory setup")
	for _, want := range []string{"The smallest of 2 members is member 2: 400 slots", "-> 200 slots, 399400 entries; 200 of 400 slots left"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}
