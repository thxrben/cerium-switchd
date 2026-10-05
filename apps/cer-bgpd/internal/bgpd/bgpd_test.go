package bgpd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/lib/bgp"
	"github.com/thxrben/cerium-switchd/lib/conf/model"
	"github.com/thxrben/cerium-switchd/lib/platform/api/bfdapi"
	"github.com/thxrben/cerium-switchd/lib/platform/api/ribapi"
	"github.com/thxrben/cerium-switchd/lib/rib"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// loNet is Net over loopback addresses: each side listens on its own
// 127.0.0.x with a random port.
type loNet struct {
	local netip.Addr
	ports *sync.Map // netip.Addr -> int
}

type loListener struct{ net.Listener }

func (loListener) SetKeys(map[netip.Addr]string) error { return nil }

func (n loNet) Listen(string, map[netip.Addr]string) (Listener, error) {
	l, err := net.Listen("tcp", netip.AddrPortFrom(n.local, 0).String())
	if err != nil {
		return nil, err
	}
	n.ports.Store(n.local, l.Addr().(*net.TCPAddr).Port)
	return loListener{l}, nil
}

func (n loNet) Dial(ctx context.Context, _ string, nb bgp.Neighbor) (net.Conn, error) {
	p, ok := n.ports.Load(nb.Addr)
	if !ok {
		return nil, errors.New("not listening")
	}
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: n.local.AsSlice()}}
	return d.DialContext(ctx, "tcp", netip.AddrPortFrom(nb.Addr, uint16(p.(int))).String())
}

func (n loNet) Transport() bgp.Transport { return netDialer{n} }

type netDialer struct{ n loNet }

func (t netDialer) Dial(ctx context.Context, nb bgp.Neighbor) (net.Conn, error) {
	return t.n.Dial(ctx, "", nb)
}

// fakeRIB applies the deltas to a routing table as cer-ribd does
// (sequence, sync, sweep when converged) and answers Active.
type fakeRIB struct {
	mu     sync.Mutex
	sets   map[string]ribapi.SetRoutes
	active []rib.Entry
	rib    *rib.RIB
	seq    uint64
	// deltas, syncs and prefixes received (for tests of the protocol).
	deltas, syncs, prefixes int
}

func (r *fakeRIB) SetRoutes(_ context.Context, sr ribapi.SetRoutes) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sets[sr.Source] = sr
	return nil
}

func (r *fakeRIB) Delta(_ context.Context, d ribapi.RoutesDelta) (ribapi.DeltaReply, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rib == nil {
		r.rib = rib.New(nil)
	}
	if d.Sync != "begin" && d.Seq != r.seq+1 {
		return ribapi.DeltaReply{Resync: true}, nil
	}
	r.seq = d.Seq
	r.deltas++
	if d.Sync == "begin" {
		r.syncs++
		r.rib.BeginGen(d.Instance, d.Protocol)
	}
	for _, p := range d.Prefixes {
		r.rib.Replace(d.Instance, d.Protocol, p.Prefix, p.Routes)
	}
	r.prefixes += len(d.Prefixes)
	if d.Sync == "end" && d.Full {
		r.rib.SweepGen(d.Instance, d.Protocol)
	}
	r.rib.Changes()
	return ribapi.DeltaReply{}, nil
}

func (r *fakeRIB) Active(context.Context, string) ([]rib.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active, nil
}

func (r *fakeRIB) routes(src string) []rib.Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rib == nil {
		return nil
	}
	var out []rib.Route
	r.rib.Each(rib.BGP, func(_ rib.Table, rt rib.Route) {
		if rt.Source == src {
			out = append(out, rt)
		}
	})
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
	}
}

func lp(v uint32) *uint32 { return &v }

func pref(v int) *int { return &v }

// The daemon against a router: its routes reach the routing table per
// neighbour (import policy applied: a rejected prefix is missing, local
// preference set), the switch's static route is announced by the export
// policy, the default export policy announces no other protocol's route.
func TestDaemon(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ports := &sync.Map{}
	me, peer := netip.MustParseAddr("127.0.0.61"), netip.MustParseAddr("127.0.0.62")

	// The router: AS 65002, announces two prefixes.
	rn := loNet{local: peer, ports: ports}
	rl, _ := rn.Listen("", nil)
	var rmu sync.Mutex
	var got []bgp.Route
	router := bgp.New(rn.Transport(), bgp.Policy{Export: func(*bgp.Neighbor, *bgp.Path) bool { return true }}, quiet)
	router.OnRoutes = func(rs []bgp.Route, _ bool) { rmu.Lock(); got = rs; rmu.Unlock() }
	router.RoutesDelay = 10 * time.Millisecond
	go router.Run(ctx)
	router.Configure(bgp.Config{AS: 65002, RouterID: peer, ConnectRetry: 200 * time.Millisecond, Neighbors: []bgp.Neighbor{{
		Addr: me, PeerAS: 65001, LocalAS: 65002, HoldTime: 9, Families: []bgp.Family{bgp.IPv4Unicast}}}})
	go func() {
		for {
			c, err := rl.Accept()
			if err != nil {
				return
			}
			router.Accept(c)
		}
	}()
	router.SetLocal([]bgp.Path{
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Source: "static"},
		{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Source: "static"},
	}, nil)

	// The switch: AS 65001, import rejects 198.51.100.0/24 and sets local
	// preference 200, export announces static routes.
	r := &fakeRIB{sets: map[string]ribapi.SetRoutes{}, active: []rib.Entry{
		{Prefix: netip.MustParsePrefix("10.10.0.0/16"), Active: 0, Routes: []rib.Route{{Protocol: rib.Static}}},
		{Prefix: netip.MustParsePrefix("10.20.0.0/16"), Active: 0, Routes: []rib.Route{{Protocol: rib.OSPF}}},
	}}
	pols := &model.Policies{PrefixLists: map[string][]netip.Prefix{}, Communities: map[string][]string{}, ASPaths: map[string]string{},
		Statements: map[string]*model.PolicyStatement{
			"in": {Name: "in", Terms: []*model.PolicyTerm{
				{Name: "no", From: model.PolicyFrom{RouteFilters: []model.RouteFilter{{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Match: "exact"}}},
					Then: model.PolicyThen{Flow: "reject"}},
				{Name: "lp", Then: model.PolicyThen{LocalPref: lp(200), Preference: pref(20), Flow: "accept"}},
			}},
			"out": {Name: "out", Terms: []*model.PolicyTerm{
				{Name: "st", From: model.PolicyFrom{Protocols: []string{"static"}}, Then: model.PolicyThen{Flow: "accept"}},
			}},
		}}
	d := New(loNet{local: me, ports: ports}, r, quiet)
	go d.Run(ctx)
	d.SetConfig(Config{Policies: pols, Instances: []Instance{{AS: 65001, RouterID: me, Neighbors: []Neighbor{{
		Neighbor: bgp.Neighbor{Addr: peer, PeerAS: 65002, LocalAS: 65001, HoldTime: 9, Families: []bgp.Family{bgp.IPv4Unicast}},
		Import:   []string{"in"}, Export: []string{"out"}}}}}})
	d.SetMaster(true)

	src := peer.String()
	waitFor(t, "routes in the RIB", func() bool { return len(r.routes(src)) == 1 })
	rr := r.routes(src)[0]
	if rr.Prefix != netip.MustParsePrefix("203.0.113.0/24") || rr.Protocol != rib.BGP || rr.NextHops[0].Gateway != peer ||
		rr.Attrs == nil || *rr.Attrs.LocalPref != 200 || rr.Attrs.ASPath != "65002 I" || rr.Preference != 20 {
		t.Fatalf("route %+v attrs %+v", rr, rr.Attrs)
	}
	adj, err := d.Adj(AdjRequest{Neighbor: peer})
	if err != nil || len(adj) != 2 || !adj[0].Hidden || adj[1].Hidden {
		t.Fatalf("receive-protocol %+v %v", adj, err)
	}
	// The export: the static route, not the OSPF one.
	waitFor(t, "export at the router", func() bool {
		rmu.Lock()
		defer rmu.Unlock()
		return len(got) == 1 && got[0].Prefix == netip.MustParsePrefix("10.10.0.0/16")
	})
	waitFor(t, "established, one advertised", func() bool {
		st := d.Status(nil)
		return len(st) == 1 && st[0].Neighbors[0].State == "Established" && st[0].Neighbors[0].Counts[0].Advertised == 1
	})
	// Not the master any more: the instance stops, its routes go.
	d.SetMaster(false)
	waitFor(t, "routes withdrawn", func() bool { return len(r.routes(src)) == 0 })
}

// TCP MD5: a session with the right key connects, a wrong key does not.
func TestLinuxMD5(t *testing.T) {
	ln := LinuxNet{ListenPort: 17900 + int(time.Now().UnixNano()%1000)}
	l, err := ln.Listen("", map[netip.Addr]string{netip.MustParseAddr("127.0.0.1"): "s3cret"})
	if err != nil {
		t.Skipf("listen: %v", err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	dial := func(key string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		defer cancel()
		c, err := ln.Dial(ctx, "", bgp.Neighbor{Addr: netip.MustParseAddr("127.0.0.1"), AuthKey: key, Internal: true})
		if err == nil {
			c.Close()
		}
		return err
	}
	if err := dial("s3cret"); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			t.Skip("no TCP MD5 in this kernel")
		}
		t.Fatalf("right key: %v", err)
	}
	if err := dial("wrong"); err == nil {
		t.Fatal("a wrong key connected")
	}
	if err := dial(""); err == nil {
		t.Fatal("no key connected")
	}
}

// fakeBFD records cer-bfdd's session list.
type fakeBFD struct {
	mu sync.Mutex
	ss []bfdapi.SessionSpec
	n  int
}

func (f *fakeBFD) Set(_ context.Context, s bfdapi.Set) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ss, f.n = s.Sessions, f.n+1
	return nil
}

func (f *fakeBFD) get() []bfdapi.SessionSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ss
}

// BFD for a neighbour: the session is set in cer-bfdd; its failure ends
// the BGP session; without BFD configured the session goes.
func TestDaemonBFD(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ports := &sync.Map{}
	me, peer := netip.MustParseAddr("127.0.0.81"), netip.MustParseAddr("127.0.0.82")
	rn := loNet{local: peer, ports: ports}
	rl, _ := rn.Listen("", nil)
	router := bgp.New(rn.Transport(), bgp.Policy{}, quiet)
	go router.Run(ctx)
	router.Configure(bgp.Config{AS: 65002, RouterID: peer, ConnectRetry: 200 * time.Millisecond, Neighbors: []bgp.Neighbor{{
		Addr: me, PeerAS: 65001, LocalAS: 65002, HoldTime: 9, Families: []bgp.Family{bgp.IPv4Unicast}}}})
	go func() {
		for {
			c, err := rl.Accept()
			if err != nil {
				return
			}
			router.Accept(c)
		}
	}()
	fb := &fakeBFD{}
	d := New(loNet{local: me, ports: ports}, &fakeRIB{sets: map[string]ribapi.SetRoutes{}}, quiet)
	d.BFD = fb
	go d.Run(ctx)
	nb := Neighbor{Neighbor: bgp.Neighbor{Addr: peer, PeerAS: 65002, LocalAS: 65001, HoldTime: 9, Families: []bgp.Family{bgp.IPv4Unicast}},
		BFDCfg: &BFDConfig{IntervalMs: 300, Multiplier: 3}}
	cfg := Config{Instances: []Instance{{AS: 65001, RouterID: me, Neighbors: []Neighbor{nb}}}}
	d.SetConfig(cfg)
	d.SetMaster(true)
	waitFor(t, "BFD session set", func() bool { return len(fb.get()) == 1 })
	s := fb.get()[0]
	if s.Key.Peer != peer || s.Key.Multihop || s.IntervalMs != 300 || s.Multiplier != 3 {
		t.Fatalf("BFD session %+v", s)
	}
	state := func() bgp.NeighborStatus { return d.Status(nil)[0].Neighbors[0] }
	waitFor(t, "established", func() bool { return state().State == "Established" })
	d.BFDChanged(s.Key.String(), true, false)
	d.BFDChanged(s.Key.String(), false, false)
	waitFor(t, "down by BFD", func() bool { return state().LastError == "BFD session down" && state().State != "Established" })
	// BFD removed from the configuration: the session ends in cer-bfdd and
	// BGP comes back.
	nb.BFDCfg = nil
	cfg.Instances[0].Neighbors = []Neighbor{nb}
	d.SetConfig(cfg)
	waitFor(t, "BFD session removed, BGP back", func() bool { return len(fb.get()) == 0 && state().State == "Established" })
}
