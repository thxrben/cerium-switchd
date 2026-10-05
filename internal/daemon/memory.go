package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/alarms"
	"github.com/thxrben/cerium-switchd/internal/api/bgpapi"
	"github.com/thxrben/cerium-switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/dataplane"
	"github.com/thxrben/cerium-switchd/internal/inventory"
	"github.com/thxrben/cerium-switchd/internal/memslots"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/names"
	"github.com/thxrben/cerium-switchd/internal/supervise"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

// memoryPlanFile keeps the applied plan while the member runs: a restart of
// switchd keeps it (the slots are static), a reload or a reboot makes a
// new one (system memory, reference 5.1).
const memoryPlanFile = "/run/switchd/memory-slots.json"

const memoryAlarm = "switchd/memory"

// appliedMemory is the plan in force on this member.
type appliedMemory struct {
	Config memslots.Config `json:"config"`
	Plan   memslots.Plan   `json:"plan"`
}

// memoryCtl applies the memory slots of this member.
type memoryCtl struct {
	member int
	log    *slog.Logger
	alarms *alarms.Set
	// ports are this member's physical ports (kernel names).
	ports func() []string
	// file is memoryPlanFile (tests: elsewhere); sysRoot "/sys".
	file, sysRoot string
	// run runs a command (tests replace it).
	run func(name string, args ...string) error

	mu      sync.Mutex
	applied *appliedMemory
	// stack is the smallest capacity of every purpose over the members
	// (any member can become master).
	stack map[memslots.Purpose]int
	// onChange runs when the stack's capacities changed (the daemons get
	// them with their configuration).
	onChange func()
}

func newMemoryCtl(member int, log *slog.Logger, al *alarms.Set, ports func() []string) *memoryCtl {
	return &memoryCtl{member: member, log: log, alarms: al, ports: ports, file: memoryPlanFile, sysRoot: "/sys",
		run: command}
}

// start applies the plan kept since this member started, or a new one for
// the active configuration (at boot and after a reload).
func (m *memoryCtl) start(tree *config.Tree) {
	var a appliedMemory
	if b, err := hwio.ReadFile(m.file); err == nil && json.Unmarshal(b, &a) == nil {
		m.log.Info("memory: the slots of this run are kept", "slots", a.Plan.Slots, "enabled", a.Config.Enabled())
	} else {
		cfg, _ := model.Build(tree, nil)
		if cfg == nil {
			cfg = &model.Config{}
		}
		a = appliedMemory{Config: cfg.System.Memory, Plan: memslots.Compute(m.hardware(cfg, tree), cfg.System.Memory)}
		if b, err := json.Marshal(a); err == nil {
			hwio.MkdirAll(filepath.Dir(m.file), 0o755)
			hwio.WriteFile(m.file, b, 0o644)
		}
		m.log.Info("memory: slots computed", "slots", a.Plan.Slots, "allocatable", a.Plan.Allocatable, "enabled", a.Config.Enabled(),
			"problem", a.Plan.Problem)
	}
	m.mu.Lock()
	m.applied = &a
	m.stack = memslots.Min([]memslots.Plan{a.Plan})
	m.mu.Unlock()
	if a.Config.Enabled() && a.Plan.Problem != "" {
		m.alarms.Raise(memoryAlarm, alarms.Major, "memory slots do not fit, memory is dynamic: "+a.Plan.Problem)
	} else {
		m.alarms.Clear(memoryAlarm)
	}
	m.applyKernel()
}

// slotted: the slots are in force (an allocation that fits).
func (a *appliedMemory) slotted() bool {
	return a != nil && a.Config.Enabled() && a.Plan.Problem == ""
}

func (m *memoryCtl) current() *appliedMemory {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applied
}

// Capacity is a purpose's capacity in the stack (0: not limited).
func (m *memoryCtl) Capacity(p memslots.Purpose) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.applied.slotted() {
		return 0
	}
	return m.stack[p]
}

// setStack takes the members' plans (the master collects them).
func (m *memoryCtl) setStack(plans []memslots.Plan) {
	min := memslots.Min(plans)
	m.mu.Lock()
	same := maps.Equal(min, m.stack)
	m.stack = min
	m.mu.Unlock()
	if !same {
		m.applyKernel()
		if m.onChange != nil {
			m.onChange()
		}
	}
}

// UpdateRoom is the update slots' room for a bundle (0: no slots).
func (m *memoryCtl) UpdateRoom() uint64 {
	a := m.current()
	if !a.slotted() {
		return 0
	}
	return a.Plan.UpdateBytes()
}

// applyKernel sets the kernel's table sizes: the neighbour tables always
// (Linux's 1024 entries are too few for a switch), the bridge's MAC and
// multicast limits with slots.
func (m *memoryCtl) applyKernel() {
	a := m.current()
	if a == nil {
		return
	}
	neigh := map[string]int{
		"ipv4": int(a.Plan.RAM / 64 / 512),
		"ipv6": int(a.Plan.RAM / 64 / 512),
	}
	if a.slotted() {
		if c := m.Capacity(memslots.ARP); c > 0 {
			neigh["ipv4"] = c
		}
		if c := m.Capacity(memslots.NDP); c > 0 {
			neigh["ipv6"] = c
		}
	}
	for fam, n := range neigh {
		n = max(n, 1024)
		for i, v := range []int{n / 4, n * 7 / 8, n} {
			p := filepath.Join("/proc/sys/net", fam, "neigh/default", fmt.Sprintf("gc_thresh%d", i+1))
			if err := hwio.WriteFile(p, []byte(strconv.Itoa(v)), 0o644); err != nil {
				m.log.Warn("memory: neighbour table size not set", "file", p, "err", err)
			}
		}
	}
	if !a.slotted() {
		return
	}
	var args []string
	if c := m.Capacity(memslots.MAC); c > 0 {
		args = append(args, "fdb_max_learned", strconv.Itoa(c))
	}
	if c := m.Capacity(memslots.Multicast); c > 0 {
		args = append(args, "mcast_hash_max", strconv.Itoa(c))
	}
	if len(args) > 0 {
		if err := m.run("ip", append([]string{"link", "set", names.Bridge, "type", "bridge"}, args...)...); err != nil {
			m.log.Warn("memory: bridge limits not set", "err", err)
		}
	}
}

// hardware is what this member has and needs before the slots.
func (m *memoryCtl) hardware(cfg *model.Config, tree *config.Tree) memslots.Hardware {
	hw := memslots.Hardware{RAM: meminfo("MemTotal")}
	if b, err := hwio.ReadFile("/proc/sys/vm/min_free_kbytes"); err == nil {
		kb, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		hw.MinFree = kb << 10
	}
	for _, p := range m.ports() {
		hw.NICRings += m.rxRing(p)
	}
	for _, b := range memslots.DaemonBase {
		hw.Daemons += b
	}
	f := memslots.Facts{Interfaces: len(cfg.Interfaces) + len(cfg.L3), VLANs: len(cfg.VLANs), Members: len(cfg.Members),
		StaticRoutes: len(cfg.Routes), Ports: len(m.ports())}
	for _, v := range cfg.VLANs {
		if v.VNI != 0 {
			f.VNIs++
		}
	}
	if tree != nil {
		f.ConfigBytes = uint64(len(config.FormatCurly(tree.Root)))
	}
	hw.System = memslots.SystemArea(f)
	return hw
}

// rxRing is the memory of a port's receive rings: every descriptor has a
// buffer (2 KiB, a page for jumbo frames) in every receive queue.
func (m *memoryCtl) rxRing(port string) uint64 {
	r, ok := inventory.ReadRings(port)
	if !ok || r.RX == 0 {
		r.RX = 256
	}
	queues, _ := hwio.Glob(filepath.Join(m.sysRoot, "class/net", port, "queues/rx-*"))
	buf := uint64(2048)
	if b, err := hwio.ReadFile(filepath.Join(m.sysRoot, "class/net", port, "mtu")); err == nil {
		if mtu, _ := strconv.Atoi(strings.TrimSpace(string(b))); mtu > 1500 {
			buf = 4096 * uint64((mtu+4095)/4096)
		}
	}
	return uint64(r.RX) * uint64(max(len(queues), 1)) * buf
}

// meminfo reads a /proc/meminfo value in bytes.
func meminfo(key string) uint64 {
	b, err := hwio.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == key+":" {
			kb, _ := strconv.ParseUint(f[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}

// critical daemons keep their memory (and program code) under pressure.
var criticalDaemons = map[string]bool{"cer-lacpd": true, "cer-bfdd": true, "cer-rstpd": true, "cer-mclagd": true, "cer-ribd": true}

// limits are a daemon's memory limits from this member's own plan (static
// while switchd runs; the stack's minimum may change and must not restart
// the daemons). Without slots there are none.
func (m *memoryCtl) limits(program string) supervise.Limits {
	a := m.current()
	if !a.slotted() {
		return supervise.Limits{}
	}
	c := func(p memslots.Purpose) uint64 { return uint64(a.Plan.Capacity(p)) }
	g := func(name string) uint64 { return uint64(memslots.Go[name]) }
	base := memslots.DaemonBase[program]
	var tables uint64
	switch program {
	case "cer-bgpd":
		tables = (c(memslots.BGPv4)+c(memslots.BGPv6))*g("bgp/prefix") + c(memslots.BGPPaths)*g("bgp/path")
	case "cer-ribd":
		tables = (c(memslots.BGPv4)+c(memslots.BGPv6)+c(memslots.BGPPaths)/2)*g("rib/bgp") + c(memslots.OSPF)*g("rib/ospf")
	case "cer-ospfd":
		tables = c(memslots.OSPF) * g("ospf/route")
	case "cer-mclagd":
		tables = c(memslots.MAC) * g("mclag/mac")
	}
	if base == 0 {
		return supervise.Limits{}
	}
	// Room for Go's garbage collector: the heap grows past the live data
	// before a collection.
	l := supervise.Limits{GoLimit: (base + tables) * 13 / 10}
	l.Max = l.GoLimit * 3 / 2
	if criticalDaemons[program] {
		l.Min = base
	}
	return l
}

// watchStack collects every member's plan (each answers "mem-plan") once a
// minute: a purpose's capacity in the stack is the smallest one.
func (m *memoryCtl) watchStack(ctx context.Context, ctl *stackCtl) {
	if ctl == nil {
		return
	}
	ctl.node.Handle("mem-plan", func(int, json.RawMessage) (any, error) { return m.current(), nil })
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		var plans []memslots.Plan
		if a := m.current(); a.slotted() {
			plans = append(plans, a.Plan)
		}
		for id := range ctl.node.Members() {
			if id == m.member {
				continue
			}
			raw, err := ctl.node.Call(id, "mem-plan", nil, 5*time.Second)
			var a appliedMemory
			if err == nil && json.Unmarshal(raw, &a) == nil && a.slotted() {
				plans = append(plans, a.Plan)
			}
		}
		m.setStack(plans)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Memory is show system memory (reference 5.1).
func (o *ops) Memory() (cli.MemoryStatus, error) {
	st := cli.MemoryStatus{Member: o.member, Available: meminfo("MemAvailable")}
	if o.mem == nil {
		return st, errors.New("memory slots are not set up")
	}
	a := o.mem.current()
	if a == nil {
		return st, errors.New("memory slots are not set up")
	}
	p := a.Plan
	st.Slotted, st.RAM, st.Kernel, st.NICRings, st.Daemons, st.Mgmt, st.Margin = a.slotted(), p.RAM, p.Kernel, p.NICRings, p.Daemons, p.Mgmt, p.Margin
	st.Slots, st.System, st.Update, st.Allocatable, st.Dynamic = p.Slots, p.System, p.Update, p.Allocatable, p.Dynamic
	if a.Config.Enabled() {
		st.Problem = p.Problem
	}
	if cfg := o.model(); cfg != nil {
		st.Pending = !reflect.DeepEqual(cfg.System.Memory, a.Config)
	}
	counts := o.memoryUse()
	for _, pu := range memslots.Purposes {
		mp := cli.MemoryPurpose{Name: string(pu), Bytes: memslots.Costs[pu], PerSlot: memslots.PerSlot(pu),
			AllSlots: p.AllFor(pu), Used: -1, Capacity: o.mem.Capacity(pu)}
		if st.Slotted {
			mp.Slots = p.Purposes[pu].Slots
		}
		if n, ok := counts[pu]; ok {
			mp.Used = n
		}
		st.Purposes = append(st.Purposes, mp)
	}
	st.NeighV4, st.NeighV6 = sysctlInt("/proc/sys/net/ipv4/neigh/default/gc_thresh3"), sysctlInt("/proc/sys/net/ipv6/neigh/default/gc_thresh3")
	if o.updater != nil {
		st.UpdateRoom, _ = o.updater.store.room()
	}
	return st, nil
}

// memoryUse counts the entries of the purposes this member can count.
func (o *ops) memoryUse() map[memslots.Purpose]int {
	out := map[memslots.Purpose]int{}
	if b, err := hwio.ReadFile("/proc/net/arp"); err == nil {
		out[memslots.ARP] = max(strings.Count(string(b), "\n")-1, 0)
	}
	if n, err := o.Neighbors(true); err == nil {
		out[memslots.NDP] = len(n)
	}
	if o.svc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// BGP: what cer-bgpd holds (it runs on the master).
		var c bgpapi.Counts
		if err := o.svc.call(ctx, "cer-bgpd", bgpapi.MethodCounts, nil, &c); err == nil {
			out[memslots.BGPv4], out[memslots.BGPv6], out[memslots.BGPPaths] = c.IPv4, c.IPv6, c.Paths
		} else if strings.Contains(err.Error(), "not running") {
			out[memslots.BGPv4], out[memslots.BGPv6], out[memslots.BGPPaths] = 0, 0, 0
		}
		// OSPF: the routes in cer-ribd.
		var sums []rib.Summary
		if err := o.svc.call(ctx, "cer-ribd", svc.MethodRouteSummary, nil, &sums); err == nil {
			n := 0
			for _, s := range sums {
				n += s.PerProtocol[rib.OSPF][0]
			}
			out[memslots.OSPF] = n
		}
	}
	if o.kernel != nil {
		if fdb, err := o.kernel.FDB(); err == nil {
			n := 0
			for _, e := range fdb {
				if !e.Static {
					n++
				}
			}
			out[memslots.MAC] = n
		}
		// Multicast: the bridge's memberships (netlink MDB dump).
		if es, _, err := dataplane.McastGroups(); err == nil {
			out[memslots.Multicast] = len(es)
		}
	}
	return out
}

func sysctlInt(path string) int {
	b, err := hwio.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}
