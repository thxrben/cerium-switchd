package policy

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/thxrben/cerium-switchd/lib/conf/config"
	"github.com/thxrben/cerium-switchd/lib/conf/model"
)

func engine(t *testing.T, conf string) *Engine {
	t.Helper()
	tr, err := config.ParseSet(conf)
	if err != nil {
		t.Fatal(err)
	}
	c, is := model.Build(tr, nil)
	if is.HasErrors() {
		t.Fatalf("errors:\n%s", is)
	}
	return New(c.Policies)
}

func rt(p string) *Route {
	return &Route{Prefix: netip.MustParsePrefix(p), Protocol: "bgp", Preference: -1}
}

func TestRouteFilters(t *testing.T) {
	f := func(p, m string, lo, hi int) model.RouteFilter {
		return model.RouteFilter{Prefix: netip.MustParsePrefix(p), Match: m, Lo: lo, Hi: hi}
	}
	for _, tc := range []struct {
		rf   model.RouteFilter
		p    string
		want bool
	}{
		{f("10.0.0.0/8", "exact", 0, 0), "10.0.0.0/8", true},
		{f("10.0.0.0/8", "exact", 0, 0), "10.1.0.0/16", false},
		{f("10.0.0.0/8", "orlonger", 0, 0), "10.1.0.0/16", true},
		{f("10.0.0.0/8", "orlonger", 0, 0), "10.0.0.0/8", true},
		{f("10.0.0.0/8", "orlonger", 0, 0), "11.0.0.0/8", false},
		{f("10.0.0.0/8", "orlonger", 0, 0), "0.0.0.0/0", false},
		{f("10.0.0.0/8", "longer", 0, 0), "10.0.0.0/8", false},
		{f("10.0.0.0/8", "longer", 0, 0), "10.0.0.0/9", true},
		{f("10.0.0.0/8", "range", 8, 24), "10.1.1.0/24", true},
		{f("10.0.0.0/8", "range", 8, 24), "10.1.1.0/25", false},
		{f("10.0.0.0/8", "range", 16, 24), "10.0.0.0/8", false},
		{f("2001:db8::/32", "orlonger", 0, 0), "10.0.0.0/8", false},
		{f("2001:db8::/32", "orlonger", 0, 0), "2001:db8:1::/48", true},
	} {
		if got := RouteFilterMatch(tc.rf, netip.MustParsePrefix(tc.p)); got != tc.want {
			t.Errorf("%+v on %s: %v", tc.rf, tc.p, got)
		}
	}
}

const policies = `
set policy-options prefix-list rfc1918 prefix [ 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 ]
set policy-options community cust members 65000:100
set policy-options community both members [ 65000:100 65000:200 ]
set policy-options community re members "^65000:1.."
set policy-options community noexp members no-export
set policy-options as-path via65001 path "^65001 .*"
set policy-options policy-statement in term martians from prefix-list-filter rfc1918 match orlonger
set policy-options policy-statement in term martians then reject
set policy-options policy-statement in term cust from community cust
set policy-options policy-statement in term cust then local-preference 300
set policy-options policy-statement in term cust then accept
set policy-options policy-statement in term peer from as-path via65001
set policy-options policy-statement in term peer then community add noexp
set policy-options policy-statement in term peer then local-preference 50
set policy-options policy-statement out term static from protocol static
set policy-options policy-statement out term static from route-filter 198.51.100.0/24 orlonger
set policy-options policy-statement out term static then metric 10
set policy-options policy-statement out term static then as-path-prepend "65000 65000"
set policy-options policy-statement out term static then community set cust
set policy-options policy-statement out term static then accept
set policy-options policy-statement out then reject
set policy-options policy-statement mark term a then tag 7
set policy-options policy-statement mark term a then next policy
set policy-options policy-statement strip term a from community re
set policy-options policy-statement strip term a then community delete re
set policy-options policy-statement strip term a then accept
set interfaces 1/0/5 unit 0 family inet address 10.1.1.1/30
set routing-options autonomous-system 65000
set protocols bgp group g type external
set protocols bgp group g peer-as 65001
set protocols bgp group g neighbor 10.1.1.2 import [ in strip ]
set protocols bgp group g neighbor 10.1.1.2 export [ mark out ]
`

func TestEvaluate(t *testing.T) {
	e := engine(t, policies)
	// A private prefix is rejected by the first term.
	r := rt("10.20.0.0/16")
	if res := e.Evaluate([]string{"in"}, r); res != Reject {
		t.Fatalf("martian: %v", res)
	}
	// Customer community: accepted with local preference.
	r = rt("203.0.113.0/24")
	r.Communities = []string{"65000:100"}
	if res := e.Evaluate([]string{"in"}, r); res != Accept || r.LocalPref != 300 {
		t.Fatalf("customer: %v %+v", res, r)
	}
	// Through AS 65001: the term has no terminating action; the chain
	// continues to "strip", which needs community re.
	r = rt("203.0.113.0/24")
	r.ASPath = []uint32{65001, 3}
	if res := e.Evaluate([]string{"in", "strip"}, r); res != Default || r.LocalPref != 50 || !slices.Contains(r.Communities, "no-export") {
		t.Fatalf("peer: %v %+v", res, r)
	}
	// Community expression: delete 65000:1xx, keep others.
	r = rt("203.0.113.0/24")
	r.Communities = []string{"65000:150", "65000:2", "65000:199"}
	if res := e.Evaluate([]string{"strip"}, r); res != Accept || !slices.Equal(r.Communities, []string{"65000:2"}) {
		t.Fatalf("strip: %v %+v", res, r.Communities)
	}
	// Export: next policy carries the tag, then out decides.
	r = rt("198.51.100.0/25")
	r.Protocol = "static"
	r.Communities = []string{"1:1"}
	if res := e.Evaluate([]string{"mark", "out"}, r); res != Accept || r.Tag != 7 || r.Metric != 10 || !r.HasMetric ||
		!slices.Equal(r.ASPath, []uint32{65000, 65000}) || !slices.Equal(r.Communities, []string{"65000:100"}) {
		t.Fatalf("export: %v %+v", res, r)
	}
	// Not static: the policy's final then rejects.
	r = rt("198.51.100.0/25")
	r.Protocol = "ospf"
	if res := e.Evaluate([]string{"out"}, r); res != Reject {
		t.Fatalf("final reject: %v", res)
	}
	// Unknown policy names are skipped; nothing matched: Default.
	if res := e.Evaluate([]string{"nope"}, rt("1.0.0.0/8")); res != Default {
		t.Fatal(res)
	}
}

func TestCommunityAllMembers(t *testing.T) {
	e := engine(t, policies+`
set policy-options policy-statement b term a from community both
set policy-options policy-statement b term a then accept
set protocols bgp group g neighbor 10.1.1.2 import b
`)
	r := rt("1.0.0.0/8")
	r.Communities = []string{"65000:100"}
	if e.Evaluate([]string{"b"}, r) == Accept {
		t.Fatal("matched with only one of two members")
	}
	r.Communities = append(r.Communities, "65000:200")
	if e.Evaluate([]string{"b"}, r) != Accept {
		t.Fatal("both members present but no match")
	}
}

func TestCommunityCodec(t *testing.T) {
	for _, s := range []string{"65000:100", "0:0", "65535:65535", "no-export", "no-advertise", "no-export-subconfed"} {
		v, ok := ParseCommunity(s)
		if !ok || FormatCommunity(v) != s {
			t.Errorf("%s -> %d %v -> %s", s, v, ok, FormatCommunity(v))
		}
	}
	for _, s := range []string{"65536:1", "large:1:2:3", "^65000:.*", "x"} {
		if _, ok := ParseCommunity(s); ok {
			t.Errorf("%s parsed", s)
		}
	}
}
