// Package routing runs switchd's routing (reference 5.8): it owns the RIB,
// feeds it the connected and static routes of every instance, hosts the
// routing protocols (OSPF, OSPFv3, BGP) and gives the data plane the
// active routes of the protocols to install.
//
// Static routes are still installed by the data plane directly (they are
// part of every member's configuration); the RIB holds them too, so that
// "show route" and the preference rules see every source, and a protocol
// route only reaches the kernel when it is active, i.e. better than any
// static route of the same prefix.
package routing

import (
	"log/slog"
	"net/netip"
	"slices"
	"sync"

	"mclag/internal/model"
	"mclag/internal/rib"
)

// FIBRoute is an active protocol route for the kernel.
type FIBRoute struct {
	Instance    string
	Prefix      netip.Prefix
	NextHops    []rib.NextHop
	Discard     bool
	KernelProto int // the kernel protocol id (OSPF 188, BGP 186)
}

// Kernel protocol ids (reference 5.8).
const (
	KernelOSPF = 188
	KernelBGP  = 186
)

// Protocol is a running routing protocol instance (OSPF, BGP).
type Protocol interface {
	// Reconfigure applies a new configuration hitlessly.
	Reconfigure(cfg *model.Config, r *model.Routing)
	// Stop ends the protocol (its routes are withdrawn by the manager).
	Stop()
}

// Factory starts protocol instances; nil fields mean "not available".
type Factory struct {
	OSPF func(m *Manager, instance string, v3 bool) Protocol
	BGP  func(m *Manager, instance string) Protocol
}

// Manager is the routing of a member.
type Manager struct {
	RIB *rib.RIB
	Log *slog.Logger
	// Changed is called (without locks held) after the active protocol
	// routes changed: the data plane reconciles.
	Changed func()
	Factory Factory

	mu     sync.Mutex
	cfg    *model.Config
	protos map[string]Protocol // "<instance>/<proto>"
	// fib: the active protocol routes as last computed.
	fib map[rib.Table]map[netip.Prefix]FIBRoute
}

// New returns a manager with an empty RIB.
func New(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{RIB: rib.New(nil), Log: log, protos: map[string]Protocol{}, fib: map[rib.Table]map[netip.Prefix]FIBRoute{}}
}

// Config returns the configuration last applied.
func (m *Manager) Config() *model.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// Apply takes a new configuration: connected and static routes are
// replaced in the RIB, protocols are started, reconfigured or stopped.
func (m *Manager) Apply(cfg *model.Config) {
	m.mu.Lock()
	m.cfg = cfg
	instances := map[string]bool{"": true}
	for n := range cfg.Instances {
		if n != cfg.System.MgmtInstance {
			instances[n] = true
		}
	}
	for inst := range instances {
		m.RIB.Set(inst, rib.Direct, "", connected(cfg, inst, false))
		m.RIB.Set(inst, rib.Local, "", connected(cfg, inst, true))
		m.RIB.Set(inst, rib.Static, "", static(cfg, inst))
	}
	// Instances that are gone (or became the management instance).
	for key, p := range m.protos {
		inst := instanceOf(key)
		if !instances[inst] {
			p.Stop()
			delete(m.protos, key)
			m.RIB.DropInstance(inst)
		}
	}
	m.reconcileProtocols(cfg, instances)
	m.mu.Unlock()
	m.Recompute()
}

func instanceOf(key string) string {
	i := len(key) - 1
	for i >= 0 && key[i] != '/' {
		i--
	}
	if i < 0 {
		return ""
	}
	return key[:i]
}

// reconcileProtocols starts, reconfigures and stops protocol instances.
func (m *Manager) reconcileProtocols(cfg *model.Config, instances map[string]bool) {
	want := map[string]*model.Routing{}
	for _, r := range cfg.AllRouting() {
		if !instances[r.Instance] {
			continue
		}
		if r.OSPF != nil && !r.OSPF.Disabled && m.Factory.OSPF != nil {
			want[r.Instance+"/ospf"] = r
		}
		if r.OSPF3 != nil && !r.OSPF3.Disabled && m.Factory.OSPF != nil {
			want[r.Instance+"/ospf3"] = r
		}
		if r.BGP != nil && !r.BGP.Disabled && m.Factory.BGP != nil {
			want[r.Instance+"/bgp"] = r
		}
	}
	for key, p := range m.protos {
		if want[key] == nil {
			p.Stop()
			delete(m.protos, key)
			inst := instanceOf(key)
			switch key[len(inst)+1:] {
			case "ospf":
				m.RIB.Set(inst, rib.OSPF, "", nil)
			case "ospf3":
				m.RIB.Set(inst, rib.OSPF3, "", nil)
			case "bgp":
				m.clearBGP(inst)
			}
		}
	}
	for key, r := range want {
		if p := m.protos[key]; p != nil {
			p.Reconfigure(cfg, r)
			continue
		}
		inst := instanceOf(key)
		var p Protocol
		switch key[len(inst)+1:] {
		case "ospf":
			p = m.Factory.OSPF(m, inst, false)
		case "ospf3":
			p = m.Factory.OSPF(m, inst, true)
		case "bgp":
			p = m.Factory.BGP(m, inst)
		}
		if p != nil {
			m.protos[key] = p
			p.Reconfigure(cfg, r)
		}
	}
}

// clearBGP withdraws every BGP route of the instance (all neighbours).
func (m *Manager) clearBGP(inst string) {
	srcs := map[string]bool{}
	for _, e := range m.RIB.Lookup(rib.Query{}) {
		if e.Table.Instance != inst {
			continue
		}
		for _, r := range e.Routes {
			if r.Protocol == rib.BGP {
				srcs[r.Source] = true
			}
		}
	}
	for s := range srcs {
		m.RIB.Set(inst, rib.BGP, s, nil)
	}
}

// SetRoutes is how protocols report their routes (all routes of the
// protocol and source in the instance); the FIB is recomputed.
func (m *Manager) SetRoutes(instance string, proto rib.Protocol, source string, routes []rib.Route) {
	m.RIB.Set(instance, proto, source, routes)
	m.Recompute()
}

// Recompute takes the RIB's changes into the FIB view and tells the data
// plane when protocol routes changed.
func (m *Manager) Recompute() {
	m.mu.Lock()
	changed := false
	for _, c := range m.RIB.Changes() {
		t := m.fib[c.Table]
		if t == nil {
			t = map[netip.Prefix]FIBRoute{}
			m.fib[c.Table] = t
		}
		old, had := t[c.Prefix]
		if c.Route == nil || !kernelOwned(c.Route.Protocol) {
			// Withdrawn, or the active route is connected or static (the
			// data plane installs those itself).
			if had {
				delete(t, c.Prefix)
				changed = true
			}
			continue
		}
		fr := FIBRoute{Instance: c.Table.Instance, Prefix: c.Prefix, NextHops: slices.Clone(c.Route.NextHops), Discard: c.Route.Discard,
			KernelProto: kernelProto(c.Route.Protocol)}
		if had && sameFIB(old, fr) {
			continue
		}
		t[c.Prefix] = fr
		changed = true
	}
	cb := m.Changed
	m.mu.Unlock()
	if changed && cb != nil {
		cb()
	}
}

func kernelOwned(p rib.Protocol) bool { return p == rib.OSPF || p == rib.OSPF3 || p == rib.BGP }

func kernelProto(p rib.Protocol) int {
	if p == rib.BGP {
		return KernelBGP
	}
	return KernelOSPF
}

func sameFIB(a, b FIBRoute) bool {
	return a.Discard == b.Discard && a.KernelProto == b.KernelProto && slices.Equal(a.NextHops, b.NextHops)
}

// FIB returns the active protocol routes, sorted.
func (m *Manager) FIB() []FIBRoute {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []FIBRoute
	for _, t := range m.fib {
		for _, r := range t {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b FIBRoute) int {
		if a.Instance != b.Instance {
			if a.Instance < b.Instance {
				return -1
			}
			return 1
		}
		if c := a.Prefix.Addr().Compare(b.Prefix.Addr()); c != 0 {
			return c
		}
		return a.Prefix.Bits() - b.Prefix.Bits()
	})
	return out
}

// Stop ends every protocol.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, p := range m.protos {
		p.Stop()
		delete(m.protos, k)
	}
}

// connected returns the Direct (subnets) or Local (own addresses) routes
// of the instance's routed units that have addresses without a member
// (the stack-wide view; per-member addresses belong to one member).
func connected(cfg *model.Config, inst string, local bool) []rib.Route {
	var out []rib.Route
	for _, name := range sortedUnits(cfg) {
		u := cfg.L3[name]
		if u.Instance != inst || u.Disabled {
			continue
		}
		for _, p := range u.Addrs {
			if local {
				bits := 32
				if p.Addr().Is6() {
					bits = 128
				}
				out = append(out, rib.Route{Prefix: netip.PrefixFrom(p.Addr(), bits), Preference: rib.PrefLocal,
					NextHops: []rib.NextHop{{Interface: name}}})
				continue
			}
			out = append(out, rib.Route{Prefix: p.Masked(), Preference: rib.PrefDirect, NextHops: []rib.NextHop{{Interface: name}}})
		}
	}
	return out
}

func sortedUnits(cfg *model.Config) []string {
	out := make([]string, 0, len(cfg.L3))
	for n := range cfg.L3 {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// static returns the static routes of the instance.
func static(cfg *model.Config, inst string) []rib.Route {
	routes := cfg.Routes
	if inst != "" {
		in := cfg.Instances[inst]
		if in == nil {
			return nil
		}
		routes = in.Routes
	}
	var out []rib.Route
	for _, r := range routes {
		rt := rib.Route{Prefix: r.Prefix, Preference: r.Preference, Discard: r.Discard}
		for _, h := range r.NextHops {
			rt.NextHops = append(rt.NextHops, rib.NextHop{Gateway: h, Interface: unitFor(cfg, inst, h)})
		}
		out = append(out, rt)
	}
	return out
}

// unitFor returns the unit of the instance whose subnet contains a ("":
// none).
func unitFor(cfg *model.Config, inst string, a netip.Addr) string {
	for _, name := range sortedUnits(cfg) {
		u := cfg.L3[name]
		if u.Instance != inst {
			continue
		}
		for _, p := range u.Addrs {
			if p.Masked().Contains(a) {
				return name
			}
		}
	}
	return ""
}
