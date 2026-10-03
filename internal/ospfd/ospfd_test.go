package ospfd

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/policy"
	"github.com/thxrben/cerium-switchd/internal/ribd"
	"github.com/thxrben/cerium-switchd/pkg/ospf"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeKernel map[string]LinkInfo

func (k fakeKernel) Link(dev string) (LinkInfo, bool) { li, ok := k[dev]; return li, ok }

// wire joins the ports of the daemons by device name: a packet sent on
// one end arrives at the others.
type wire struct {
	mu    sync.Mutex
	ports map[string][]*fakePort // device -> ends
}

type fakePort struct {
	w   *wire
	dev string
	rx  func(src, dst netip.Addr, pkt []byte)
	src netip.Addr
}

type fakeNet struct {
	w    *wire
	name string
}

func (n fakeNet) Open(v ospf.Version, dev string, ifindex int, rx func(src, dst netip.Addr, pkt []byte)) (Port, error) {
	p := &fakePort{w: n.w, dev: dev, rx: rx}
	n.w.mu.Lock()
	n.w.ports[dev] = append(n.w.ports[dev], p)
	n.w.mu.Unlock()
	return p, nil
}

func (p *fakePort) Send(src, dst netip.Addr, pkt []byte) error {
	p.w.mu.Lock()
	ends := append([]*fakePort(nil), p.w.ports[p.dev]...)
	p.w.mu.Unlock()
	for _, o := range ends {
		if o != p {
			o.rx(src, dst, append([]byte(nil), pkt...))
		}
	}
	return nil
}

func (p *fakePort) Close() error {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	ends := p.w.ports[p.dev]
	for i, o := range ends {
		if o == p {
			p.w.ports[p.dev] = append(ends[:i], ends[i+1:]...)
			break
		}
	}
	return nil
}

type fakeRIB struct {
	mu     sync.Mutex
	sets   map[rib.Protocol]ribd.SetRoutes
	active []rib.Entry
}

func (r *fakeRIB) SetRoutes(_ context.Context, sr ribd.SetRoutes) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sets[sr.Protocol] = sr
	return nil
}

func (r *fakeRIB) Active(context.Context, string) ([]rib.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active, nil
}

func (r *fakeRIB) get(p rib.Protocol) (ribd.SetRoutes, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sr, ok := r.sets[p]
	return sr, ok
}

func iface(unit, dev string, prefixes ...string) Iface {
	ic := Iface{Unit: unit, Device: dev, Priority: 1, Hello: 1, Dead: 4, Retransmit: 1, TransitDelay: 1, P2P: true}
	for _, p := range prefixes {
		ic.Prefixes = append(ic.Prefixes, netip.MustParsePrefix(p))
	}
	return ic
}

// Two switches over one link, OSPF and OSPFv3 at once: each learns the
// other's loopback, the routes reach cer-ribd with the unit as next hop
// interface, complete once settled; a non-master runs nothing.
func TestTwoDaemons(t *testing.T) {
	w := &wire{ports: map[string][]*fakePort{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mk := func(name string, rid ospf.ID, ll string, linkPfx4, linkPfx6, lo4, lo6 string) (*Daemon, *fakeRIB) {
		k := fakeKernel{"eth-" + name: {Index: 7, MTU: 1500, Up: true, LinkLocal: netip.MustParseAddr(ll), SpeedMbps: 10000},
			"lo-" + name: {Index: 8, MTU: 65536, Up: true, LinkLocal: netip.MustParseAddr("fe80::99"), SpeedMbps: 0}}
		r := &fakeRIB{sets: map[rib.Protocol]ribd.SetRoutes{}}
		d := New(k, fakeNet{w: w, name: name}, r, quiet)
		d.Settle = 2 * time.Second
		link := iface("1/0/1.0", "eth-"+name, linkPfx4, linkPfx6)
		link.Device = "link" // both ends on one wire
		k["link"] = k["eth-"+name]
		lo := iface("lo0.0", "lo-"+name, lo4, lo6)
		lo.Passive = true
		cfg := Config{Instances: []Instance{
			{Version: ospf.V2, RouterID: rid, ReferenceBW: 100e9, Interfaces: []Iface{link, lo}},
			{Version: ospf.V3, RouterID: rid, ReferenceBW: 100e9, Interfaces: []Iface{link, lo}},
		}}
		d.SetConfig(cfg)
		d.SetMaster(true)
		go d.Run(ctx)
		return d, r
	}
	_, r1 := mk("a", 0x01010101, "fe80::a", "10.0.0.1/30", "2001:db8::1/64", "192.0.2.1/32", "2001:db8:ff::1/128")
	d2, r2 := mk("b", 0x02020202, "fe80::b", "10.0.0.2/30", "2001:db8::2/64", "192.0.2.2/32", "2001:db8:ff::2/128")
	deadline := time.Now().Add(10 * time.Second)
	for {
		s4, ok4 := r1.get(rib.OSPF)
		s6, ok6 := r1.get(rib.OSPF3)
		if ok4 && ok6 && s4.Full && s6.Full && len(s4.Routes) == 1 && len(s6.Routes) == 1 {
			if rt := s4.Routes[0]; rt.Prefix != netip.MustParsePrefix("192.0.2.2/32") || rt.NextHops[0].Gateway != netip.MustParseAddr("10.0.0.2") ||
				rt.NextHops[0].Interface != "1/0/1.0" || rt.Metric != 11 {
				t.Fatalf("OSPF route %+v", rt)
			}
			if rt := s6.Routes[0]; rt.Prefix != netip.MustParsePrefix("2001:db8:ff::2/128") || rt.NextHops[0].Gateway != netip.MustParseAddr("fe80::b") {
				t.Fatalf("OSPFv3 route %+v", rt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("routes: v2 %+v\nv3 %+v", s4, s6)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Cost from the reference bandwidth: 100g / 10g = 10.
	st, err := d2.Status(StatusRequest{Version: ospf.V2})
	if err != nil || len(st) != 1 || st[0].Interfaces[0].Cost != 10 || st[0].Neighbors[0].State != "Full" {
		t.Fatalf("status %+v %v", st, err)
	}
	if _, ok := r2.get(rib.OSPF); !ok {
		t.Fatal("r2 got no routes")
	}
	// Not the master: the instances stop.
	d2.SetMaster(false)
	time.Sleep(200 * time.Millisecond)
	if st, _ := d2.Status(StatusRequest{}); len(st) != 0 {
		t.Fatalf("a non-master runs %d instances", len(st))
	}
}

// Export: the accepted active routes of other protocols of the family
// become externals; OSPF's own routes and inactive ones never.
func TestExports(t *testing.T) {
	m, tag := uint32(20), uint32(7)
	pol := policy.New(&model.Policies{Statements: map[string]*model.PolicyStatement{
		"statics": {Name: "statics", Terms: []*model.PolicyTerm{{Name: "s", From: model.PolicyFrom{Protocols: []string{"static"}},
			Then: model.PolicyThen{Flow: "accept", ExternalType: 1, Metric: &m, Tag: &tag}}}},
	}})
	entries := []rib.Entry{
		{Prefix: netip.MustParsePrefix("10.9.0.0/16"), Active: 0, Routes: []rib.Route{{Protocol: rib.Static}}},
		{Prefix: netip.MustParsePrefix("10.8.0.0/16"), Active: 0, Routes: []rib.Route{{Protocol: rib.OSPF}}},
		{Prefix: netip.MustParsePrefix("10.6.0.0/16"), Active: 0, Routes: []rib.Route{{Protocol: rib.Direct}}},
		{Prefix: netip.MustParsePrefix("2001:db8:9::/48"), Active: 0, Routes: []rib.Route{{Protocol: rib.Static}}},
		{Prefix: netip.MustParsePrefix("10.7.0.0/16"), Active: -1, Routes: []rib.Route{{Protocol: rib.Static}}},
	}
	got := exports(pol, Instance{Version: ospf.V2, Export: []string{"statics"}}, entries)
	if len(got) != 1 || got[0] != (ospf.External{Prefix: netip.MustParsePrefix("10.9.0.0/16"), Metric: 20, Type1: true, Tag: 7}) {
		t.Fatalf("v2 exports %+v", got)
	}
	got = exports(pol, Instance{Version: ospf.V3, Export: []string{"statics"}}, entries)
	if len(got) != 1 || got[0].Prefix != netip.MustParsePrefix("2001:db8:9::/48") {
		t.Fatalf("v3 exports %+v", got)
	}
}
