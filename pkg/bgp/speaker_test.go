package bgp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(logOut(), nil))

func logOut() io.Writer {
	if os.Getenv("BGPLOG") != "" {
		return os.Stderr
	}
	return io.Discard
}

// net127 runs speakers on loopback addresses (127.0.0.x), each with its
// own listener; dialing a neighbour reaches its listener.
type net127 struct {
	t     *testing.T
	ctx   context.Context
	mu    sync.Mutex
	ports map[netip.Addr]int
	conns map[netip.Addr][]net.Conn // by the speaker's address, to cut them
}

func newNet(t *testing.T) *net127 {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &net127{t: t, ctx: ctx, ports: map[netip.Addr]int{}, conns: map[netip.Addr][]net.Conn{}}
}

type transport struct {
	n     *net127
	local netip.Addr
}

func (tr transport) Dial(ctx context.Context, nb Neighbor) (net.Conn, error) {
	tr.n.mu.Lock()
	port := tr.n.ports[nb.Addr]
	tr.n.mu.Unlock()
	if port == 0 {
		return nil, fmt.Errorf("%s does not listen", nb.Addr)
	}
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: tr.local.AsSlice()}}
	c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(nb.Addr, uint16(port)).String())
	if err == nil {
		tr.n.track(tr.local, c)
	}
	return c, err
}

func (n *net127) track(a netip.Addr, c net.Conn) {
	n.mu.Lock()
	n.conns[a] = append(n.conns[a], c)
	n.mu.Unlock()
}

// cut closes every connection of a speaker (a crash: no NOTIFICATION).
func (n *net127) cut(a netip.Addr) {
	n.mu.Lock()
	cs := n.conns[a]
	n.conns[a] = nil
	n.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
}

// speaker starts a speaker on addr; ctx ends it.
func (n *net127) speaker(ctx context.Context, addr string, as uint32, pol Policy, nbrs ...Neighbor) (*Speaker, *routes) {
	a := netip.MustParseAddr(addr)
	l, err := net.Listen("tcp", netip.AddrPortFrom(a, 0).String())
	if err != nil {
		n.t.Fatal(err)
	}
	s := New(transport{n, a}, pol, quiet)
	rs := &routes{}
	s.OnRoutes = rs.set
	s.RoutesDelay = 10 * time.Millisecond
	for i := range nbrs {
		if nbrs[i].LocalAS == 0 {
			nbrs[i].LocalAS = as
		}
	}
	go s.Run(ctx)
	s.Configure(Config{AS: as, RouterID: a, Neighbors: nbrs, ConnectRetry: 200 * time.Millisecond})
	n.mu.Lock()
	n.ports[a] = l.Addr().(*net.TCPAddr).Port
	n.mu.Unlock()
	go func() {
		<-ctx.Done()
		l.Close()
		n.mu.Lock()
		delete(n.ports, a)
		n.mu.Unlock()
		n.cut(a)
	}()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			n.track(a, c)
			s.Accept(c)
		}
	}()
	return s, rs
}

// routes collects OnRoutes.
type routes struct {
	mu sync.Mutex
	rs []Route
}

func (r *routes) set(rs []Route, _ bool) {
	r.mu.Lock()
	r.rs = rs
	r.mu.Unlock()
}

func (r *routes) get(prefix string) []Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Route
	for _, x := range r.rs {
		if x.Prefix == netip.MustParsePrefix(prefix) {
			out = append(out, x)
		}
	}
	return out
}

func nbr(addr string, peerAS uint32, internal bool) Neighbor {
	return Neighbor{Addr: netip.MustParseAddr(addr), PeerAS: peerAS, Internal: internal, HoldTime: 9,
		Families: []Family{IPv4Unicast, IPv6Unicast}, GracefulRestart: true, RestartTime: 5, StaleTime: 10}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
	}
}

func state(s *Speaker, addr string) NeighborStatus {
	for _, st := range s.Status() {
		if st.Addr == netip.MustParseAddr(addr) {
			return st
		}
	}
	return NeighborStatus{}
}

// exportAll announces originated paths and passes BGP paths unchanged.
var exportAll = Policy{Export: func(*Neighbor, *Path) bool { return true }}

func local(prefix string) Path {
	return Path{Prefix: netip.MustParsePrefix(prefix), Source: "static", Attrs: Attrs{Origin: OriginIGP}}
}

// eBGP between two speakers: an originated route arrives with the AS path
// and next hop of the sender, goes away when withdrawn; neither side
// flaps.
func TestEBGP(t *testing.T) {
	n := newNet(t)
	a, _ := n.speaker(n.ctx, "127.0.0.1", 65001, exportAll, nbr("127.0.0.2", 65002, false))
	_, rb := n.speaker(n.ctx, "127.0.0.2", 4200000002, exportAll, nbr("127.0.0.1", 65001, false))
	// (b's AS is 4-byte; a's neighbour config must say so)
	a.Configure(Config{AS: 65001, RouterID: netip.MustParseAddr("127.0.0.1"), ConnectRetry: 200 * time.Millisecond,
		Neighbors: []Neighbor{func() Neighbor { x := nbr("127.0.0.2", 4200000002, false); x.LocalAS = 65001; return x }()}})
	waitFor(t, "established", func() bool { return state(a, "127.0.0.2").State == "Established" })
	a.SetLocal([]Path{local("10.0.0.0/24"), local("2001:db8::/48")}, nil)
	waitFor(t, "routes at b", func() bool { return len(rb.get("10.0.0.0/24")) == 1 && len(rb.get("2001:db8::/48")) == 0 })
	r := rb.get("10.0.0.0/24")[0]
	if r.PathString() != "65001 I" || r.NextHop != netip.MustParseAddr("127.0.0.1") || !r.EBGP || r.Rank != 0 || r.LocalPref != nil {
		t.Fatalf("route at b: %+v (%s)", r, r.PathString())
	}
	// The IPv6 prefix has no IPv6 next hop on an IPv4 session: not sent.
	a.SetLocal(nil, nil)
	waitFor(t, "withdrawn", func() bool { return len(rb.get("10.0.0.0/24")) == 0 })
	if st := state(a, "127.0.0.2"); st.Stats.Flaps != 0 || !st.AS4 || st.HoldTime != 9 {
		t.Fatalf("a's view of b: %+v", st)
	}
}

// Route reflection: a client's route reaches the other client with the
// originator id and cluster list; without a cluster nothing is reflected.
func TestRouteReflection(t *testing.T) {
	n := newNet(t)
	cl := netip.MustParseAddr("10.255.255.1")
	c1n, c2n := nbr("127.0.0.11", 65000, true), nbr("127.0.0.12", 65000, true)
	c1n.Cluster, c2n.Cluster = cl, cl
	rr, _ := n.speaker(n.ctx, "127.0.0.10", 65000, exportAll, c1n, c2n)
	c1, _ := n.speaker(n.ctx, "127.0.0.11", 65000, exportAll, nbr("127.0.0.10", 65000, true))
	_, r2 := n.speaker(n.ctx, "127.0.0.12", 65000, exportAll, nbr("127.0.0.10", 65000, true))
	waitFor(t, "sessions", func() bool {
		return state(rr, "127.0.0.11").State == "Established" && state(rr, "127.0.0.12").State == "Established"
	})
	c1.SetLocal([]Path{local("10.1.0.0/16")}, nil)
	waitFor(t, "reflected", func() bool { return len(r2.get("10.1.0.0/16")) == 1 })
	r := r2.get("10.1.0.0/16")[0]
	if r.OriginatorID != netip.MustParseAddr("127.0.0.11") || !slices.Equal(r.ClusterList, []netip.Addr{cl}) ||
		r.NextHop != netip.MustParseAddr("127.0.0.11") || r.localPref() != 100 || r.EBGP {
		t.Fatalf("reflected route %+v", r)
	}
	// No longer a route reflector: c2 loses the route.
	c1n.Cluster, c2n.Cluster = netip.Addr{}, netip.Addr{}
	c1n.LocalAS, c2n.LocalAS = 65000, 65000
	rr.Configure(Config{AS: 65000, RouterID: netip.MustParseAddr("127.0.0.10"), Neighbors: []Neighbor{c1n, c2n}, ConnectRetry: 200 * time.Millisecond})
	waitFor(t, "not reflected", func() bool { return len(r2.get("10.1.0.0/16")) == 0 })
}

// Import policy: a rejected path is hidden; a new policy takes effect from
// the Adj-RIB-In without resetting the session.
func TestImportPolicy(t *testing.T) {
	n := newNet(t)
	a, _ := n.speaker(n.ctx, "127.0.0.21", 65001, exportAll, nbr("127.0.0.22", 65002, false))
	reject := Policy{Import: func(_ *Neighbor, p *Path) bool { return p.Prefix.Bits() < 24 }}
	b, rb := n.speaker(n.ctx, "127.0.0.22", 65002, reject, nbr("127.0.0.21", 65001, false))
	a.SetLocal([]Path{local("10.2.0.0/16"), local("10.3.1.0/24")}, nil)
	waitFor(t, "accepted /16", func() bool { return len(rb.get("10.2.0.0/16")) == 1 })
	in := b.AdjIn(netip.MustParseAddr("127.0.0.21"))
	if len(in) != 2 || in[0].Hidden || !in[1].Hidden || len(rb.get("10.3.1.0/24")) != 0 {
		t.Fatalf("adj-rib-in %+v", in)
	}
	lp := uint32(250)
	b.SetPolicy(Policy{Import: func(_ *Neighbor, p *Path) bool { p.LocalPref = &lp; return true }})
	waitFor(t, "accepted /24", func() bool {
		r := rb.get("10.3.1.0/24")
		return len(r) == 1 && r[0].localPref() == 250
	})
	if st := state(b, "127.0.0.21"); st.Stats.Flaps != 0 || st.Counts[0].Received != 2 || st.Counts[0].Accepted != 2 || st.Counts[0].Active != 2 {
		t.Fatalf("b's view of a: %+v", st)
	}
	if out := a.AdjOut(netip.MustParseAddr("127.0.0.22")); len(out) != 2 || out[0].PathString() != "65001 I" {
		t.Fatalf("adj-rib-out %+v", out)
	}
}

// Multipath: two neighbours of one AS announce a prefix; with multipath
// both are used, without it one; a loop (our own AS) is rejected.
func TestMultipathAndLoop(t *testing.T) {
	n := newNet(t)
	b1, b2 := nbr("127.0.0.31", 65001, false), nbr("127.0.0.32", 65001, false)
	b1.Multipath, b2.Multipath = true, true
	c, rc := n.speaker(n.ctx, "127.0.0.30", 65000, exportAll, b1, b2)
	a1, _ := n.speaker(n.ctx, "127.0.0.31", 65001, exportAll, nbr("127.0.0.30", 65000, false))
	a2, _ := n.speaker(n.ctx, "127.0.0.32", 65001, exportAll, nbr("127.0.0.30", 65000, false))
	a1.SetLocal([]Path{local("10.4.0.0/16")}, nil)
	looped := local("10.5.0.0/16")
	looped.ASPath = []Segment{{ASNs: []uint32{65000}}}
	a2.SetLocal([]Path{local("10.4.0.0/16"), looped}, nil)
	waitFor(t, "two paths", func() bool { return len(rc.get("10.4.0.0/16")) == 2 })
	rs := rc.get("10.4.0.0/16")
	if rs[0].Rank != 0 || rs[1].Rank != 0 {
		t.Fatalf("multipath ranks %d %d", rs[0].Rank, rs[1].Rank)
	}
	time.Sleep(100 * time.Millisecond)
	if len(rc.get("10.5.0.0/16")) != 0 {
		t.Fatal("a path with our own AS must be rejected")
	}
	b1.Multipath, b2.Multipath = false, false
	b1.LocalAS, b2.LocalAS = 65000, 65000
	c.Configure(Config{AS: 65000, RouterID: netip.MustParseAddr("127.0.0.30"), Neighbors: []Neighbor{b1, b2}, ConnectRetry: 200 * time.Millisecond})
	waitFor(t, "one used", func() bool {
		rs := rc.get("10.4.0.0/16")
		return len(rs) == 2 && rs[0].Rank == 0 && rs[1].Rank == 1
	})
	if st := state(c, "127.0.0.31"); st.Stats.Flaps != 0 {
		t.Fatal("changing multipath must not reset the session")
	}
	// A hold time change resets the session.
	b1.HoldTime = 30
	c.Configure(Config{AS: 65000, RouterID: netip.MustParseAddr("127.0.0.30"), Neighbors: []Neighbor{b1, b2}, ConnectRetry: 200 * time.Millisecond})
	waitFor(t, "reset and back", func() bool {
		st := state(c, "127.0.0.31")
		return st.Stats.Flaps == 1 && st.State == "Established"
	})
	if st := state(c, "127.0.0.32"); st.Stats.Flaps != 0 {
		t.Fatal("the other neighbour must keep its session")
	}
}

// Graceful restart: a neighbour that disappears without NOTIFICATION
// keeps its routes (stale) until it is back and has sent End-of-RIB.
func TestGracefulRestart(t *testing.T) {
	n := newNet(t)
	b, rb := n.speaker(n.ctx, "127.0.0.42", 65002, exportAll, nbr("127.0.0.41", 65001, false))
	actx, acancel := context.WithCancel(n.ctx)
	a, _ := n.speaker(actx, "127.0.0.41", 65001, exportAll, nbr("127.0.0.42", 65002, false))
	a.SetLocal([]Path{local("10.6.0.0/16"), local("10.7.0.0/16")}, nil)
	waitFor(t, "routes", func() bool { return len(rb.get("10.6.0.0/16")) == 1 && len(rb.get("10.7.0.0/16")) == 1 })
	// a crashes: its connections drop without a NOTIFICATION.
	n.cut(netip.MustParseAddr("127.0.0.41"))
	n.cut(netip.MustParseAddr("127.0.0.42"))
	acancel()
	waitFor(t, "stale routes kept", func() bool {
		r := rb.get("10.6.0.0/16")
		return state(b, "127.0.0.41").Stale && len(r) == 1 && r[0].Stale
	})
	// a is back with one of the two routes.
	time.Sleep(100 * time.Millisecond)
	a2, _ := n.speaker(n.ctx, "127.0.0.41", 65001, exportAll, nbr("127.0.0.42", 65002, false))
	a2.SetLocal([]Path{local("10.6.0.0/16")}, nil)
	waitFor(t, "stale swept", func() bool {
		r := rb.get("10.6.0.0/16")
		return !state(b, "127.0.0.41").Stale && len(r) == 1 && !r[0].Stale && len(rb.get("10.7.0.0/16")) == 0
	})
}

// A neighbour with the wrong AS is refused with a NOTIFICATION.
func TestBadPeerAS(t *testing.T) {
	n := newNet(t)
	a, _ := n.speaker(n.ctx, "127.0.0.51", 65001, exportAll, nbr("127.0.0.52", 65099, false))
	n.speaker(n.ctx, "127.0.0.52", 65002, exportAll, nbr("127.0.0.51", 65001, false))
	waitFor(t, "refused", func() bool {
		st := state(a, "127.0.0.52")
		return st.State != "Established" && st.LastError != ""
	})
	time.Sleep(500 * time.Millisecond)
	if st := state(a, "127.0.0.52"); st.State == "Established" {
		t.Fatal("established with the wrong AS")
	}
}
