package bgpd

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/ribd"
	"github.com/thxrben/cerium-switchd/pkg/bgp"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

// startRouter runs a BGP router at addr (AS 65002) that announces paths.
func startRouter(t *testing.T, ctx context.Context, ports *sync.Map, addr, peer netip.Addr) *bgp.Speaker {
	rn := loNet{local: addr, ports: ports}
	rl, err := rn.Listen("", nil)
	if err != nil {
		t.Fatal(err)
	}
	router := bgp.New(rn.Transport(), bgp.Policy{Export: func(*bgp.Neighbor, *bgp.Path) bool { return true }}, quiet)
	router.RoutesDelay = 10 * time.Millisecond
	go router.Run(ctx)
	router.Configure(bgp.Config{AS: 65002, RouterID: addr, ConnectRetry: 200 * time.Millisecond, Neighbors: []bgp.Neighbor{{
		Addr: peer, PeerAS: 65001, LocalAS: 65002, HoldTime: 9, Families: []bgp.Family{bgp.IPv4Unicast}}}})
	go func() {
		for {
			c, err := rl.Accept()
			if err != nil {
				return
			}
			router.Accept(c)
		}
	}()
	return router
}

func announce(router *bgp.Speaker, n int) {
	var ps []bgp.Path
	for i := range n {
		ps = append(ps, bgp.Path{Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{100, byte(i / 256), byte(i % 256), 0}), 24), Source: "static"})
	}
	router.SetLocal(ps, nil)
}

// legacyMember records whole tables (the protocol of an older release).
type legacyMember struct {
	mu   sync.Mutex
	sets map[string][]rib.Route
}

func (l *legacyMember) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, rs := range l.sets {
		n += len(rs)
	}
	return n
}

// The master's routes reach cer-ribd and every member as changes; a member
// of an older release gets whole tables; a lost delta makes cer-ribd sync
// again and ends with the right table.
func TestRoutesAsDeltas(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ports := &sync.Map{}
	me, peer := netip.MustParseAddr("127.0.0.71"), netip.MustParseAddr("127.0.0.72")
	router := startRouter(t, ctx, ports, peer, me)
	announce(router, 300)

	local := &fakeRIB{sets: map[string]ribd.SetRoutes{}}
	m2 := &fakeRIB{sets: map[string]ribd.SetRoutes{}}
	m3 := &legacyMember{sets: map[string][]rib.Route{}}
	d := New(loNet{local: me, ports: ports}, local, quiet)
	d.Members = func() []int { return []int{2, 3} }
	d.MemberDelta = func(ctx context.Context, m int, dl ribd.RoutesDelta) (ribd.DeltaReply, error) {
		if m == 3 {
			return ribd.DeltaReply{}, errors.New("member 3: unknown operation bgp-routes-delta")
		}
		return m2.Delta(ctx, dl)
	}
	d.MemberSetRoutes = func(_ context.Context, m int, sr ribd.SetRoutes) error {
		m3.mu.Lock()
		m3.sets[sr.Source] = sr.Routes
		m3.mu.Unlock()
		return nil
	}
	go d.Run(ctx)
	d.SetConfig(Config{Instances: []Instance{{AS: 65001, RouterID: me, Neighbors: []Neighbor{{
		Neighbor: bgp.Neighbor{Addr: peer, PeerAS: 65002, LocalAS: 65001, HoldTime: 9, Families: []bgp.Family{bgp.IPv4Unicast}}}}}}})
	d.SetMaster(true)
	src := peer.String()
	all := func(n int) func() bool {
		return func() bool { return len(local.routes(src)) == n && len(m2.routes(src)) == n && m3.count() == n }
	}
	waitFor(t, "300 routes everywhere", all(300))
	// Changes travel as changes: withdraw 100.
	local.mu.Lock()
	syncs, sent := local.syncs, local.prefixes
	local.mu.Unlock()
	announce(router, 200)
	waitFor(t, "200 routes everywhere", all(200))
	local.mu.Lock()
	if local.syncs != syncs {
		t.Errorf("a withdrawal caused a sync (%d -> %d)", syncs, local.syncs)
	}
	// Only the 100 withdrawn prefixes travelled, not the table.
	if n := local.prefixes - sent; n != 100 {
		t.Errorf("%d prefixes sent for 100 withdrawals", n)
	}
	// A lost delta: cer-ribd's sequence jumps.
	local.seq += 7
	local.mu.Unlock()
	announce(router, 150)
	waitFor(t, "150 routes after the resync", all(150))
	local.mu.Lock()
	if local.syncs == syncs {
		t.Error("no sync after a lost delta")
	}
	local.mu.Unlock()
	// The instance stops: the routes go everywhere.
	d.SetMaster(false)
	waitFor(t, "withdrawn", func() bool { return len(local.routes(src)) == 0 && len(m2.routes(src)) == 0 && m3.count() == 0 })
}

// Before convergence a sync keeps the routes of before (cer-bgpd
// restarted: no drop while it learns the table); once converged, a sync
// removes what is gone.
func TestSyncKeepsUntilConverged(t *testing.T) {
	r := &fakeRIB{sets: map[string]ribd.SetRoutes{}}
	ctx := context.Background()
	old := ribd.PrefixRoutes{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Routes: []rib.Route{{Source: "10.0.0.1", Preference: rib.PrefBGP,
		NextHops: []rib.NextHop{{Gateway: netip.MustParseAddr("10.0.0.1")}}}}}
	r.Delta(ctx, ribd.RoutesDelta{Protocol: rib.BGP, Seq: 1, Sync: "begin", Prefixes: []ribd.PrefixRoutes{old}, Full: true})
	r.Delta(ctx, ribd.RoutesDelta{Protocol: rib.BGP, Seq: 2, Sync: "end", Full: true})
	// The restarted daemon's first sync: nothing learned yet.
	r.Delta(ctx, ribd.RoutesDelta{Protocol: rib.BGP, Seq: 1, Sync: "begin"})
	r.Delta(ctx, ribd.RoutesDelta{Protocol: rib.BGP, Seq: 2, Sync: "end"})
	if n := len(r.routes("10.0.0.1")); n != 1 {
		t.Fatalf("a sync before convergence removed routes (%d left)", n)
	}
	r.Delta(ctx, ribd.RoutesDelta{Protocol: rib.BGP, Seq: 3, Sync: "begin", Full: true})
	r.Delta(ctx, ribd.RoutesDelta{Protocol: rib.BGP, Seq: 4, Sync: "end", Full: true})
	if n := len(r.routes("10.0.0.1")); n != 0 {
		t.Fatalf("the converged sync kept %d routes that are gone", n)
	}
}
