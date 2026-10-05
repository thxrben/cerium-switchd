package cli

import (
	"net/netip"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/lib/ospf"
)

type ospfOps struct {
	fakeOps
	cleared []string
}

func (f *ospfOps) OSPFStatus(v ospf.Version, instance *string, detail bool) ([]OSPFInstance, error) {
	if v == ospf.V3 {
		return nil, nil
	}
	rid := ospf.ID(0x01010101)
	return []OSPFInstance{{Version: v, Status: ospf.Status{
		Overview:   ospf.Overview{Version: v, RouterID: rid, Areas: []ospf.ID{0}, SPFRuns: 3, LSAs: 4, Uptime: time.Hour},
		Interfaces: []ospf.IfaceStatus{{Name: "irb.10", Area: 0, State: "DR", DRs: "10.0.0.1/10.0.0.2", Neighbors: 1, Adjacent: 1, Cost: 10}},
		Neighbors: []ospf.NeighborStatus{{ID: 0x02020202, Addr: netip.MustParseAddr("10.0.0.2"), Iface: "irb.10", State: "Full",
			Priority: 128, DeadIn: 33 * time.Second}},
		Database: []ospf.LSAStatus{{Scope: "area", Type: ospf.V2Router, TypeName: "Router", ID: rid, AdvRtr: rid, Seq: 0x80000003,
			Age: 12, Checksum: 0xabcd, Length: 48, Self: true}},
		Routes: []ospf.Route{{Prefix: netip.MustParsePrefix("192.0.2.2/32"), Type: ospf.IntraArea, Cost: 11,
			NextHops: []ospf.NextHop{{Iface: "irb.10", Gateway: netip.MustParseAddr("10.0.0.2")}}}},
	}}}, nil
}

func (f *ospfOps) ClearOSPF(v ospf.Version, instance string, nbr netip.Addr) (int, error) {
	f.cleared = append(f.cleared, nbr.String())
	return 1, nil
}

func TestShowOSPF(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ops := &ospfOps{}
	ts.sh.env.Ops = ops
	contains(t, ts.ok("show ospf neighbor"), "10.0.0.2", "irb.10", "Full", "2.2.2.2", "128  33")
	contains(t, ts.ok("show ospf interface"), "irb.10", "DR", "10.0.0.1/10.0.0.2")
	contains(t, ts.ok("show ospf database"), "OSPF database, Area 0.0.0.0", "*Router", "0x80000003")
	contains(t, ts.ok("show ospf route"), "192.0.2.2/32", "Intra", "10.0.0.2 via irb.10")
	contains(t, ts.ok("show ospf overview"), "Router ID: 1.1.1.1", "SPF: 3 runs")
	contains(t, ts.ok("show ospf3 neighbor"), "OSPFv3 is not running here")
	contains(t, ts.ok("clear ospf neighbor 10.0.0.2"), "1 adjacencies restarted")
	if len(ops.cleared) != 1 || ops.cleared[0] != "10.0.0.2" {
		t.Fatalf("cleared %v", ops.cleared)
	}
}
