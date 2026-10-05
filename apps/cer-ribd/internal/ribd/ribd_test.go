package ribd

import (
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/lib/rib"
	"github.com/thxrben/cerium-switchd/lib/sys/netdev"
)

type fakeKernel struct {
	want   []netdev.Route
	protos []int
	calls  int
}

func (f *fakeKernel) install(want []netdev.Route, protos []int) (bool, []string, error) {
	f.want, f.protos = want, protos
	f.calls++
	return true, nil, nil
}

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func ip(s string) netip.Addr    { return netip.MustParseAddr(s) }

func route(f *fakeKernel, p string) (netdev.Route, bool) {
	i := slices.IndexFunc(f.want, func(r netdev.Route) bool { return r.Prefix == pfx(p) })
	if i < 0 {
		return netdev.Route{}, false
	}
	return f.want[i], true
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestConfigRoutes(t *testing.T) {
	f := &fakeKernel{}
	now := time.Unix(1000, 0)
	s := New(f.install, quiet, now)
	// Nothing before the configuration: the kernel keeps its routes.
	if s.Sync(now); f.calls != 0 {
		t.Fatal("installed without a configuration")
	}
	s.SetConfig(&Config{Instances: map[string]Instance{
		"": {Devices: map[string]string{"irb.10": "irb.10", "1/0/5.0": "eth5"}, Routes: []rib.Route{
			{Prefix: pfx("10.1.0.0/24"), Protocol: rib.Direct, NextHops: []rib.NextHop{{Interface: "irb.10"}}},
			{Prefix: pfx("10.1.0.1/32"), Protocol: rib.Local, NextHops: []rib.NextHop{{Interface: "irb.10"}}},
			{Prefix: pfx("10.9.0.0/16"), Protocol: rib.Static, Preference: rib.PrefStatic,
				NextHops: []rib.NextHop{{Gateway: ip("10.1.0.254"), Interface: "irb.10"}, {Gateway: ip("10.5.0.1"), Interface: "1/0/5.0"}}},
			// Static wins over the lease's default route.
			{Prefix: DefaultRoute, Protocol: rib.Static, Preference: rib.PrefStatic, NextHops: []rib.NextHop{{Gateway: ip("10.1.0.253")}}},
			{Prefix: DefaultRoute, Protocol: rib.DHCP, Preference: rib.PrefDHCP, NextHops: []rib.NextHop{{Gateway: ip("10.1.0.1"), Interface: "irb.10"}}},
			{Prefix: pfx("192.0.2.0/24"), Protocol: rib.Static, Preference: rib.PrefStatic, Discard: true},
			// Through a unit of another member: not installed here.
			{Prefix: pfx("10.7.0.0/16"), Protocol: rib.Static, Preference: rib.PrefStatic, NextHops: []rib.NextHop{{Gateway: ip("10.7.0.1"), Interface: "2/0/1.0"}}},
		}},
		"red": {VRF: "red", Devices: map[string]string{"irb.20": "irb.20"}, Routes: []rib.Route{
			{Prefix: DefaultRoute, Protocol: rib.DHCP, Preference: rib.PrefDHCP, NextHops: []rib.NextHop{{Gateway: ip("10.2.0.1"), Interface: "irb.20"}}},
		}},
	}})
	s.Sync(now)
	if !slices.Equal(f.protos, []int{netdev.ProtoStatic}) {
		t.Fatalf("managed %v: routing protocols must wait for their daemons", f.protos)
	}
	if len(f.want) != 4 {
		t.Fatalf("want %+v", f.want)
	}
	if r, _ := route(f, "10.9.0.0/16"); !slices.Equal(r.Devs, []string{"irb.10", "eth5"}) || len(r.NextHops) != 2 {
		t.Errorf("ECMP static %+v", r)
	}
	if r, _ := route(f, "0.0.0.0/0"); r.VRF != "" || r.NextHops[0] != ip("10.1.0.253") {
		t.Errorf("default %+v (the static route wins)", r)
	}
	if r, _ := route(f, "192.0.2.0/24"); !r.Discard {
		t.Errorf("discard %+v", r)
	}
	if _, ok := route(f, "10.1.0.0/24"); ok {
		t.Error("a direct route installed")
	}
	i := slices.IndexFunc(f.want, func(r netdev.Route) bool { return r.VRF == "red" })
	if i < 0 || f.want[i].NextHops[0] != ip("10.2.0.1") || f.want[i].Devs[0] != "irb.20" {
		t.Errorf("red's lease default route: %+v", f.want)
	}
	// The instance goes away: its routes too.
	s.SetConfig(&Config{Instances: map[string]Instance{"": {}}})
	s.Sync(now)
	if len(f.want) != 0 {
		t.Errorf("after removing everything: %+v", f.want)
	}
}

// Routes of a protocol are managed once it sent all of them (or after the
// grace time): a restart of cer-ribd never removes routes of a protocol
// that has not reported yet.
func TestProtocolReadiness(t *testing.T) {
	f := &fakeKernel{}
	now := time.Unix(1000, 0)
	s := New(f.install, quiet, now)
	s.SetConfig(&Config{Instances: map[string]Instance{"": {Devices: map[string]string{"irb.10": "irb.10"}}}})
	ospf := rib.Route{Prefix: pfx("10.50.0.0/16"), Preference: rib.PrefOSPF, NextHops: []rib.NextHop{{Gateway: ip("10.1.0.2"), Interface: "irb.10"}}}
	s.SetRoutes(SetRoutes{Protocol: rib.OSPF, Routes: []rib.Route{ospf}, Full: true})
	s.Sync(now)
	if slices.Contains(f.protos, netdev.ProtoOSPF) {
		t.Fatal("OSPF managed before OSPFv3 reported")
	}
	if _, ok := route(f, "10.50.0.0/16"); ok {
		t.Fatal("an OSPF route installed while OSPF's kernel routes are not managed")
	}
	s.SetRoutes(SetRoutes{Protocol: rib.OSPF3, Full: true})
	s.Sync(now)
	if !slices.Contains(f.protos, netdev.ProtoOSPF) {
		t.Fatalf("managed %v", f.protos)
	}
	if r, ok := route(f, "10.50.0.0/16"); !ok || r.Proto != netdev.ProtoOSPF {
		t.Fatalf("OSPF route %+v", r)
	}
	// BGP never reported: managed after the grace time.
	if slices.Contains(f.protos, netdev.ProtoBGP) {
		t.Fatal("BGP managed at once")
	}
	s.Sync(now.Add(Grace))
	if !slices.Contains(f.protos, netdev.ProtoBGP) {
		t.Fatal("BGP not managed after the grace time")
	}
}

func TestPreferenceAcrossSources(t *testing.T) {
	f := &fakeKernel{}
	now := time.Unix(1000, 0)
	s := New(f.install, quiet, now)
	s.SetConfig(&Config{Instances: map[string]Instance{"": {Devices: map[string]string{"irb.10": "irb.10"}, Routes: []rib.Route{
		{Prefix: DefaultRoute, Protocol: rib.DHCP, Preference: rib.PrefDHCP, NextHops: []rib.NextHop{{Gateway: ip("10.1.0.1"), Interface: "irb.10"}}},
	}}}})
	s.SetRoutes(SetRoutes{Protocol: rib.BGP, Source: "10.1.0.9", Full: true, Routes: []rib.Route{
		{Prefix: DefaultRoute, Preference: rib.PrefBGP, NextHops: []rib.NextHop{{Gateway: ip("10.1.0.9"), Interface: "irb.10"}}}}})
	s.Sync(now)
	if r, _ := route(f, "0.0.0.0/0"); r.Proto != netdev.ProtoBGP || r.NextHops[0] != ip("10.1.0.9") {
		t.Fatalf("BGP's default route must win over the lease's: %+v", r)
	}
	s.SetRoutes(SetRoutes{Protocol: rib.BGP, Source: "10.1.0.9", Full: true})
	s.Sync(now)
	if r, _ := route(f, "0.0.0.0/0"); r.Proto != netdev.ProtoStatic || r.NextHops[0] != ip("10.1.0.1") {
		t.Fatalf("the lease's default route again: %+v", r)
	}
}

// BGP next hops are resolved through the routes of other protocols: a
// connected one directly, one behind OSPF through OSPF's next hops; one
// reachable only through BGP is not installed.
func TestBGPNextHopResolution(t *testing.T) {
	f := &fakeKernel{}
	now := time.Unix(1000, 0)
	s := New(f.install, quiet, now)
	s.SetConfig(&Config{Instances: map[string]Instance{
		"": {Devices: map[string]string{"irb.10": "irb.10", "1/0/5.0": "eth5"}, Routes: []rib.Route{
			{Prefix: pfx("10.1.0.0/24"), Protocol: rib.Direct, NextHops: []rib.NextHop{{Interface: "irb.10"}}},
		}},
	}})
	s.SetRoutes(SetRoutes{Protocol: rib.OSPF, Full: true, Routes: []rib.Route{
		{Prefix: pfx("192.0.2.9/32"), Protocol: rib.OSPF, Preference: rib.PrefOSPF,
			NextHops: []rib.NextHop{{Gateway: ip("10.5.0.2"), Interface: "1/0/5.0"}, {Gateway: ip("10.1.0.3"), Interface: "irb.10"}}}}})
	s.SetRoutes(SetRoutes{Protocol: rib.OSPF3, Full: true})
	s.SetRoutes(SetRoutes{Protocol: rib.BGP, Source: "10.1.0.2", Full: true, Routes: []rib.Route{
		{Prefix: pfx("203.0.113.0/24"), Protocol: rib.BGP, Preference: rib.PrefBGP, NextHops: []rib.NextHop{{Gateway: ip("10.1.0.2")}}},
		{Prefix: pfx("198.51.100.0/24"), Protocol: rib.BGP, Preference: rib.PrefBGP, NextHops: []rib.NextHop{{Gateway: ip("192.0.2.9")}}},
		{Prefix: pfx("192.0.2.200/32"), Protocol: rib.BGP, Preference: rib.PrefBGP, NextHops: []rib.NextHop{{Gateway: ip("10.1.0.2")}}},
		{Prefix: pfx("100.64.0.0/16"), Protocol: rib.BGP, Preference: rib.PrefBGP, NextHops: []rib.NextHop{{Gateway: ip("192.0.2.200")}}},
	}})
	s.Sync(now)
	if r, ok := route(f, "203.0.113.0/24"); !ok || r.NextHops[0] != ip("10.1.0.2") || r.Devs[0] != "irb.10" {
		t.Errorf("connected next hop: %+v", r)
	}
	if r, ok := route(f, "198.51.100.0/24"); !ok || len(r.NextHops) != 2 || !slices.Contains(r.Devs, "eth5") {
		t.Errorf("next hop behind OSPF: %+v", r)
	}
	if _, ok := route(f, "100.64.0.0/16"); ok {
		t.Error("a next hop reachable only through BGP must not be installed")
	}
}

// A BGP route whose next hop cannot be resolved is hidden: the static
// route for the same prefix is used; once the next hop is reachable, BGP
// (if better) wins again.
func TestBGPUnresolvableHidden(t *testing.T) {
	f := &fakeKernel{}
	now := time.Unix(1000, 0)
	s := New(f.install, quiet, now)
	s.SetConfig(&Config{Instances: map[string]Instance{
		"": {Devices: map[string]string{"irb.10": "irb.10"}, Routes: []rib.Route{
			{Prefix: pfx("10.1.0.0/24"), Protocol: rib.Direct, NextHops: []rib.NextHop{{Interface: "irb.10"}}},
			{Prefix: pfx("203.0.113.0/24"), Protocol: rib.Static, Preference: 200, NextHops: []rib.NextHop{{Gateway: ip("10.1.0.9"), Interface: "irb.10"}}},
		}},
	}})
	s.SetRoutes(SetRoutes{Protocol: rib.BGP, Source: "10.9.9.9", Full: true, Routes: []rib.Route{
		{Prefix: pfx("203.0.113.0/24"), Protocol: rib.BGP, Preference: rib.PrefBGP, NextHops: []rib.NextHop{{Gateway: ip("192.0.2.1")}}}}})
	es := s.Lookup(rib.Query{Prefix: pfx("203.0.113.0/24"), Match: "exact"})
	if len(es) != 1 || es[0].Active != 0 || es[0].Routes[0].Protocol != rib.Static {
		t.Fatalf("with an unresolvable BGP next hop: %+v", es)
	}
	if h := s.Lookup(rib.Query{Hidden: true}); len(h) != 1 || h[0].Routes[0].Protocol != rib.BGP {
		t.Fatalf("hidden routes %+v", h)
	}
	if sum := s.RIB.Summaries(); sum[0].Hidden != 1 {
		t.Fatalf("summary %+v", sum)
	}
	// OSPF learns the next hop: the BGP route is usable and wins (170 < 200).
	s.SetRoutes(SetRoutes{Protocol: rib.OSPF, Full: true, Routes: []rib.Route{
		{Prefix: pfx("192.0.2.0/24"), Protocol: rib.OSPF, Preference: rib.PrefOSPF, NextHops: []rib.NextHop{{Gateway: ip("10.1.0.2"), Interface: "irb.10"}}}}})
	es = s.Lookup(rib.Query{Prefix: pfx("203.0.113.0/24"), Match: "exact"})
	if len(es) != 1 || es[0].Routes[es[0].Active].Protocol != rib.BGP {
		t.Fatalf("after the next hop became reachable: %+v", es)
	}
}
