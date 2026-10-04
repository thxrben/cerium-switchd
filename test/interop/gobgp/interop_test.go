// Package gobgp tests cerOS's BGP speaker (pkg/bgp) against an independent
// implementation, the GoBGP server, in one process over loopback TCP. It
// is a module of its own so that GoBGP's server (gRPC, protobuf) never
// becomes a dependency of cerOS itself.
package gobgp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/apiutil"
	gobgplog "github.com/osrg/gobgp/v3/pkg/log"
	pb "github.com/osrg/gobgp/v3/pkg/packet/bgp"
	"github.com/osrg/gobgp/v3/pkg/server"

	"github.com/thxrben/cerium-switchd/pkg/bgp"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const (
	ourAS   = 65001
	theirAS = 4200000002 // 4-byte
)

var (
	ours   = netip.MustParseAddr("127.0.0.101")
	theirs = netip.MustParseAddr("127.0.0.102")
)

// dialer reaches GoBGP's port from our address.
type dialer struct{ port int }

func (d dialer) Dial(ctx context.Context, n bgp.Neighbor) (net.Conn, error) {
	nd := net.Dialer{LocalAddr: &net.TCPAddr{IP: ours.AsSlice()}}
	return nd.DialContext(ctx, "tcp", netip.AddrPortFrom(n.Addr, uint16(d.port)).String())
}

type routes struct {
	mu sync.Mutex
	rs []bgp.Route
}

func (r *routes) set(rs []bgp.Route, _ bool) { r.mu.Lock(); r.rs = rs; r.mu.Unlock() }

func (r *routes) get(prefix string) *bgp.Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.rs {
		if r.rs[i].Prefix == netip.MustParsePrefix(prefix) {
			return &r.rs[i]
		}
	}
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !cond(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
	}
}

func freePort(t *testing.T, a netip.Addr) int {
	l, err := net.Listen("tcp", netip.AddrPortFrom(a, 0).String())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func family(afi api.Family_Afi) *api.Family {
	return &api.Family{Afi: afi, Safi: api.Family_SAFI_UNICAST}
}

// gobgpPath builds a GoBGP path.
func gobgpPath(t *testing.T, prefix string, nh string, withdraw bool, extra ...pb.PathAttributeInterface) *api.Path {
	p := netip.MustParsePrefix(prefix)
	attrs := []pb.PathAttributeInterface{pb.NewPathAttributeOrigin(0)}
	var nlri pb.AddrPrefixInterface
	if p.Addr().Is4() {
		nlri = pb.NewIPAddrPrefix(uint8(p.Bits()), p.Addr().String())
		attrs = append(attrs, pb.NewPathAttributeNextHop(nh))
	} else {
		nlri = pb.NewIPv6AddrPrefix(uint8(p.Bits()), p.Addr().String())
		attrs = append(attrs, pb.NewPathAttributeMpReachNLRI(nh, []pb.AddrPrefixInterface{nlri}))
	}
	attrs = append(attrs, extra...)
	ap, err := apiutil.NewPath(nlri, withdraw, attrs, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ap
}

// received lists GoBGP's paths of a family: prefix -> attributes.
func received(t *testing.T, s *server.BgpServer, afi api.Family_Afi) map[string][]pb.PathAttributeInterface {
	out := map[string][]pb.PathAttributeInterface{}
	err := s.ListPath(context.Background(), &api.ListPathRequest{TableType: api.TableType_GLOBAL, Family: family(afi)}, func(d *api.Destination) {
		for _, p := range d.Paths {
			if p.IsWithdraw {
				continue
			}
			as, err := apiutil.GetNativePathAttributes(p)
			if err == nil && p.NeighborIp == ours.String() {
				out[d.Prefix] = as
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func attr[T pb.PathAttributeInterface](as []pb.PathAttributeInterface) (T, bool) {
	for _, a := range as {
		if v, ok := a.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

func TestInteropGoBGP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	theirPort, ourPort := freePort(t, theirs), freePort(t, ours)

	// GoBGP: AS 4200000002, IPv4 and IPv6 unicast with us, no gRPC.
	gs := server.NewBgpServer(server.LoggerOption(quietLogger{}))
	go gs.Serve()
	defer gs.Stop()
	if err := gs.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: theirAS, RouterId: theirs.String(),
		ListenPort: int32(theirPort), ListenAddresses: []string{theirs.String()}}}); err != nil {
		t.Fatal(err)
	}
	err := gs.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: ours.String(), PeerAsn: ourAS},
		Transport: &api.Transport{RemotePort: uint32(ourPort), LocalAddress: theirs.String()},
		Timers:    &api.Timers{Config: &api.TimersConfig{HoldTime: 9, KeepaliveInterval: 3, ConnectRetry: 1}},
		AfiSafis: []*api.AfiSafi{
			{Config: &api.AfiSafiConfig{Family: family(api.Family_AFI_IP), Enabled: true}},
			{Config: &api.AfiSafiConfig{Family: family(api.Family_AFI_IP6), Enabled: true}},
		}}})
	if err != nil {
		t.Fatal(err)
	}

	// Our speaker.
	rs := &routes{}
	sp := bgp.New(dialer{theirPort}, bgp.Policy{Export: func(*bgp.Neighbor, *bgp.Path) bool { return true }}, quiet)
	sp.OnRoutes = rs.set
	sp.RoutesDelay = 10 * time.Millisecond
	go sp.Run(ctx)
	l, err := net.Listen("tcp", netip.AddrPortFrom(ours, uint16(ourPort)).String())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			sp.Accept(c)
		}
	}()
	sp.Configure(bgp.Config{AS: ourAS, RouterID: ours, ConnectRetry: 500 * time.Millisecond, Neighbors: []bgp.Neighbor{{
		Addr: theirs, PeerAS: theirAS, LocalAS: ourAS, HoldTime: 9, Families: []bgp.Family{bgp.IPv4Unicast, bgp.IPv6Unicast},
		NextHop6: netip.MustParseAddr("2001:db8::101"), GracefulRestart: true, RestartTime: 120, StaleTime: 300}}})
	status := func() bgp.NeighborStatus { return sp.Status()[0] }
	waitFor(t, "session with GoBGP", func() bool { return status().State == "Established" })
	if st := status(); !st.AS4 || !st.RouteRefresh || len(st.Families) != 2 || st.HoldTime != 9 {
		t.Fatalf("negotiated: %+v", st)
	}

	// GoBGP announces: IPv4 with MED, communities and a large community;
	// IPv6 with a global next hop.
	if _, err := gs.AddPath(ctx, &api.AddPathRequest{Path: gobgpPath(t, "198.51.100.0/24", theirs.String(), false,
		pb.NewPathAttributeMultiExitDisc(42), pb.NewPathAttributeCommunities([]uint32{65002<<16 | 7}),
		pb.NewPathAttributeLargeCommunities([]*pb.LargeCommunity{{ASN: theirAS, LocalData1: 1, LocalData2: 2}}))}); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.AddPath(ctx, &api.AddPathRequest{Path: gobgpPath(t, "2001:db8:42::/48", "2001:db8::102", false)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "GoBGP's routes", func() bool { return rs.get("198.51.100.0/24") != nil && rs.get("2001:db8:42::/48") != nil })
	r4, r6 := rs.get("198.51.100.0/24"), rs.get("2001:db8:42::/48")
	if r4.PathString() != fmt.Sprintf("%d I", theirAS) || r4.NextHop != theirs || r4.MED == nil || *r4.MED != 42 ||
		!slices.Equal(r4.Communities, []uint32{65002<<16 | 7}) || len(r4.Large) != 1 || r4.Large[0] != [3]uint32{theirAS, 1, 2} {
		t.Fatalf("IPv4 route from GoBGP: %+v (%s)", r4, r4.PathString())
	}
	if r6.NextHop != netip.MustParseAddr("2001:db8::102") {
		t.Fatalf("IPv6 route from GoBGP: %+v", r6)
	}

	// We announce an IPv4 and an IPv6 route.
	sp.SetLocal([]bgp.Path{
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Source: "static", Attrs: bgp.Attrs{Communities: []uint32{65001<<16 | 9}}},
		{Prefix: netip.MustParsePrefix("2001:db8:1::/48"), Source: "static"},
	}, nil)
	waitFor(t, "our routes at GoBGP", func() bool {
		return received(t, gs, api.Family_AFI_IP)["203.0.113.0/24"] != nil && received(t, gs, api.Family_AFI_IP6)["2001:db8:1::/48"] != nil
	})
	as4 := received(t, gs, api.Family_AFI_IP)["203.0.113.0/24"]
	if p, _ := attr[*pb.PathAttributeAsPath](as4); p == nil || len(p.Value) != 1 || !slices.Equal(asns(p.Value[0]), []uint32{ourAS}) {
		t.Fatalf("AS path at GoBGP: %v", as4)
	}
	if nh, _ := attr[*pb.PathAttributeNextHop](as4); nh == nil || nh.Value.String() != ours.String() {
		t.Fatalf("next hop at GoBGP: %v", as4)
	}
	if c, _ := attr[*pb.PathAttributeCommunities](as4); c == nil || !slices.Equal(c.Value, []uint32{65001<<16 | 9}) {
		t.Fatalf("communities at GoBGP: %v", as4)
	}
	as6 := received(t, gs, api.Family_AFI_IP6)["2001:db8:1::/48"]
	if mp, _ := attr[*pb.PathAttributeMpReachNLRI](as6); mp == nil || mp.Nexthop.String() != "2001:db8::101" {
		t.Fatalf("IPv6 next hop at GoBGP: %v", as6)
	}

	// Withdrawals both ways.
	if _, err := gs.AddPath(ctx, &api.AddPathRequest{Path: gobgpPath(t, "198.51.100.0/24", theirs.String(), true)}); err != nil {
		t.Fatal(err)
	}
	sp.SetLocal([]bgp.Path{{Prefix: netip.MustParsePrefix("2001:db8:1::/48"), Source: "static"}}, nil)
	waitFor(t, "withdrawn both ways", func() bool {
		return rs.get("198.51.100.0/24") == nil && received(t, gs, api.Family_AFI_IP)["203.0.113.0/24"] == nil
	})

	// Route refresh: we ask GoBGP for its routes again; the session stays.
	sp.Clear(theirs, bgp.ClearSoft)
	time.Sleep(time.Second)
	if st := status(); st.State != "Established" || st.Stats.Flaps != 0 || rs.get("2001:db8:42::/48") == nil {
		t.Fatalf("after route refresh: %+v", st)
	}
}

func asns(s pb.AsPathParamInterface) []uint32 {
	switch v := s.(type) {
	case *pb.As4PathParam:
		return v.AS
	case *pb.AsPathParam:
		out := make([]uint32, len(v.AS))
		for i, a := range v.AS {
			out[i] = uint32(a)
		}
		return out
	}
	return nil
}

// quietLogger silences GoBGP.
type quietLogger struct{}

func (quietLogger) Panic(string, gobgplog.Fields) {}
func (quietLogger) Fatal(string, gobgplog.Fields) {}
func (quietLogger) Error(string, gobgplog.Fields) {}
func (quietLogger) Warn(string, gobgplog.Fields)  {}
func (quietLogger) Info(string, gobgplog.Fields)  {}
func (quietLogger) Debug(string, gobgplog.Fields) {}
func (quietLogger) SetLevel(gobgplog.LogLevel)    {}
func (quietLogger) GetLevel() gobgplog.LogLevel   { return gobgplog.PanicLevel }
