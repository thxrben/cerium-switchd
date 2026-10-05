package cli

import (
	"net/netip"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/lib/rib"
)

type ribOps struct {
	fakeOps
	queries []rib.Query
}

func (f *ribOps) RIB(q rib.Query) ([]rib.Entry, error) {
	f.queries = append(f.queries, q)
	since := time.Now().Add(-(26*time.Hour + 3*time.Minute + 4*time.Second))
	nh := func(gw, ifc string) []rib.NextHop {
		h := rib.NextHop{Interface: ifc}
		if gw != "" {
			h.Gateway = netip.MustParseAddr(gw)
		}
		return []rib.NextHop{h}
	}
	lp := uint32(100)
	t4 := rib.Table{}
	return []rib.Entry{
		{Table: t4, Prefix: netip.MustParsePrefix("0.0.0.0/0"), Active: 0, Routes: []rib.Route{
			{Protocol: rib.OSPF, Preference: 150, Metric: 0, NextHops: nh("10.1.1.2", "1/0/1.0"), Since: since,
				Attrs: &rib.Attrs{Area: "0.0.0.0", PathType: "Ext2"}},
			{Protocol: rib.BGP, Preference: 170, NextHops: nh("10.2.2.2", "irb.20"), Since: since, Source: "10.2.2.2",
				Attrs: &rib.Attrs{Peer: "10.2.2.2", PeerAS: 65001, ASPath: "65001 I", LocalPref: &lp}},
		}},
		{Table: t4, Prefix: netip.MustParsePrefix("10.1.1.0/30"), Active: 0, Routes: []rib.Route{
			{Protocol: rib.Direct, Preference: 0, NextHops: nh("", "1/0/1.0"), Since: since}}},
		{Table: t4, Prefix: netip.MustParsePrefix("10.1.1.1/32"), Active: 0, Routes: []rib.Route{
			{Protocol: rib.Local, Preference: 0, NextHops: nh("", "1/0/1.0"), Since: since}}},
		{Table: t4, Prefix: netip.MustParsePrefix("198.51.100.0/24"), Active: 0, Routes: []rib.Route{
			{Protocol: rib.Static, Preference: 5, Discard: true, Since: since}}},
	}, nil
}

func (f *ribOps) RIBSummary() ([]rib.Summary, error) {
	return []rib.Summary{{Table: rib.Table{}, Destinations: 4, Routes: 5, Active: 4,
		PerProtocol: map[rib.Protocol][2]int{rib.Direct: {1, 1}, rib.Local: {1, 1}, rib.Static: {1, 1}, rib.OSPF: {1, 1}, rib.BGP: {1, 0}}}}, nil
}

func TestShowRoute(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ops := &ribOps{}
	ts.sh.env.Ops = ops
	contains(t, ts.ok("show route"),
		"inet.0: 4 destinations, 5 routes (4 active, 0 holddown, 0 hidden)",
		"+ = Active Route, - = Last Active, * = Both",
		"0.0.0.0/0          *[OSPF/150] 1d 02:03:04, metric 0, tag 0",
		"\n                    >  to 10.1.1.2 via 1/0/1.0\n",
		" [BGP/170] 1d 02:03:04, localpref 100",
		"\n                      AS path: 65001 I\n",
		"10.1.1.0/30        *[Direct/0] 1d 02:03:04\n",
		">  via 1/0/1.0",
		"\n                       Local via 1/0/1.0\n",
		"198.51.100.0/24    *[Static/5] 1d 02:03:04\n",
		"Discard")
	contains(t, ts.ok("show route 0.0.0.0/0 detail"), "0.0.0.0/0 (2 entries, 1 announced)", "*OSPF    Preference: 150",
		"Next hop: 10.1.1.2 via 1/0/1.0, selected", "Path type: Ext2", "Inactive reason: Route Preference", "Peer AS: 65001", "Localpref: 100")
	contains(t, ts.ok("show route terse"), "A V Destination", "* ? 0.0.0.0/0          O 150", ">10.1.1.2", "65001 I")
	contains(t, ts.ok("show route summary"), "inet.0: 4 destinations", "OSPF:      1 routes,      1 active", "BGP:      1 routes,      0 active")
	// Filters reach the RIB query.
	ops.queries = nil
	ts.ok("show route 10.1.1.0/24 longer protocol direct next-hop 10.1.1.2 active-path table red.inet6.0")
	q := ops.queries[0]
	if q.Prefix != netip.MustParsePrefix("10.1.1.0/24") || q.Match != "longer" || q.Protocol == nil || *q.Protocol != rib.Direct ||
		q.NextHop != netip.MustParseAddr("10.1.1.2") || !q.Active || len(q.Tables) != 1 || q.Tables[0] != (rib.Table{Instance: "red", V6: true}) {
		t.Fatalf("query %+v", q)
	}
	ts.ok("show route instance all")
	if q := ops.queries[1]; q.Tables != nil {
		t.Fatalf("instance all: tables %v", q.Tables)
	}
	ts.ok("show route 10.1.1.1")
	if q := ops.queries[2]; q.Prefix != netip.MustParsePrefix("10.1.1.1/32") || q.Match != "" || len(q.Tables) != 2 {
		t.Fatalf("address lookup %+v", q)
	}
	contains(t, ts.run("show route protocol rip"), "unknown protocol")
	contains(t, ts.run("show route exact"), "need a prefix")
	contains(t, ts.run("show route table inet.9"), "unknown table name")
}
