package routing

import (
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

func cfg(t *testing.T, s string) *model.Config {
	t.Helper()
	tr, err := config.ParseSet(s)
	if err != nil {
		t.Fatal(err)
	}
	c, is := model.Build(tr, nil)
	if is.HasErrors() {
		t.Fatalf("errors:\n%s", is)
	}
	return c
}

const base = `
set interfaces 1/0/5 unit 0 family inet address 10.1.1.1/30
set interfaces 1/0/7 unit 0 family inet address 10.9.9.1/30
set routing-instances red interface 1/0/7.0
set routing-options static route 0.0.0.0/0 next-hop 10.1.1.2
set routing-options static route 192.0.2.0/24 next-hop 10.1.1.2
set routing-options static route 192.0.2.0/24 preference 200
set routing-instances red routing-options static route 0.0.0.0/0 discard
`

type fakeProto struct {
	m        *Manager
	inst     string
	stopped  bool
	reconfig int
}

func (f *fakeProto) Reconfigure(*model.Config, *model.Routing) { f.reconfig++ }
func (f *fakeProto) Stop()                                     { f.stopped = true }

func TestConnectedAndStatic(t *testing.T) {
	m := New(nil)
	m.Apply(cfg(t, base))
	e := m.RIB.Lookup(rib.Query{Tables: []rib.Table{{}}})
	got := map[string]rib.Protocol{}
	for _, d := range e {
		got[d.Prefix.String()] = d.Routes[d.Active].Protocol
	}
	want := map[string]rib.Protocol{"10.1.1.0/30": rib.Direct, "10.1.1.1/32": rib.Local, "0.0.0.0/0": rib.Static, "192.0.2.0/24": rib.Static}
	for p, pr := range want {
		if got[p] != pr {
			t.Errorf("%s: %v (all: %v)", p, got[p], got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("extra routes: %v", got)
	}
	red := m.RIB.Lookup(rib.Query{Tables: []rib.Table{{Instance: "red"}}})
	if len(red) != 3 {
		t.Fatalf("red: %+v", red)
	}
	// Static routes are not handed to the kernel by routing (the data
	// plane installs them).
	if f := m.FIB(); len(f) != 0 {
		t.Fatalf("fib %+v", f)
	}
	// The static next hop names its unit.
	d := m.RIB.Lookup(rib.Query{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Match: "exact"})
	if d[0].Routes[0].NextHops[0].Interface != "1/0/5.0" {
		t.Fatalf("next hop unit %+v", d[0].Routes[0].NextHops)
	}
}

func TestProtocolRoutesAndPreference(t *testing.T) {
	m := New(nil)
	var changes atomic.Int32
	m.Changed = func() { changes.Add(1) }
	m.Apply(cfg(t, base))
	changes.Store(0)
	p192 := netip.MustParsePrefix("192.0.2.0/24")
	pdef := netip.MustParsePrefix("0.0.0.0/0")
	ospf := []rib.Route{
		{Prefix: p192, Preference: rib.PrefOSPF, NextHops: []rib.NextHop{{Gateway: netip.MustParseAddr("10.1.1.2"), Interface: "1/0/5.0"}}},
		{Prefix: pdef, Preference: rib.PrefOSPFExternal, NextHops: []rib.NextHop{{Gateway: netip.MustParseAddr("10.1.1.2"), Interface: "1/0/5.0"}}},
	}
	m.SetRoutes("", rib.OSPF, "", ospf)
	f := m.FIB()
	// The floating static (preference 200) loses to OSPF; the default
	// route stays static (5 < 150).
	if len(f) != 1 || f[0].Prefix != p192 || f[0].KernelProto != KernelOSPF || changes.Load() != 1 {
		t.Fatalf("fib %+v, changes %d", f, changes.Load())
	}
	// The same routes again: no change, no reconcile.
	m.SetRoutes("", rib.OSPF, "", ospf)
	if changes.Load() != 1 {
		t.Fatal("unchanged routes triggered a reconcile")
	}
	// OSPF withdraws: back to the static route, which the data plane has.
	m.SetRoutes("", rib.OSPF, "", nil)
	if f := m.FIB(); len(f) != 0 || changes.Load() != 2 {
		t.Fatalf("fib after withdraw %+v", f)
	}
}

func TestProtocolLifecycle(t *testing.T) {
	m := New(nil)
	var made []*fakeProto
	m.Factory = Factory{
		OSPF: func(m *Manager, inst string, v3 bool) Protocol {
			p := &fakeProto{m: m, inst: inst}
			made = append(made, p)
			return p
		},
		BGP: func(m *Manager, inst string) Protocol {
			p := &fakeProto{m: m, inst: inst}
			made = append(made, p)
			return p
		},
	}
	withOSPF := base + `
set protocols ospf area 0 interface 1/0/5.0
set routing-instances red protocols ospf area 0 interface 1/0/7.0
`
	m.Apply(cfg(t, withOSPF))
	if len(made) != 2 || made[0].reconfig != 1 {
		t.Fatalf("started %d", len(made))
	}
	m.SetRoutes("red", rib.OSPF, "", []rib.Route{{Prefix: netip.MustParsePrefix("172.16.0.0/16"), Preference: rib.PrefOSPF,
		NextHops: []rib.NextHop{{Gateway: netip.MustParseAddr("10.9.9.2"), Interface: "1/0/7.0"}}}})
	if len(m.FIB()) != 1 {
		t.Fatal("red route not in the FIB")
	}
	// A commit with the same protocols reconfigures, never restarts.
	m.Apply(cfg(t, withOSPF+"set protocols ospf area 0 interface 1/0/5.0 metric 7\n"))
	if len(made) != 2 || made[0].reconfig != 2 || made[1].reconfig != 2 {
		t.Fatalf("restarted instead of reconfigured: %d made", len(made))
	}
	// OSPF removed from red: stopped, routes withdrawn.
	m.Apply(cfg(t, base+"set protocols ospf area 0 interface 1/0/5.0\n"))
	stopped := 0
	for _, p := range made {
		if p.stopped {
			stopped++
			if p.inst != "red" {
				t.Fatalf("wrong instance stopped: %q", p.inst)
			}
		}
	}
	if stopped != 1 || len(m.FIB()) != 0 {
		t.Fatalf("stopped %d, fib %+v", stopped, m.FIB())
	}
}
