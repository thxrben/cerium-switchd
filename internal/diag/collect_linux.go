//go:build linux

package diag

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/thxrben/cerium-switchd/internal/inventory"
	"github.com/thxrben/cerium-switchd/internal/schema"
)

// PortRef names a port of this member.
type PortRef struct {
	Name, Linux string
}

// Collector gathers facts; it keeps the counters of its previous run, so
// that a later run reports increases.
type Collector struct {
	SysRoot  string // "/sys"
	ProcRoot string // "/proc"

	mu   sync.Mutex
	prev map[string]uint64
}

func (c *Collector) root() (string, string) {
	s, p := c.SysRoot, c.ProcRoot
	if s == "" {
		s = "/sys"
	}
	if p == "" {
		p = "/proc"
	}
	return s, p
}

func readStr(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func readUint(path string) uint64 {
	v, _ := strconv.ParseUint(readStr(path), 10, 64)
	return v
}

// gts parses "8.0 GT/s PCIe" (or "Unknown").
func gts(s string) float64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// Collect gathers the facts of ports (the member's physical ports).
func (c *Collector) Collect(ports []PortRef) Facts {
	sys, proc := c.root()
	f := Facts{NumCPU: runtime.NumCPU()}
	irqs := irqsByName(proc)

	cards := map[string]*Card{}
	var order []string
	for _, pr := range ports {
		net := filepath.Join(sys, "class", "net", pr.Linux)
		p := Port{Name: pr.Name, Linux: pr.Linux, Features: inventory.ReadCaps(sys, pr.Linux).Features}
		if readStr(filepath.Join(net, "carrier")) == "1" {
			if v, err := strconv.Atoi(readStr(filepath.Join(net, "speed"))); err == nil && v > 0 {
				p.SpeedMbps = v
			}
		}
		qs, _ := filepath.Glob(filepath.Join(net, "queues", "rx-*"))
		p.RXQueues = len(qs)
		for _, q := range qs {
			if m := strings.Trim(readStr(filepath.Join(q, "rps_cpus")), "0,"); m != "" {
				p.RPS = true
			}
		}
		for _, irq := range irqs[pr.Linux] {
			p.IRQs = append(p.IRQs, irq)
			cpu, _ := strconv.Atoi(strings.SplitN(strings.SplitN(readStr(filepath.Join(proc, "irq", strconv.Itoa(irq), "effective_affinity_list")), ",", 2)[0], "-", 2)[0])
			p.IRQCPUs = append(p.IRQCPUs, cpu)
		}
		if r, ok := inventory.ReadRings(pr.Linux); ok {
			p.RingRX, p.RingRXMax, p.RingTX, p.RingTXMax = r.RX, r.RXMax, r.TX, r.TXMax
		}
		st := filepath.Join(net, "statistics")
		p.RXMissed = c.delta(pr.Linux+" missed", readUint(filepath.Join(st, "rx_missed_errors")))
		p.RXFIFO = c.delta(pr.Linux+" fifo", readUint(filepath.Join(st, "rx_fifo_errors")))
		p.RXNoBuffer = c.delta(pr.Linux+" nobuf", readUint(filepath.Join(st, "rx_over_errors")))
		p.RXDropped = c.delta(pr.Linux+" dropped", readUint(filepath.Join(st, "rx_dropped")))
		f.Ports = append(f.Ports, p)

		// The PCIe link of the port's PCI device (a card's functions share
		// it).
		dev, err := filepath.EvalSymlinks(filepath.Join(net, "device"))
		if err != nil || readStr(filepath.Join(dev, "current_link_speed")) == "" {
			continue
		}
		key := filepath.Base(dev) // 0000:04:00.1 -> the card 0000:04:00
		if i := strings.LastIndexByte(key, '.'); i > 0 {
			key = key[:i]
		}
		cd := cards[key]
		if cd == nil {
			name := key
			if pp, ok := schema.ParsePhysical(pr.Name); ok {
				name = fmt.Sprintf("card %d (%s)", pp.Card, key)
			}
			cd = &Card{Name: name, CurGTs: gts(readStr(filepath.Join(dev, "current_link_speed"))), MaxGTs: gts(readStr(filepath.Join(dev, "max_link_speed")))}
			cd.CurWidth, _ = strconv.Atoi(readStr(filepath.Join(dev, "current_link_width")))
			cd.MaxWidth, _ = strconv.Atoi(readStr(filepath.Join(dev, "max_link_width")))
			cards[key] = cd
			order = append(order, key)
		}
		cd.Ports = append(cd.Ports, pr.Name)
		need := p.SpeedMbps
		if need == 0 {
			need = inventory.ReadCaps(sys, pr.Linux).MaxSpeedMbps
		}
		cd.NeedMbps += need
	}
	sort.Strings(order)
	for _, k := range order {
		f.Cards = append(f.Cards, *cards[k])
	}

	// softnet_stat: one line per CPU; columns processed, dropped,
	// time_squeeze (hex).
	if fh, err := os.Open(filepath.Join(proc, "net", "softnet_stat")); err == nil {
		sc := bufio.NewScanner(fh)
		for id := 0; sc.Scan(); id++ {
			col := strings.Fields(sc.Text())
			if len(col) < 3 {
				continue
			}
			hex := func(s string) uint64 { v, _ := strconv.ParseUint(s, 16, 64); return v }
			cpu := id
			if len(col) >= 13 {
				cpu = int(hex(col[12])) // the CPU id column (newer kernels)
			}
			f.CPUs = append(f.CPUs, CPU{ID: cpu,
				Dropped:     c.delta(fmt.Sprintf("cpu%d dropped", cpu), hex(col[1])),
				TimeSqueeze: c.delta(fmt.Sprintf("cpu%d squeeze", cpu), hex(col[2]))})
		}
		fh.Close()
	}
	govs, _ := filepath.Glob(filepath.Join(sys, "devices", "system", "cpu", "cpu[0-9]*", "cpufreq", "scaling_governor"))
	for _, g := range govs {
		f.Governors = append(f.Governors, readStr(g))
	}
	for _, l := range strings.Split(readStr(filepath.Join(proc, "meminfo")), "\n") {
		fs := strings.Fields(l)
		if len(fs) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fs[1], 10, 64)
		switch fs[0] {
		case "MemTotal:":
			f.MemTotal = v
		case "MemAvailable:":
			f.MemAvail = v
		}
	}
	c.mu.Lock()
	f.Delta = c.prev != nil && c.prev["#run"] > 0
	if c.prev == nil {
		c.prev = map[string]uint64{}
	}
	c.prev["#run"]++
	c.mu.Unlock()
	return f
}

// delta returns the increase of a counter since the previous run (the
// total on the first run).
func (c *Collector) delta(key string, v uint64) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prev == nil {
		c.prev = map[string]uint64{}
	}
	old, seen := c.prev[key]
	c.prev[key] = v
	if !seen || c.prev["#run"] == 0 || v < old {
		return v
	}
	return v - old
}

// irqsByName maps interface names to the interrupts named after them
// (/proc/interrupts: "ens1f0-TxRx-0", "eth0", "enp1s0-rx-1" ...).
func irqsByName(proc string) map[string][]int {
	out := map[string][]int{}
	fh, err := os.Open(filepath.Join(proc, "interrupts"))
	if err != nil {
		return out
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		irq, err := strconv.Atoi(strings.TrimSpace(line[:i]))
		if err != nil {
			continue
		}
		fs := strings.Fields(line)
		name := fs[len(fs)-1]
		base := name
		if j := strings.IndexAny(name, "-@"); j > 0 {
			base = name[:j]
		}
		out[base] = append(out[base], irq)
	}
	return out
}
