package ospfd

import (
	"context"
	"encoding/json"
	"errors"
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

// fakeStack delivers stack calls between daemons (JSON as on the wire).
type fakeStack struct{ ds map[int]*Daemon }

func (s *fakeStack) call(_ context.Context, member int, method string, req, resp any) error {
	d := s.ds[member]
	if d == nil {
		return errors.New("not reachable")
	}
	raw, _ := json.Marshal(req)
	switch method {
	case StackRx:
		var p RelayPacket
		json.Unmarshal(raw, &p)
		d.ReceiveRelayed(p)
	case StackTx:
		var p RelayPacket
		json.Unmarshal(raw, &p)
		d.SendRelayed(p)
	case StackLink:
		var l RelayLink
		json.Unmarshal(raw, &l)
		d.LinkReported(l)
	case StackBFDSet:
		var r RelayBFD
		json.Unmarshal(raw, &r)
		out, _ := json.Marshal(d.SetRelayedBFD(1, r))
		if resp != nil {
			json.Unmarshal(out, resp)
		}
	case StackBFDState:
		var st RelayBFDState
		json.Unmarshal(raw, &st)
		d.RelayedBFDState(st)
	}
	return nil
}

// A routed port of member 2: the master (member 1, no such device) runs
// the adjacency with the router behind it through member 2's socket.
func TestRelayedInterface(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, r1, rr := relaySetup(ctx, nil, nil, nil)
	deadline := time.Now().Add(15 * time.Second)
	for {
		s4, ok4 := r1.get(rib.OSPF)
		s6, ok6 := r1.get(rib.OSPF3)
		x4, okx := rr.get(rib.OSPF)
		if ok4 && ok6 && okx && len(s4.Routes) == 1 && len(s6.Routes) == 1 && len(x4.Routes) == 1 {
			if h := s4.Routes[0].NextHops[0]; h.Interface != "2/0/1.0" || h.Gateway != netip.MustParseAddr("10.0.0.2") {
				t.Fatalf("master's OSPF next hop %+v", h)
			}
			if h := s6.Routes[0].NextHops[0]; h.Interface != "2/0/1.0" || h.Gateway != netip.MustParseAddr("fe80::2") {
				t.Fatalf("master's OSPFv3 next hop %+v", h)
			}
			if x4.Routes[0].Prefix != netip.MustParsePrefix("192.0.2.1/32") {
				t.Fatalf("the router's route %+v", x4.Routes[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no adjacency through the relay: master %+v / %+v, router %+v", s4, s6, x4)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// relaySetup runs a stack of two members, the master 1 and member 2 with
// the routed port 2/0/1.0 (BFD bfd on it when not nil; b1/b2 are the
// members' cer-bfdd), and a router behind that port.
func relaySetup(ctx context.Context, bfd *BFDSpec, b1, b2 BFD) (d1, d2 *Daemon, r1, rr *fakeRIB) {
	w := &wire{ports: map[string][]*fakePort{}}
	st := &fakeStack{ds: map[int]*Daemon{}}
	port := iface("2/0/1.0", "", "10.0.0.1/30", "2001:db8::1/64")
	port.Owners, port.BFD = []int{2}, bfd
	lo := iface("lo0.0", "lo1", "192.0.2.1/32", "2001:db8:ff::1/128")
	lo.Passive, lo.Owners = true, []int{1}
	cfg := Config{Instances: []Instance{
		{Version: ospf.V2, RouterID: 0x01010101, ReferenceBW: 100e9, Interfaces: []Iface{port, lo}},
		{Version: ospf.V3, RouterID: 0x01010101, ReferenceBW: 100e9, Interfaces: []Iface{port, lo}},
	}}
	// Member 1, the master.
	r1 = &fakeRIB{sets: map[rib.Protocol]ribd.SetRoutes{}}
	d1 = New(fakeKernel{"lo1": {Index: 1, MTU: 65536, Up: true, LinkLocal: netip.MustParseAddr("fe80::99")}}, fakeNet{w: w}, r1, quiet)
	d1.Member, d1.StackCall, d1.Settle, d1.BFD = 1, st.call, time.Second, b1
	// Member 2 has the port's device ("link", joined to the router).
	cfg2 := cfg
	cfg2.Instances = []Instance{cfg.Instances[0], cfg.Instances[1]}
	for k := range cfg2.Instances {
		ifs := append([]Iface(nil), cfg2.Instances[k].Interfaces...)
		ifs[0].Device = "link"
		cfg2.Instances[k].Interfaces = ifs
	}
	d2 = New(fakeKernel{"link": {Index: 7, MTU: 1500, Up: true, LinkLocal: netip.MustParseAddr("fe80::1")}}, fakeNet{w: w}, &fakeRIB{sets: map[rib.Protocol]ribd.SetRoutes{}}, quiet)
	d2.Member, d2.StackCall, d2.BFD = 2, st.call, b2
	st.ds[1], st.ds[2] = d1, d2
	for _, d := range []*Daemon{d1, d2} {
		go d.Run(ctx)
	}
	d1.SetConfig(cfg)
	d2.SetConfig(cfg2)
	d1.SetRole(true, 1)
	d2.SetRole(false, 1)
	// The router behind member 2's port.
	rr = &fakeRIB{sets: map[rib.Protocol]ribd.SetRoutes{}}
	dr := New(fakeKernel{"link": {Index: 3, MTU: 1500, Up: true, LinkLocal: netip.MustParseAddr("fe80::2")},
		"lo": {Index: 1, MTU: 65536, Up: true, LinkLocal: netip.MustParseAddr("fe80::98")}}, fakeNet{w: w}, rr, quiet)
	dr.Settle = time.Second
	rp := iface("e0", "link", "10.0.0.2/30", "2001:db8::2/64")
	rlo := iface("lo", "lo", "192.0.2.2/32", "2001:db8:ff::2/128")
	rlo.Passive = true
	dr.SetConfig(Config{Instances: []Instance{
		{Version: ospf.V2, RouterID: 0x02020202, ReferenceBW: 100e9, Interfaces: []Iface{rp, rlo}},
		{Version: ospf.V3, RouterID: 0x02020202, ReferenceBW: 100e9, Interfaces: []Iface{rp, rlo}},
	}})
	dr.SetMaster(true)
	go dr.Run(ctx)
	return d1, d2, r1, rr
}

func TestRelayedDecision(t *testing.T) {
	cases := []struct {
		name          string
		ic            Iface
		me, master    int
		relayed, here bool
	}{
		{"irb: never (its frames come in their VLAN)", Iface{IRB: true, Owners: []int{1, 2}, Device: "irb.10"}, 2, 1, false, false},
		{"the master's own routed port", Iface{Owners: []int{1}, Device: "eth1"}, 1, 1, false, false},
		{"member 2's routed port, on member 2", Iface{Owners: []int{2}, Device: "eth5"}, 2, 1, true, true},
		{"member 2's routed port, on member 3", Iface{Owners: []int{2}}, 3, 1, true, false},
		{"MC-LAG bundle with a leg on the master: the other leg relays", Iface{Owners: []int{1, 2}, Device: "ae1"}, 2, 1, true, true},
		{"MC-LAG bundle, on the master", Iface{Owners: []int{1, 2}, Device: "ae1"}, 1, 1, true, false},
	}
	for _, c := range cases {
		r, h := relayed(c.ic, c.me, c.master)
		if r != c.relayed || h != c.here {
			t.Errorf("%s: relayed %v here %v", c.name, r, h)
		}
	}
}
