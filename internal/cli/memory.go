package cli

import (
	"errors"
	"fmt"
	"strconv"
)

// MemoryPurpose is one purpose of the memory slots.
type MemoryPurpose struct {
	Name           string
	Bytes, PerSlot int // per entry; entries per slot
	Slots          int // allocated (0: none)
	Capacity       int // in force in the stack (0: no limit)
	AllSlots       int // what this member would hold with every slot
	Used           int // entries now (-1: unknown)
}

// MemoryStatus is show system memory of one member (reference 5.1).
type MemoryStatus struct {
	Member int
	// Slotted: an allocation is in force; Problem: it did not fit.
	Slotted bool
	Problem string
	// Pending: the configuration changed system memory since the start or
	// the last reload.
	Pending                                      bool
	RAM, Kernel, NICRings, Daemons, Mgmt, Margin uint64
	Slots, System, Update, Allocatable, Dynamic  int
	Purposes                                     []MemoryPurpose
	Available                                    uint64 // MemAvailable now
	UpdateRoom                                   uint64 // room for a bundle now
	NeighV4, NeighV6                             int    // the kernel's neighbour table sizes
}

// Purpose returns a purpose by name.
func (m *MemoryStatus) Purpose(name string) *MemoryPurpose {
	if m == nil {
		return nil
	}
	for i := range m.Purposes {
		if m.Purposes[i].Name == name {
			return &m.Purposes[i]
		}
	}
	return nil
}

// Memory is implemented by switches with memory slots.
type Memory interface {
	Memory() (MemoryStatus, error)
}

func mb(n uint64) string { return strconv.FormatUint((n+(1<<20)-1)>>20, 10) + " MB" }

func used(n int) string {
	if n < 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

// showMemory is "show system memory".
func (sh *Shell) showMemory(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	o, ok := sh.env.Ops.(Memory)
	if sh.env.Ops == nil || !ok {
		return errors.New("memory information is not available")
	}
	m, err := o.Memory()
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Memory of member %d: %s RAM, %s available now\n", m.Member, mb(m.RAM), mb(m.Available))
	switch {
	case m.Problem != "":
		fmt.Fprintf(c.out, "Slots: NOT IN FORCE, memory is dynamic: %s\n", m.Problem)
	case !m.Slotted:
		c.out.WriteString("Slots: none (system memory allocation is not configured): memory is dynamic\n")
	}
	if m.Pending {
		c.out.WriteString("The configuration changed system memory: it applies at the next 'request system reload'\n")
	}
	fmt.Fprintf(c.out, "\nFixed part\n")
	for _, r := range []struct {
		n string
		v uint64
	}{{"Kernel", m.Kernel}, {"NIC receive rings", m.NICRings}, {"Programs (base)", m.Daemons}, {"Management reserve", m.Mgmt}, {"Margin", m.Margin}} {
		fmt.Fprintf(c.out, "  %-24s %10s\n", r.n+":", mb(r.v))
	}
	fmt.Fprintf(c.out, "\nSlots of 4 MiB: %d\n", m.Slots)
	fmt.Fprintf(c.out, "  %-24s %10d\n", "System area:", m.System)
	fmt.Fprintf(c.out, "  %-24s %10d  (bundles up to %s)\n", "Update:", m.Update, mb(uint64(max(m.Update-1, 0))*4<<20))
	fmt.Fprintf(c.out, "  %-24s %10d\n", "Allocatable:", m.Allocatable)
	fmt.Fprintf(c.out, "\n  %-10s %7s %8s %6s %10s %10s %6s %12s\n", "Purpose", "Bytes", "Per slot", "Slots", "Capacity", "Entries", "Full", "All slots")
	for _, p := range m.Purposes {
		slots, capa, full := "-", "-", "-"
		if p.Slots > 0 {
			slots, capa = strconv.Itoa(p.Slots), strconv.Itoa(p.Capacity)
			if p.Used >= 0 && p.Capacity > 0 {
				full = strconv.Itoa(p.Used*100/p.Capacity) + "%"
			}
		}
		fmt.Fprintf(c.out, "  %-10s %7d %8d %6s %10s %10s %6s %12d\n", p.Name, p.Bytes, p.PerSlot, slots, capa, used(p.Used), full, p.AllSlots)
	}
	if m.Slotted {
		fmt.Fprintf(c.out, "  %-10s %24d\n", "dynamic", m.Dynamic)
	}
	fmt.Fprintf(c.out, "\nKernel neighbour tables: IPv4 %d, IPv6 %d entries\n", m.NeighV4, m.NeighV6)
	fmt.Fprintf(c.out, "Room for a software bundle now: %s\n", mb(m.UpdateRoom))
	return nil
}
