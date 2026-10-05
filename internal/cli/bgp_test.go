package cli

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/api/bgpapi"
	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/pkg/bgp"
)

type bgpOps struct {
	ribOps
	cleared []bgpapi.ClearRequest
	adj     []bgpapi.AdjRequest
}

var (
	nbr1 = netip.MustParseAddr("10.1.1.2")
	nbr2 = netip.MustParseAddr("10.1.1.6")
)

func (f *bgpOps) BGPStatus(*string) ([]bgpapi.InstanceStatus, error) {
	return []bgpapi.InstanceStatus{{AS: 65000, RouterID: netip.MustParseAddr("10.0.0.1"), Neighbors: []bgp.NeighborStatus{
		{Addr: nbr1, Group: "ext", PeerAS: 65001, LocalAS: 65000, State: "Established", Since: 10*time.Minute + 12*time.Second,
			RouterID: netip.MustParseAddr("10.9.9.9"), HoldTime: 90, LocalHold: 90, Local: netip.MustParseAddr("10.1.1.1"),
			Families: []bgp.Family{bgp.IPv4Unicast}, AS4: true, RouteRefresh: true, Import: []string{"in"}, Export: []string{"out"},
			Counts: []bgp.FamilyCount{{Family: bgp.IPv4Unicast, Received: 6, Accepted: 5, Active: 4, Advertised: 3}},
			Stats:  bgp.Stats{MsgsIn: 25, MsgsOut: 27}},
		{Addr: nbr2, Group: "ext", PeerAS: 65002, LocalAS: 65000, State: "Active", Since: time.Hour, LastError: "connect: refused",
			Counts: []bgp.FamilyCount{{Family: bgp.IPv4Unicast}}},
	}}}, nil
}

func (f *bgpOps) BGPAdj(q bgpapi.AdjRequest) ([]bgp.InPath, error) {
	f.adj = append(f.adj, q)
	if q.Neighbor != nbr1 {
		return nil, nil
	}
	lp := uint32(200)
	return []bgp.InPath{
		{Path: bgp.Path{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Attrs: bgp.Attrs{NextHop: nbr1, LocalPref: &lp,
			ASPath: []bgp.Segment{{ASNs: []uint32{65001}}}, Communities: []uint32{65001<<16 | 7}}}},
		{Path: bgp.Path{Prefix: netip.MustParsePrefix("198.51.100.0/24"), Attrs: bgp.Attrs{NextHop: nbr1,
			ASPath: []bgp.Segment{{ASNs: []uint32{65001, 65009}}}}}, Hidden: true},
	}, nil
}

func (f *bgpOps) ClearBGP(q bgpapi.ClearRequest) (int, error) {
	f.cleared = append(f.cleared, q)
	return 1, nil
}

func TestShowBGP(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ops := &bgpOps{}
	ts.sh.env.Ops = ops
	contains(t, ts.ok("show bgp summary"), "Local AS: 65000   Router ID: 10.0.0.1", "Groups: 1 Peers: 2 Down peers: 1",
		"inet.0                  6          4", "10.1.1.2", "65001", "0:10:12 Establ", "  inet.0: 4/6/5", "10.1.1.6", "Active")
	contains(t, ts.ok("show bgp neighbor 10.1.1.2"), "Peer: 10.1.1.2 AS 65001    Local: 10.1.1.1 AS 65000", "Group: ext",
		"Type: External    State: Established", "Export: [ out ] Import: [ in ]", "Peer ID: 10.9.9.9",
		"Peer supports 4 byte AS extension (peer-as 65001)", "Accepted prefixes:          5", "Advertised prefixes:        3")
	contains(t, ts.ok("show bgp neighbor 10.1.1.6"), "Last Error: connect: refused")
	contains(t, ts.run("show bgp neighbor 10.1.1.99"), "no BGP neighbour 10.1.1.99")
	contains(t, ts.ok("show bgp group"), "Group Type: External    Local AS: 65000", "Name: ext", "Total peers: 2    Established: 1")
	contains(t, ts.ok("show route receive-protocol bgp 10.1.1.2"), "Prefix", "Lclpref", "203.0.113.0/24          10.1.1.2",
		"200        65001 I")
	out := ts.ok("show route receive-protocol bgp 10.1.1.2 hidden")
	contains(t, out, "198.51.100.0/24", "65001 65009 I")
	if strings.Contains(out, "203.0.113.0/24") {
		t.Errorf("hidden lists an accepted route:\n%s", out)
	}
	contains(t, ts.ok("show route receive-protocol bgp 10.1.1.2 detail"), "Communities: 65001:7", "Localpref: 200")
	contains(t, ts.ok("show route hidden"), "198.51.100.0/24")
	ts.ok("show route advertising-protocol bgp 10.1.1.6")
	if q := ops.adj[len(ops.adj)-1]; !q.Out || q.Neighbor != nbr2 {
		t.Fatalf("advertising-protocol request %+v", q)
	}
	contains(t, ts.ok("clear bgp neighbor 10.1.1.2 soft"), "1 neighbours refreshed")
	contains(t, ts.ok("clear bgp neighbor"), "1 sessions reset")
	if len(ops.cleared) != 2 || ops.cleared[0].Mode != "soft" || ops.cleared[0].Neighbor != nbr1 || ops.cleared[1].Neighbor.IsValid() {
		t.Fatalf("cleared %+v", ops.cleared)
	}
}
