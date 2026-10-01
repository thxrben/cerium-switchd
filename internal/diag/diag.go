// Package diag finds what limits a member's forwarding ("show system
// bottlenecks", reference 3.5.2). Collect gathers the facts (Linux),
// Analyze turns them into findings; it only reads.
package diag

import (
	"fmt"
	"slices"
	"strings"
)

// Severity of a finding.
type Severity string

const (
	Limit Severity = "limit" // caps throughput or drops frames now
	Hint  Severity = "hint"  // worth changing
)

// Finding is one result.
type Finding struct {
	Severity Severity
	Area     string // PCIe, CPU, NIC, drops, memory
	Subject  string // card, port or CPU
	Text     string
	Advice   string
}

// Card is a PCIe NIC with its ports.
type Card struct {
	Name     string // e.g. "card 1 (0000:04:00)"
	CurGTs   float64
	CurWidth int
	MaxGTs   float64
	MaxWidth int
	// NeedMbps: the sum of its ports' link speeds (per direction).
	NeedMbps int
	Ports    []string
}

// Port is a NIC port's facts.
type Port struct {
	Name, Linux       string
	SpeedMbps         int // current link speed (0: down or unknown)
	RXQueues          int
	RPS               bool // receive packet steering configured
	IRQs              []int
	IRQCPUs           []int // CPU of each IRQ (same order)
	RingRX, RingRXMax int
	RingTX, RingTXMax int
	Features          map[string]string // "on"/"off" (available but off)
	// Drop counters (since boot or since the last run, see Delta).
	RXMissed, RXFIFO, RXNoBuffer, RXDropped uint64
}

// CPU is one CPU's network softirq counters.
type CPU struct {
	ID                  int
	Dropped, TimeSqueeze uint64
}

// Facts are what Collect gathers.
type Facts struct {
	Cards     []Card
	Ports     []Port
	CPUs      []CPU
	NumCPU    int
	Governors []string // per CPU frequency governor ("" unknown)
	MemTotal  uint64   // kB
	MemAvail  uint64   // kB
	// Delta: the counters are increases since the previous run (else
	// totals since boot).
	Delta bool
}

// pcieMbps is the usable bandwidth of one lane per direction (encoding
// overhead removed).
func pcieMbps(gts float64) float64 {
	switch {
	case gts <= 0:
		return 0
	case gts < 8: // 2.5 and 5 GT/s: 8b/10b
		return gts * 1000 * 8 / 10
	default: // 128b/130b (and PCIe 6 roughly the same)
		return gts * 1000 * 128 / 130
	}
}

func pcieGen(gts float64) string {
	switch {
	case gts <= 2.5:
		return "1.0"
	case gts <= 5:
		return "2.0"
	case gts <= 8:
		return "3.0"
	case gts <= 16:
		return "4.0"
	case gts <= 32:
		return "5.0"
	}
	return "6.0"
}

func gbit(mbps float64) string { return strings.TrimSuffix(fmt.Sprintf("%.1f", mbps/1000), ".0") + " Gbit/s" }

// Analyze turns facts into findings (most severe first).
func Analyze(f Facts) []Finding {
	var out []Finding
	add := func(sev Severity, area, subject, text, advice string) {
		out = append(out, Finding{sev, area, subject, text, advice})
	}

	// PCIe: negotiated link against the card's maximum and its ports' need.
	for _, c := range f.Cards {
		if c.CurGTs <= 0 || c.CurWidth <= 0 {
			continue
		}
		carry := pcieMbps(c.CurGTs) * float64(c.CurWidth)
		could := pcieMbps(c.MaxGTs) * float64(c.MaxWidth)
		link := fmt.Sprintf("PCIe %s x%d", pcieGen(c.CurGTs), c.CurWidth)
		switch {
		case c.NeedMbps > 0 && carry < float64(c.NeedMbps):
			add(Limit, "PCIe", c.Name, fmt.Sprintf("the link (%s) carries %s, its ports (%s) need %s",
				link, gbit(carry), strings.Join(c.Ports, ", "), gbit(float64(c.NeedMbps))),
				fmt.Sprintf("use a slot with PCIe %s x%d (the card's maximum) or a faster one", pcieGen(c.MaxGTs), c.MaxWidth))
		case could > carry*1.01:
			add(Hint, "PCIe", c.Name, fmt.Sprintf("the link runs at %s, the card can do PCIe %s x%d", link, pcieGen(c.MaxGTs), c.MaxWidth),
				"the slot (or the CPU's lanes) limits it; enough for its ports today")
		}
	}

	// Queues, interrupts and CPU.
	for _, p := range f.Ports {
		if f.NumCPU > 1 && p.RXQueues == 1 && !p.RPS {
			add(Hint, "CPU", p.Name, fmt.Sprintf("one receive queue and no receive packet steering: one of %d CPUs handles all its traffic", f.NumCPU),
				"enable RPS (rps_cpus) or use a NIC with several queues")
		}
		if len(p.IRQCPUs) > 1 {
			cpus := slices.Clone(p.IRQCPUs)
			slices.Sort(cpus)
			if cpus[0] == cpus[len(cpus)-1] && f.NumCPU > 1 {
				add(Hint, "CPU", p.Name, fmt.Sprintf("all %d queue interrupts are handled by CPU %d", len(cpus), cpus[0]),
					"spread the interrupts (irqbalance, or smp_affinity per queue)")
			}
		}
	}
	slow := 0
	for _, g := range f.Governors {
		if g == "powersave" || g == "conservative" {
			slow++
		}
	}
	if slow > 0 {
		add(Hint, "CPU", "cpufreq", fmt.Sprintf("%d of %d CPUs use a power-saving frequency governor", slow, len(f.Governors)),
			"the 'performance' (or 'schedutil') governor lowers forwarding latency")
	}

	// NIC settings.
	for _, p := range f.Ports {
		if p.RingRXMax > 0 && p.RingRX < p.RingRXMax {
			add(Hint, "NIC", p.Name, fmt.Sprintf("receive ring %d of %d", p.RingRX, p.RingRXMax), "a larger ring absorbs bursts without drops")
		}
		if p.RingTXMax > 0 && p.RingTX < p.RingTXMax {
			add(Hint, "NIC", p.Name, fmt.Sprintf("transmit ring %d of %d", p.RingTX, p.RingTXMax), "a larger ring absorbs bursts")
		}
		var off []string
		for _, k := range []string{"rx-gro", "tx-checksum-ip-generic", "tx-checksum-ipv4", "tx-tcp-segmentation", "rx-checksum", "rx-vlan-filter"} {
			if p.Features[k] == "off" {
				off = append(off, k)
			}
		}
		if len(off) > 0 {
			add(Hint, "NIC", p.Name, "offloads supported but off: "+strings.Join(off, ", "), "switch them on (ethtool -K) unless they were turned off on purpose")
		}
	}

	// Drops.
	since := "since boot"
	if f.Delta {
		since = "since the last check"
	}
	for _, p := range f.Ports {
		nic := p.RXMissed + p.RXFIFO + p.RXNoBuffer
		if nic > 0 {
			add(Limit, "drops", p.Name, fmt.Sprintf("the NIC dropped %d received frames %s (missed %d, FIFO %d, no buffer %d)",
				nic, since, p.RXMissed, p.RXFIFO, p.RXNoBuffer), "larger receive rings, more queues/CPUs, or pause frames")
		}
		if p.RXDropped > 0 {
			add(Hint, "drops", p.Name, fmt.Sprintf("the kernel dropped %d received frames %s", p.RXDropped, since),
				"often frames nobody wants (unknown protocols, filtered VLANs); check if it grows under load")
		}
	}
	for _, c := range f.CPUs {
		if c.Dropped > 0 {
			add(Limit, "drops", fmt.Sprintf("CPU %d", c.ID), fmt.Sprintf("%d frames dropped from the receive backlog %s", c.Dropped, since),
				"raise net.core.netdev_max_backlog, spread the load over more CPUs")
		}
		if c.TimeSqueeze > 0 {
			add(Hint, "CPU", fmt.Sprintf("CPU %d", c.ID), fmt.Sprintf("the network softirq ran out of time %d times %s", c.TimeSqueeze, since),
				"the CPU is busy with packets; spread the queues or raise net.core.netdev_budget")
		}
	}

	// Memory.
	if f.MemTotal > 0 && f.MemAvail*10 < f.MemTotal {
		add(Limit, "memory", "system", fmt.Sprintf("only %d MB of %d MB available", f.MemAvail/1024, f.MemTotal/1024), "find what uses the memory (start shell, ps)")
	}

	slices.SortStableFunc(out, func(a, b Finding) int {
		if a.Severity != b.Severity {
			if a.Severity == Limit {
				return -1
			}
			return 1
		}
		return 0
	})
	return out
}
