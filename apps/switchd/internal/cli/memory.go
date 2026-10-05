package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/lib/conf/config"
	"github.com/thxrben/cerium-switchd/lib/conf/model"
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

// MemoryStack is implemented in a stack: every member's plan (a partial
// error names the members that did not answer).
type MemoryStack interface {
	MemoryAll() ([]MemoryStatus, error)
}

// smallestMember picks the member with the fewest slots to allocate: the
// allocation must fit every member (any of them can become master).
func smallestMember(ms []MemoryStatus) (MemoryStatus, bool) {
	var best MemoryStatus
	found := false
	for _, m := range ms {
		if !found || m.Allocatable < best.Allocatable {
			best, found = m, true
		}
	}
	return best, found
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

// memorySetup is "request system memory setup" (reference 3.5): it asks
// per purpose for a percentage or a number of slots, shows the resulting
// capacities, and writes system memory into the shared candidate.
func (sh *Shell) memorySetup(c *call) error {
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
	members := 1
	if st, ok := sh.env.Ops.(MemoryStack); ok {
		all, err := st.MemoryAll()
		if err != nil {
			if pe := (*PartialError)(nil); errors.As(err, &pe) {
				fmt.Fprintf(c.out, "warning: %v; the slots below may not fit those members\n", err)
			} else {
				return err
			}
		}
		if s, ok := smallestMember(all); ok {
			m, members = s, len(all)
		}
	}
	if m.Allocatable <= 0 {
		return fmt.Errorf("member %d has no slots to allocate (%d slots, system area %d, update %d)", m.Member, m.Slots, m.System, m.Update)
	}
	type choice struct{ percent, slots int }
	cur := map[string]choice{}
	if cfg, _ := model.Build(sh.env.Engine.Active(), nil); cfg != nil {
		for p, a := range cfg.System.Memory.Alloc {
			cur[string(p)] = choice{a.Percent, a.Slots}
		}
	}
	slotsOf := func(ch choice) int {
		if ch.percent > 0 {
			return m.Allocatable * ch.percent / 100
		}
		return ch.slots
	}
	show := func(ch choice) string {
		switch {
		case ch.percent > 0:
			return strconv.Itoa(ch.percent) + "%"
		case ch.slots > 0:
			return strconv.Itoa(ch.slots)
		}
		return "-"
	}
	if members > 1 {
		fmt.Fprintf(c.out, "The smallest of %d members is member %d: %d slots of 4 MiB to allocate (after the system area and the update slots); the allocation must fit it.\n",
			members, m.Member, m.Allocatable)
	} else {
		fmt.Fprintf(c.out, "Member %d: %d slots of 4 MiB to allocate (after the system area and the update slots).\n", m.Member, m.Allocatable)
	}
	c.out.WriteString("Answer per purpose: a percentage (30%), a number of slots (120), '-' for none, '?' for help, or nothing to keep.\n\n")
	for _, p := range m.Purposes {
		for {
			a, err := c.term.Ask(fmt.Sprintf("%-10s (%d bytes, %d per slot) [%s]: ", p.Name, p.Bytes, p.PerSlot, show(cur[p.Name])), true)
			if err != nil {
				return nil // interrupted: nothing written
			}
			a = strings.TrimSpace(a)
			var ch choice
			switch {
			case a == "":
				ch = cur[p.Name]
			case a == "-":
			case a == "?":
				for _, q := range m.Purposes {
					fmt.Fprintf(c.out, "  %-10s %5d bytes per entry, %6d per slot, %d with every slot\n", q.Name, q.Bytes, q.PerSlot, q.AllSlots)
				}
				continue
			case strings.HasSuffix(a, "%"):
				n, err := strconv.Atoi(strings.TrimSuffix(a, "%"))
				if err != nil || n < 1 || n > 100 {
					c.out.WriteString("  a percentage is 1% to 100%\n")
					continue
				}
				ch.percent = n
			default:
				n, err := strconv.Atoi(a)
				if err != nil || n < 1 {
					c.out.WriteString("  expecting a percentage (30%), a number of slots, '-' or nothing\n")
					continue
				}
				ch.slots = n
			}
			if ch == (choice{}) {
				delete(cur, p.Name)
			} else {
				cur[p.Name] = ch
			}
			n, total := slotsOf(ch), 0
			for _, x := range cur {
				total += slotsOf(x)
			}
			if n > 0 {
				fmt.Fprintf(c.out, "  -> %d slots, %d entries; %d of %d slots left\n", n, n*p.PerSlot, m.Allocatable-total, m.Allocatable)
			}
			break
		}
	}
	total, percent := 0, 0
	for _, x := range cur {
		total += slotsOf(x)
		percent += x.percent
	}
	if total > m.Allocatable || percent > 100 {
		fmt.Fprintf(c.out, "\nThe allocation needs %d slots, this member has %d: nothing written.\n", total, m.Allocatable)
		return nil
	}
	lines := "delete system memory allocation\n"
	for _, p := range m.Purposes {
		ch, ok := cur[p.Name]
		switch {
		case !ok:
		case ch.percent > 0:
			lines += fmt.Sprintf("set system memory allocation %s percent %d\n", p.Name, ch.percent)
		default:
			lines += fmt.Sprintf("set system memory allocation %s slots %d\n", p.Name, ch.slots)
		}
	}
	fmt.Fprintf(c.out, "\n%s", lines)
	a, err := c.term.Ask("Write this into the candidate configuration? [yes,no] (no) ", true)
	if err != nil || !isYes(a) {
		return nil
	}
	s, _, err := sh.env.Engine.Configure(sh.env.User, sh.env.Class, commit.Shared)
	if err != nil {
		return err
	}
	err = s.Modify(func(t *config.Tree) error { return config.ApplySetLines(t, lines) })
	s.Close()
	if err != nil {
		return err
	}
	c.out.WriteString("Written into the candidate configuration: 'configure', 'show | compare', 'commit'. It applies at the next 'request system reload'.\n")
	return nil
}
