package ospf

import (
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// A simulated network: routers joined by segments (broadcast networks or
// point-to-point links); packets are queued and delivered in order.

type simPort struct {
	r     *simRouter
	iface string
	addr  netip.Addr // source address (v2: IPv4, v3: link-local)
	seg   *simSeg
}

type simSeg struct {
	name  string
	ports []*simPort
	down  bool
}

type simRouter struct {
	name  string
	r     *Router
	cfg   Config
	ports map[string]*simPort
	n     *simNet
}

type simPkt struct {
	from *simPort
	dst  netip.Addr
	raw  []byte
}

type simNet struct {
	t       *testing.T
	v       Version
	now     time.Time
	routers map[string]*simRouter
	segs    map[string]*simSeg
	queue   []simPkt
	nseg    int
	// drop: packets not delivered (by type), for loss tests.
	dropType map[uint8]bool
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newSim(t *testing.T, v Version) *simNet {
	return &simNet{t: t, v: v, now: time.Unix(1_000_000, 0), routers: map[string]*simRouter{}, segs: map[string]*simSeg{}}
}

// Send implements IO for one router.
func (s *simRouter) Send(ifname string, dst netip.Addr, pkt []byte) {
	p := s.ports[ifname]
	if p == nil || p.seg.down {
		return
	}
	s.n.queue = append(s.n.queue, simPkt{from: p, dst: dst, raw: slices.Clone(pkt)})
}

func (n *simNet) router(name string, id string) *simRouter {
	rid, _ := ParseID(id)
	sr := &simRouter{name: name, ports: map[string]*simPort{}, n: n}
	sr.r = New(n.v, sr, quiet, n.now)
	sr.cfg = Config{RouterID: rid}
	n.routers[name] = sr
	return sr
}

type ifOpt func(*IfaceConfig)

func p2p(c *IfaceConfig)  { c.P2P = true }
func cost(n uint16) ifOpt { return func(c *IfaceConfig) { c.Cost = n } }
func areaOf(id ID) ifOpt  { return func(c *IfaceConfig) { c.Area = id } }

// connect joins routers to a new segment; every router gets an interface
// "<seg>" with address .<k> (v2 10.<seg>.0.k/24, v3 fe80::k and
// 2001:db8:<seg>::/64).
func (n *simNet) connect(name string, members []string, opts ...ifOpt) {
	n.nseg++
	seg := &simSeg{name: name}
	n.segs[name] = seg
	for k, m := range members {
		sr := n.routers[m]
		host := k + 1
		c := IfaceConfig{Name: name, ID: uint32(100*n.nseg + host), Cost: 10, Priority: 1, Hello: 10, Dead: 40, Retransmit: 5,
			TransitDelay: 1, MTU: 1500, Up: true}
		if n.v == V2 {
			c.Addr = netip.AddrFrom4([4]byte{10, byte(n.nseg), 0, byte(host)})
			c.Prefixes = []netip.Prefix{netip.PrefixFrom(c.Addr, 24)}
		} else {
			c.Addr = netip.MustParseAddr(fmt.Sprintf("fe80::%d:%d", n.nseg, host))
			c.Prefixes = []netip.Prefix{netip.MustParsePrefix(fmt.Sprintf("2001:db8:%x::/64", n.nseg))}
		}
		for _, o := range opts {
			o(&c)
		}
		port := &simPort{r: sr, iface: name, addr: c.Addr, seg: seg}
		seg.ports = append(seg.ports, port)
		sr.ports[name] = port
		sr.cfg.Interfaces = append(sr.cfg.Interfaces, c)
	}
}

// stub gives a router a passive interface with its own prefix.
func (n *simNet) stub(router, name string, prefix string, opts ...ifOpt) {
	sr := n.routers[router]
	p := netip.MustParsePrefix(prefix)
	c := IfaceConfig{Name: name, ID: uint32(9000 + len(sr.cfg.Interfaces)), Cost: 1, Passive: true, Hello: 10, Dead: 40, Up: true,
		Addr: p.Addr(), Prefixes: []netip.Prefix{p}}
	if n.v == V3 {
		c.Addr = netip.MustParseAddr("fe80::99")
	}
	for _, o := range opts {
		o(&c)
	}
	sr.cfg.Interfaces = append(sr.cfg.Interfaces, c)
}

func (n *simNet) start() {
	for _, name := range n.names() {
		sr := n.routers[name]
		sr.r.Configure(sr.cfg, n.now)
	}
	n.deliver()
}

func (n *simNet) names() []string {
	var out []string
	for k := range n.routers {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (n *simNet) deliver() {
	for steps := 0; len(n.queue) > 0; steps++ {
		if steps > 200000 {
			count := map[string]int{}
			for _, q := range n.queue[:min(len(n.queue), 5000)] {
				if d, err := Decode(n.v, q.raw); err == nil {
					count[fmt.Sprintf("%s from %s to %s", TypeName(d.Type), q.from.r.name, q.dst)]++
				}
			}
			n.t.Fatalf("packet storm: %v", count)
		}
		p := n.queue[0]
		n.queue = n.queue[1:]
		if p.from.seg.down {
			continue
		}
		if n.dropType != nil {
			if q, err := Decode(n.v, p.raw); err == nil && n.dropType[q.Type] {
				continue
			}
		}
		multicast := p.dst.IsMulticast()
		for _, to := range p.from.seg.ports {
			if to == p.from || (!multicast && to.addr != p.dst) {
				continue
			}
			to.r.r.Receive(to.iface, p.from.addr, p.dst, p.raw, n.now)
		}
	}
}

// run advances the clock in 100 ms steps.
func (n *simNet) run(d time.Duration) {
	for end := n.now.Add(d); n.now.Before(end); {
		n.now = n.now.Add(100 * time.Millisecond)
		for _, name := range n.names() {
			n.routers[name].r.Tick(n.now)
		}
		n.deliver()
	}
}

func (n *simNet) nbrStates(router, ifname string) map[ID]NbrState {
	out := map[ID]NbrState{}
	for _, nb := range n.routers[router].r.ifaces[ifname].nbrs {
		out[nb.id] = nb.state
	}
	return out
}

func (n *simNet) route(router, prefix string) *Route {
	p := netip.MustParsePrefix(prefix)
	for _, rt := range n.routers[router].r.Routes() {
		if rt.Prefix == p {
			return &rt
		}
	}
	return nil
}

func (n *simNet) dump(router string) string {
	var b strings.Builder
	for _, rt := range n.routers[router].r.Routes() {
		fmt.Fprintf(&b, "  %s %s cost %d/%d direct=%v %v\n", rt.Prefix, rt.Type, rt.Cost, rt.Cost2, rt.Direct, rt.NextHops)
	}
	return b.String()
}

func id(s string) ID { v, _ := ParseID(s); return v }

func versions(t *testing.T, f func(t *testing.T, v Version)) {
	for _, v := range []Version{V2, V3} {
		t.Run(fmt.Sprintf("v%d", v), func(t *testing.T) { f(t, v) })
	}
}

// v4v6 picks the prefix of the version.
func v4v6(v Version, p4, p6 string) string {
	if v == V2 {
		return p4
	}
	return p6
}

// ---- tests ----

func TestP2PAdjacency(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n := newSim(t, v)
		n.router("r1", "1.1.1.1")
		n.router("r2", "2.2.2.2")
		n.connect("l12", []string{"r1", "r2"}, p2p)
		n.stub("r2", "lo", v4v6(v, "192.0.2.2/32", "2001:db8:ff::2/128"))
		n.start()
		n.run(5 * time.Second)
		if st := n.nbrStates("r1", "l12"); st[id("2.2.2.2")] != NbrFull {
			t.Fatalf("r1's neighbours %v", st)
		}
		rt := n.route("r1", v4v6(v, "192.0.2.2/32", "2001:db8:ff::2/128"))
		if rt == nil || rt.Type != IntraArea || rt.Cost != 11 || len(rt.NextHops) != 1 || rt.NextHops[0].Iface != "l12" {
			t.Fatalf("route to r2's loopback: %+v\n%s", rt, n.dump("r1"))
		}
		want := v4v6(v, "10.1.0.2", "fe80::1:2")
		if rt.NextHops[0].Gateway != netip.MustParseAddr(want) {
			t.Fatalf("gateway %v, want %s", rt.NextHops[0].Gateway, want)
		}
		if l := n.route("r1", v4v6(v, "10.1.0.0/24", "2001:db8:1::/64")); l == nil || !l.Direct {
			t.Fatalf("the link's own prefix: %+v", l)
		}
	})
}

func TestBroadcastDR(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n := newSim(t, v)
		for k := 1; k <= 4; k++ {
			n.router(fmt.Sprintf("r%d", k), fmt.Sprintf("%d.%d.%d.%d", k, k, k, k))
		}
		n.connect("lan", []string{"r1", "r2", "r3", "r4"})
		n.stub("r1", "lo", v4v6(v, "192.0.2.1/32", "2001:db8:ff::1/128"))
		n.start()
		n.run(50 * time.Second) // the wait timer is the dead interval
		r4 := n.routers["r4"].r.ifaces["lan"]
		r3 := n.routers["r3"].r.ifaces["lan"]
		if r4.state != IfDR || r3.state != IfBackup {
			t.Fatalf("r4 %s, r3 %s: the highest router ids win", r4.state, r3.state)
		}
		// DROthers are 2Way with each other, Full with DR and BDR.
		st := n.nbrStates("r1", "lan")
		if st[id("4.4.4.4")] != NbrFull || st[id("3.3.3.3")] != NbrFull || st[id("2.2.2.2")] != NbrTwoWay {
			t.Fatalf("r1's neighbours %v", st)
		}
		rt := n.route("r2", v4v6(v, "192.0.2.1/32", "2001:db8:ff::1/128"))
		if rt == nil || rt.Cost != 11 || rt.NextHops[0].Gateway != n.routers["r1"].ports["lan"].addr {
			t.Fatalf("r2 to r1's loopback: %+v\n%s", rt, n.dump("r2"))
		}
		// The DR fails: the BDR takes over, routes come back.
		n.segs["lan"].ports = slices.DeleteFunc(n.segs["lan"].ports, func(p *simPort) bool { return p.r.name == "r4" })
		n.run(50 * time.Second)
		if r3.state != IfDR {
			t.Fatalf("after the DR failed r3 is %s", r3.state)
		}
		if rt := n.route("r2", v4v6(v, "192.0.2.1/32", "2001:db8:ff::1/128")); rt == nil {
			t.Fatalf("no route after the DR failed\n%s", n.dump("r2"))
		}
	})
}

// Two equal paths r1-r2-r4 and r1-r3-r4: both next hops.
func TestECMP(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n := newSim(t, v)
		for k := 1; k <= 4; k++ {
			n.router(fmt.Sprintf("r%d", k), fmt.Sprintf("%d.%d.%d.%d", k, k, k, k))
		}
		n.connect("a", []string{"r1", "r2"}, p2p)
		n.connect("b", []string{"r1", "r3"}, p2p)
		n.connect("c", []string{"r2", "r4"}, p2p)
		n.connect("d", []string{"r3", "r4"})
		n.stub("r4", "lo", v4v6(v, "192.0.2.4/32", "2001:db8:ff::4/128"))
		n.start()
		n.run(60 * time.Second)
		rt := n.route("r1", v4v6(v, "192.0.2.4/32", "2001:db8:ff::4/128"))
		if rt == nil || rt.Cost != 21 || len(rt.NextHops) != 2 {
			t.Fatalf("ECMP route: %+v\n%s", rt, n.dump("r1"))
		}
		if rt.NextHops[0].Iface != "a" || rt.NextHops[1].Iface != "b" {
			t.Fatalf("next hops %v", rt.NextHops)
		}
		// One path costs more: one next hop.
		cfg := n.routers["r2"].cfg
		for k := range cfg.Interfaces {
			if cfg.Interfaces[k].Name == "c" {
				cfg.Interfaces[k].Cost = 50
			}
		}
		n.routers["r2"].r.Configure(cfg, n.now)
		n.run(10 * time.Second)
		if rt := n.route("r1", v4v6(v, "192.0.2.4/32", "2001:db8:ff::4/128")); rt == nil || len(rt.NextHops) != 1 || rt.NextHops[0].Iface != "b" {
			t.Fatalf("after the cost change: %+v", rt)
		}
		// No adjacency was reset by the cost change.
		if st := n.nbrStates("r1", "a"); st[id("2.2.2.2")] != NbrFull {
			t.Fatalf("adjacency %v", st)
		}
	})
}

// r1 (area 1) - r2 (ABR) - r3 (area 0, ASBR with a default route).
func TestAreasAndExternals(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n := newSim(t, v)
		n.router("r1", "1.1.1.1")
		n.router("r2", "2.2.2.2")
		n.router("r3", "3.3.3.3")
		n.connect("x", []string{"r1", "r2"}, p2p, areaOf(1))
		n.connect("y", []string{"r2", "r3"}, p2p)
		n.stub("r1", "lo", v4v6(v, "192.0.2.1/32", "2001:db8:ff::1/128"), areaOf(1))
		n.stub("r3", "lo", v4v6(v, "192.0.2.3/32", "2001:db8:ff::3/128"))
		n.routers["r3"].cfg.Externals = []External{{Prefix: netip.MustParsePrefix(v4v6(v, "0.0.0.0/0", "::/0")), Metric: 5}}
		n.start()
		n.run(60 * time.Second)
		if !n.routers["r2"].r.isABR() {
			t.Fatal("r2 is not an ABR")
		}
		rt := n.route("r1", v4v6(v, "192.0.2.3/32", "2001:db8:ff::3/128"))
		if rt == nil || rt.Type != InterArea || rt.Cost != 21 {
			t.Fatalf("r1 to r3's loopback: %+v\n%s", rt, n.dump("r1"))
		}
		rt = n.route("r3", v4v6(v, "192.0.2.1/32", "2001:db8:ff::1/128"))
		if rt == nil || rt.Type != InterArea || rt.Cost != 21 {
			t.Fatalf("r3 to r1's loopback: %+v\n%s", rt, n.dump("r3"))
		}
		def := n.route("r1", v4v6(v, "0.0.0.0/0", "::/0"))
		if def == nil || def.Type != External2 || def.Cost2 != 5 || def.Cost != 20 {
			t.Fatalf("r1's default route: %+v\n%s", def, n.dump("r1"))
		}
		// The export stops: the route goes away.
		n.routers["r3"].r.SetExternals(nil, n.now)
		n.run(10 * time.Second)
		if def := n.route("r1", v4v6(v, "0.0.0.0/0", "::/0")); def != nil {
			t.Fatalf("default route still there: %+v", def)
		}
	})
}

// A link fails without notice: the neighbour dies after the dead
// interval and traffic takes the other path.
func TestFailover(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n := newSim(t, v)
		for k := 1; k <= 3; k++ {
			n.router(fmt.Sprintf("r%d", k), fmt.Sprintf("%d.%d.%d.%d", k, k, k, k))
		}
		n.connect("a", []string{"r1", "r2"}, p2p)
		n.connect("b", []string{"r1", "r3"}, p2p, cost(20))
		n.connect("c", []string{"r3", "r2"}, p2p)
		n.stub("r2", "lo", v4v6(v, "192.0.2.2/32", "2001:db8:ff::2/128"))
		n.start()
		n.run(30 * time.Second)
		if rt := n.route("r1", v4v6(v, "192.0.2.2/32", "2001:db8:ff::2/128")); rt == nil || rt.NextHops[0].Iface != "a" {
			t.Fatalf("before: %+v", rt)
		}
		n.segs["a"].down = true
		n.run(45 * time.Second)
		rt := n.route("r1", v4v6(v, "192.0.2.2/32", "2001:db8:ff::2/128"))
		if rt == nil || rt.NextHops[0].Iface != "b" || rt.Cost != 31 {
			t.Fatalf("after the link failed: %+v\n%s", rt, n.dump("r1"))
		}
		// Back: the shorter path again.
		n.segs["a"].down = false
		n.run(30 * time.Second)
		if rt := n.route("r1", v4v6(v, "192.0.2.2/32", "2001:db8:ff::2/128")); rt == nil || rt.NextHops[0].Iface != "a" {
			t.Fatalf("after the link came back: %+v", rt)
		}
	})
}

// A router restarts (empty database, sequence numbers from the start): it
// learns its own old LSAs and originates newer ones.
func TestRestartSequence(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n := newSim(t, v)
		n.router("r1", "1.1.1.1")
		n.router("r2", "2.2.2.2")
		n.connect("l", []string{"r1", "r2"})
		n.stub("r2", "lo", v4v6(v, "192.0.2.2/32", "2001:db8:ff::2/128"))
		n.start()
		n.run(60 * time.Second)
		// Many changes on r2 raise its sequence numbers.
		for k := range 3 {
			cfg := n.routers["r2"].cfg
			cfg.Interfaces[1].Cost = uint16(2 + k)
			n.routers["r2"].r.Configure(cfg, n.now)
			n.run(6 * time.Second)
		}
		old := n.routers["r1"].r.areas[Backbone].sc.db.OfType(rtrType(v), n.now)
		var r2seq int32
		for _, l := range old {
			if l.AdvRtr == id("2.2.2.2") {
				r2seq = l.Seq
			}
		}
		// r2 restarts.
		sr := n.routers["r2"]
		sr.r = New(v, sr, quiet, n.now)
		sr.r.Configure(sr.cfg, n.now)
		n.run(60 * time.Second)
		for _, l := range n.routers["r1"].r.areas[Backbone].sc.db.OfType(rtrType(v), n.now) {
			if l.AdvRtr == id("2.2.2.2") && l.Seq <= r2seq {
				t.Fatalf("r2's router LSA seq %x after the restart, before %x", uint32(l.Seq), uint32(r2seq))
			}
		}
		if rt := n.route("r1", v4v6(v, "192.0.2.2/32", "2001:db8:ff::2/128")); rt == nil || rt.Cost != 14 {
			t.Fatalf("after r2's restart: %+v\n%s", rt, n.dump("r1"))
		}
	})
}

func rtrType(v Version) LSType {
	if v == V2 {
		return V2Router
	}
	return V3Router
}

// Lost packets are retransmitted: the databases still converge.
func TestLossyExchange(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n := newSim(t, v)
		n.router("r1", "1.1.1.1")
		n.router("r2", "2.2.2.2")
		n.connect("l", []string{"r1", "r2"}, p2p)
		for k := range 30 {
			n.stub("r2", fmt.Sprintf("s%d", k), v4v6(v, fmt.Sprintf("192.0.%d.1/24", k+10), fmt.Sprintf("2001:db8:%x::1/64", k+0x100)))
		}
		n.routers["r2"].cfg.Externals = nil
		for k := range 200 {
			n.routers["r2"].cfg.Externals = append(n.routers["r2"].cfg.Externals, External{
				Prefix: netip.MustParsePrefix(v4v6(v, fmt.Sprintf("172.16.%d.0/24", k), fmt.Sprintf("2001:db8:aa:%x::/64", k)))})
		}
		n.dropType = map[uint8]bool{TypeLSAck: true}
		n.start()
		n.run(20 * time.Second)
		n.dropType = nil
		n.run(20 * time.Second)
		d1 := n.routers["r1"].r.as.db.Len()
		if d1 != 200 {
			t.Fatalf("r1 has %d externals", d1)
		}
		if st := n.nbrStates("r1", "l"); st[id("2.2.2.2")] != NbrFull {
			t.Fatalf("neighbours %v", st)
		}
		if rt := n.route("r1", v4v6(v, "172.16.199.0/24", "2001:db8:aa:c7::/64")); rt == nil {
			t.Fatal("no external route")
		}
	})
}

// Overload: r2 is avoided as transit while another path exists.
func TestOverload(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n := newSim(t, v)
		for k := 1; k <= 4; k++ {
			n.router(fmt.Sprintf("r%d", k), fmt.Sprintf("%d.%d.%d.%d", k, k, k, k))
		}
		n.connect("a", []string{"r1", "r2"}, p2p)
		n.connect("b", []string{"r2", "r4"}, p2p)
		n.connect("c", []string{"r1", "r3"}, p2p, cost(30))
		n.connect("d", []string{"r3", "r4"}, p2p, cost(30))
		n.stub("r4", "lo", v4v6(v, "192.0.2.4/32", "2001:db8:ff::4/128"))
		n.start()
		n.run(30 * time.Second)
		if rt := n.route("r1", v4v6(v, "192.0.2.4/32", "2001:db8:ff::4/128")); rt == nil || rt.NextHops[0].Iface != "a" {
			t.Fatalf("before: %+v", rt)
		}
		n.routers["r2"].r.SetOverload(true, n.now)
		n.run(10 * time.Second)
		if rt := n.route("r1", v4v6(v, "192.0.2.4/32", "2001:db8:ff::4/128")); rt == nil || rt.NextHops[0].Iface != "c" {
			t.Fatalf("overloaded r2 still used: %+v\n%s", rt, n.dump("r1"))
		}
	})
}

// A larger MTU on the neighbour, or other hello timers: no adjacency.
func TestMismatches(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n := newSim(t, v)
		n.router("r1", "1.1.1.1")
		n.router("r2", "2.2.2.2")
		n.connect("mtu", []string{"r1", "r2"}, p2p)
		n.routers["r2"].cfg.Interfaces[0].MTU = 9000
		n.connect("hello", []string{"r1", "r2"}, p2p)
		n.routers["r2"].cfg.Interfaces[1].Hello = 5
		n.start()
		n.run(60 * time.Second)
		if st := n.nbrStates("r1", "mtu"); st[id("2.2.2.2")] == NbrFull {
			t.Errorf("full adjacency over an MTU mismatch: %v", st)
		}
		if st := n.nbrStates("r1", "hello"); len(st) != 0 {
			t.Errorf("neighbour despite other hello timers: %v", st)
		}
	})
}

func TestMD5(t *testing.T) {
	n := newSim(t, V2)
	n.router("r1", "1.1.1.1")
	n.router("r2", "2.2.2.2")
	key := &Auth{Type: AuthCrypto, Keys: map[uint8]string{1: "secret"}, SendKey: 1}
	n.connect("l", []string{"r1", "r2"}, p2p, func(c *IfaceConfig) { c.Auth = key })
	n.stub("r2", "lo", "192.0.2.2/32")
	n.start()
	n.run(20 * time.Second)
	if n.route("r1", "192.0.2.2/32") == nil {
		t.Fatal("no route with MD5")
	}
	// A wrong key on one side: the adjacency goes down.
	n.routers["r2"].cfg.Interfaces[0].Auth = &Auth{Type: AuthCrypto, Keys: map[uint8]string{1: "other"}, SendKey: 1}
	n.routers["r2"].r.Configure(n.routers["r2"].cfg, n.now)
	n.run(60 * time.Second)
	if n.route("r1", "192.0.2.2/32") != nil || n.routers["r1"].r.Stats.AuthFailures == 0 {
		t.Fatalf("route despite the wrong key (auth failures %d)", n.routers["r1"].r.Stats.AuthFailures)
	}
}

func TestNeighborFailed(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n := newSim(t, v)
		n.router("r1", "1.1.1.1")
		n.router("r2", "2.2.2.2")
		n.connect("l12", []string{"r1", "r2"}, p2p)
		n.stub("r2", "lo", v4v6(v, "192.0.2.2/32", "2001:db8:ff::2/128"))
		n.start()
		n.run(5 * time.Second)
		r1 := n.routers["r1"].r
		nbrs := r1.TwoWayNeighbors()
		peer := netip.MustParseAddr(v4v6(v, "10.1.0.2", "fe80::1:2"))
		if len(nbrs) != 1 || nbrs[0] != (NeighborRef{Iface: "l12", ID: id("2.2.2.2"), Addr: peer}) {
			t.Fatalf("2-Way neighbours %+v", nbrs)
		}
		// The link fails; BFD notices long before the dead interval.
		n.segs["l12"].down = true
		if !r1.NeighborFailed("l12", peer, n.now) || r1.NeighborFailed("l12", peer, n.now) {
			t.Fatal("NeighborFailed must take the neighbour down once")
		}
		n.run(2 * time.Second) // the SPF delay, far below the dead interval
		if len(r1.TwoWayNeighbors()) != 0 || n.route("r1", v4v6(v, "192.0.2.2/32", "2001:db8:ff::2/128")) != nil {
			t.Fatalf("after BFD down: neighbours %v\n%s", r1.TwoWayNeighbors(), n.dump("r1"))
		}
		// The link returns: the adjacency forms again.
		n.segs["l12"].down = false
		n.run(15 * time.Second)
		if st := n.nbrStates("r1", "l12"); st[id("2.2.2.2")] != NbrFull {
			t.Fatalf("r1's neighbours after recovery %v", st)
		}
	})
}
