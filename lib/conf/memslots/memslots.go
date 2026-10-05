// Package memslots divides a member's memory into slots (system memory,
// reference 5.1): a fixed part for the kernel, the NICs, the programs and
// the management services, then slots of 4 MiB, each for one purpose,
// holding a whole number of entries.
package memslots

import (
	"fmt"
	"sort"
)

// SlotSize is the size of a slot: 1024 pages.
const SlotSize = 4 << 20

// Purpose is what a slot holds.
type Purpose string

const (
	BGPv4     Purpose = "bgp-ipv4"
	BGPv6     Purpose = "bgp-ipv6"
	BGPPaths  Purpose = "bgp-paths"
	OSPF      Purpose = "ospf"
	ARP       Purpose = "arp"
	NDP       Purpose = "ndp"
	MAC       Purpose = "mac"
	Multicast Purpose = "multicast"
)

// Purposes in the order they are shown.
var Purposes = []Purpose{BGPv4, BGPv6, BGPPaths, OSPF, ARP, NDP, MAC, Multicast}

// PerSlot is how many entries of p one slot holds.
func PerSlot(p Purpose) int { return SlotSize / Costs[p] }

// Amount is one purpose's allocation: Percent of the allocatable slots or
// a number of Slots.
type Amount struct {
	Percent int
	Slots   int
}

// Config is system memory.
type Config struct {
	// Alloc is the allocation (nil or empty: dynamic memory, no slots).
	Alloc       map[Purpose]Amount
	UpdateSize  uint64
	MgmtReserve uint64
}

// Defaults of system memory.
const (
	DefaultUpdateSize  = 512 << 20
	DefaultMgmtReserve = 384 << 20
)

// Hardware is what a member has and needs before any slot.
type Hardware struct {
	RAM     uint64 // MemTotal
	MinFree uint64 // the kernel's min_free_kbytes, in bytes
	// NICRings is the memory of the NICs' receive rings.
	NICRings uint64
	// Daemons is the base memory of switchd and the daemons that run.
	Daemons uint64
	// System is the system area's bytes (SystemArea).
	System uint64
}

// Plan is how a member's memory is divided.
type Plan struct {
	RAM, Kernel, NICRings, Daemons, Mgmt, Margin uint64
	// Slots is every slot; System and Update are the system area's and
	// the update slots; Allocatable what remains for the purposes.
	Slots, System, Update, Allocatable int
	Purposes                           map[Purpose]Share
	// Dynamic is the slots no purpose has.
	Dynamic int
	// Problem: the allocation does not fit ("": it does); the member uses
	// dynamic memory then.
	Problem string
}

// Share is one purpose's part of a plan.
type Share struct {
	Slots    int
	Capacity int
}

// Enabled: memory is allocated in slots.
func (c Config) Enabled() bool { return len(c.Alloc) > 0 }

func slotsFor(n uint64) int { return int((n + SlotSize - 1) / SlotSize) }

// Compute divides a member's memory.
func Compute(hw Hardware, cfg Config) Plan {
	if cfg.UpdateSize == 0 {
		cfg.UpdateSize = DefaultUpdateSize
	}
	if cfg.MgmtReserve == 0 {
		cfg.MgmtReserve = DefaultMgmtReserve
	}
	p := Plan{RAM: hw.RAM, NICRings: hw.NICRings, Daemons: hw.Daemons, Mgmt: cfg.MgmtReserve,
		Kernel: hw.RAM*16/1000 + 128<<20, Margin: max(hw.RAM*5/100, 3*hw.MinFree)}
	fixed := p.Kernel + p.NICRings + p.Daemons + p.Mgmt + p.Margin
	if fixed < hw.RAM {
		p.Slots = int((hw.RAM - fixed) / SlotSize)
	}
	p.System = max(slotsFor(hw.System), 1)
	p.Update = slotsFor(cfg.UpdateSize) + 1
	p.Allocatable = p.Slots - p.System - p.Update
	if p.Allocatable < 0 {
		p.Problem = fmt.Sprintf("the fixed part, the system area (%d slots) and the update slots (%d) need more than the %d slots of this member",
			p.System, p.Update, p.Slots)
		p.Allocatable = 0
	}
	p.Purposes = map[Purpose]Share{}
	used := 0
	for _, pu := range Purposes {
		a, ok := cfg.Alloc[pu]
		if !ok {
			continue
		}
		n := a.Slots
		if a.Percent > 0 {
			n = p.Allocatable * a.Percent / 100
		}
		p.Purposes[pu] = Share{Slots: n, Capacity: n * PerSlot(pu)}
		used += n
	}
	if used > p.Allocatable && p.Problem == "" {
		p.Problem = fmt.Sprintf("the allocation needs %d slots, this member has %d", used, p.Allocatable)
	}
	p.Dynamic = max(p.Allocatable-used, 0)
	return p
}

// Capacity is a purpose's capacity (0: no slots for it).
func (p Plan) Capacity(pu Purpose) int { return p.Purposes[pu].Capacity }

// UpdateBytes is the room of the update slots for a bundle (the write
// buffer's slot left out).
func (p Plan) UpdateBytes() uint64 { return uint64(max(p.Update-1, 0)) * SlotSize }

// AllFor is what this member would hold of pu if every allocatable slot
// went to it.
func (p Plan) AllFor(pu Purpose) int { return p.Allocatable * PerSlot(pu) }

// Min is the smallest capacity of every purpose over the members' plans
// that fit (any member can become master: the stack can only rely on
// that).
func Min(plans []Plan) map[Purpose]int {
	out := map[Purpose]int{}
	for _, p := range plans {
		if p.Problem != "" {
			continue
		}
		for pu, c := range p.Purposes {
			if v, seen := out[pu]; !seen || c.Capacity < v {
				out[pu] = c.Capacity
			}
		}
	}
	return out
}

// Sorted lists a plan's purposes in order.
func (p Plan) Sorted() []Purpose {
	var out []Purpose
	for pu := range p.Purposes {
		out = append(out, pu)
	}
	sort.Slice(out, func(i, j int) bool { return index(out[i]) < index(out[j]) })
	return out
}

func index(p Purpose) int {
	for i, x := range Purposes {
		if x == p {
			return i
		}
	}
	return len(Purposes)
}
