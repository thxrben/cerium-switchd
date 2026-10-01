package diag

import (
	"strings"
	"testing"
)

func TestAnalyze(t *testing.T) {
	f := Facts{NumCPU: 4,
		Cards: []Card{
			{Name: "card 1", CurGTs: 5, CurWidth: 4, MaxGTs: 8, MaxWidth: 8, NeedMbps: 40000, Ports: []string{"1/1/0", "1/1/1", "1/1/2", "1/1/3"}},
			{Name: "card 2", CurGTs: 5, CurWidth: 1, MaxGTs: 8, MaxWidth: 1, NeedMbps: 1000, Ports: []string{"1/2/0"}},
		},
		Ports: []Port{
			{Name: "1/1/0", RXQueues: 4, IRQCPUs: []int{0, 0, 0, 0}, RingRX: 256, RingRXMax: 4096, RingTX: 4096, RingTXMax: 4096,
				Features: map[string]string{"rx-gro": "off"}, RXMissed: 12},
			{Name: "1/2/0", RXQueues: 1},
		},
		CPUs:      []CPU{{ID: 0, Dropped: 3}, {ID: 1}},
		Governors: []string{"powersave", "performance"},
		MemTotal:  1000000, MemAvail: 50000,
	}
	got := Analyze(f)
	text := ""
	for _, x := range got {
		text += string(x.Severity) + " " + x.Area + " " + x.Subject + ": " + x.Text + "\n"
	}
	for _, want := range []string{
		"limit PCIe card 1: the link (PCIe 2.0 x4) carries 16 Gbit/s",
		"hint PCIe card 2: the link runs at PCIe 2.0 x1, the card can do PCIe 3.0 x1",
		"hint CPU 1/2/0: one receive queue and no receive packet steering",
		"hint CPU 1/1/0: all 4 queue interrupts are handled by CPU 0",
		"hint CPU cpufreq: 1 of 2 CPUs",
		"hint NIC 1/1/0: receive ring 256 of 4096",
		"hint NIC 1/1/0: offloads supported but off: rx-gro",
		"limit drops 1/1/0: the NIC dropped 12",
		"limit drops CPU 0: 3 frames dropped",
		"limit memory system",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if got[0].Severity != Limit || got[len(got)-1].Severity != Hint {
		t.Errorf("not sorted by severity:\n%s", text)
	}
	if strings.Contains(text, "transmit ring") {
		t.Errorf("full transmit ring reported:\n%s", text)
	}
}
