package model

import (
	"net/netip"
	"strings"
	"testing"
)

const routingBase = `
set vlans core vlan-id 10
set vlans core l3-interface irb.10
set interfaces irb unit 10 family inet address 10.0.10.1/24
set interfaces irb unit 10 family inet6 address 2001:db8:10::1/64
set interfaces 1/0/5 unit 0 family inet address 10.1.1.1/30
set interfaces 1/0/6 unit 0 family inet address 10.2.2.1/30
set routing-options autonomous-system 65000
`

func TestRoutingBuild(t *testing.T) {
	c, is := build(t, routingBase+`
set routing-options router-id 10.255.0.1
set protocols ospf area 0 interface irb.10 passive
set protocols ospf area 0 interface 1/0/5.0 interface-type p2p
set protocols ospf area 0 interface 1/0/5.0 authentication md5 1 key secret
set protocols ospf area 0 interface 1/0/5.0 bfd-liveness-detection minimum-interval 100
set protocols ospf area 1 interface 1/0/6.0 metric 20
set protocols ospf export to-ospf
set protocols ospf3 area 0.0.0.0 interface irb.10
set protocols bgp group up type external
set protocols bgp group up peer-as 65001
set protocols bgp group up export to-bgp
set protocols bgp group up neighbor 10.1.1.2
set protocols bgp group up neighbor 10.2.2.2 peer-as 65002
set protocols bgp group up neighbor 10.2.2.2 import from-b
set protocols bgp group ibgp type internal
set protocols bgp group ibgp cluster 10.255.0.1
set protocols bgp group ibgp neighbor 10.0.10.2
set policy-options prefix-list mine prefix [ 10.0.0.0/8 ]
set policy-options community c1 members 65000:100
set policy-options as-path from65001 path "^65001 .*"
set policy-options policy-statement to-ospf term s from protocol static
set policy-options policy-statement to-ospf term s then accept
set policy-options policy-statement to-bgp term a from prefix-list mine
set policy-options policy-statement to-bgp term a then community add c1
set policy-options policy-statement to-bgp term a then accept
set policy-options policy-statement from-b term a from as-path from65001
set policy-options policy-statement from-b term a from route-filter 192.168.0.0/16 upto /24
set policy-options policy-statement from-b term a then local-preference 200
set policy-options policy-statement from-b term a then accept
set policy-options policy-statement from-b then reject
`, nil)
	if is.HasErrors() {
		t.Fatalf("errors:\n%s", is)
	}
	r := c.Routing
	if r == nil || r.RouterID != netip.MustParseAddr("10.255.0.1") || r.AS != 65000 {
		t.Fatalf("routing %+v", r)
	}
	o := r.OSPF
	if o == nil || !o.ABR || len(o.Areas) != 2 || o.ReferenceBW != 100e9 || !o.GracefulRestart {
		t.Fatalf("ospf %+v", o)
	}
	ifc := o.Areas[netip.MustParseAddr("0.0.0.0")].Interfaces["1/0/5.0"]
	if !ifc.P2P || ifc.MD5[1] != "secret" || ifc.BFD == nil || ifc.BFD.IntervalMs != 100 || ifc.BFD.Multiplier != 3 || ifc.Dead != 40 {
		t.Fatalf("ospf interface %+v", ifc)
	}
	if o.Areas[netip.MustParseAddr("0.0.0.1")].Interfaces["1/0/6.0"].Metric != 20 {
		t.Fatal("metric")
	}
	if r.OSPF3 == nil || len(r.OSPF3.Areas) != 1 {
		t.Fatalf("ospf3 %+v", r.OSPF3)
	}
	up := r.BGP.Groups["up"]
	a := up.Neighbors[netip.MustParseAddr("10.1.1.2")]
	bn := up.Neighbors[netip.MustParseAddr("10.2.2.2")]
	if a.PeerAS != 65001 || a.LocalAS != 65000 || strings.Join(a.Export, ",") != "to-bgp" || !a.IPv4 || a.IPv6 {
		t.Fatalf("neighbor a %+v", a)
	}
	if bn.PeerAS != 65002 || strings.Join(bn.Import, ",") != "from-b" || strings.Join(bn.Export, ",") != "to-bgp" {
		t.Fatalf("neighbor b %+v", bn)
	}
	ib := r.BGP.Groups["ibgp"].Neighbors[netip.MustParseAddr("10.0.10.2")]
	if !ib.Internal || ib.PeerAS != 65000 || !ib.RouteReflector {
		t.Fatalf("ibgp %+v", ib)
	}
	ps := c.Policies.Statements["from-b"]
	if ps.Final != "reject" || len(ps.Terms) != 1 {
		t.Fatalf("policy %+v", ps)
	}
	rf := ps.Terms[0].From.RouteFilters[0]
	if rf.Match != "range" || rf.Lo != 16 || rf.Hi != 24 || *ps.Terms[0].Then.LocalPref != 200 {
		t.Fatalf("route filter %+v", rf)
	}
}

func TestRoutingErrors(t *testing.T) {
	for _, tc := range []struct{ conf, want string }{
		{`set protocols ospf area 1 interface 1/0/5.0
set protocols ospf area 2 interface 1/0/6.0`, "needs an interface in area 0"},
		{`set protocols ospf area 0 interface 1/0/9.0`, "not a routed interface"},
		{`set protocols ospf3 area 0 interface 1/0/5.0`, "no IPv6 address"},
		{`set protocols ospf area 0 interface 1/0/5.0
set protocols ospf area 1 interface 1/0/5.0`, "already in area"},
		{`set protocols ospf area 0 interface 1/0/5.0 hello-interval 30
set protocols ospf area 0 interface 1/0/5.0 dead-interval 20`, "larger than the hello interval"},
		{`set protocols ospf area 0 interface 1/0/5.0
set protocols ospf export nope`, "policy nope is not defined"},
		{`set protocols bgp group g peer-as 65001
set protocols bgp group g neighbor 10.1.1.2`, "needs 'type internal'"},
		{`set protocols bgp group g type external
set protocols bgp group g neighbor 10.1.1.2`, "needs peer-as"},
		{`set protocols bgp group g type external
set protocols bgp group g peer-as 65000
set protocols bgp group g neighbor 10.1.1.2`, "is the own AS"},
		{`set protocols bgp group g type internal
set protocols bgp group g peer-as 65009
set protocols bgp group g neighbor 10.1.1.2`, "in the own AS"},
		{`set protocols bgp group g type external
set protocols bgp group g peer-as 65001
set protocols bgp group g neighbor 10.1.1.2
set protocols bgp group h type external
set protocols bgp group h peer-as 65001
set protocols bgp group h neighbor 10.1.1.2`, "already in group g"},
		{`set protocols bgp group g type external
set protocols bgp group g cluster 1.1.1.1
set protocols bgp group g peer-as 65001
set protocols bgp group g neighbor 10.1.1.2`, "route reflection is for internal groups"},
		{`set policy-options policy-statement p term a from prefix-list none
set policy-options policy-statement p term a then accept
set protocols bgp group g type external
set protocols bgp group g peer-as 65001
set protocols bgp group g neighbor 10.1.1.2 export p`, "prefix list none is not defined"},
		{`set policy-options as-path bad path "^65000 [1-x]"`, "invalid AS"},
		{`set policy-options policy-statement p term a from route-filter 10.0.0.0/16 upto /8`, "prefix lengths"},
	} {
		_, is := build(t, routingBase+tc.conf, nil)
		if !strings.Contains(is.String(), tc.want) {
			t.Errorf("%q: want %q in:\n%s", tc.conf, tc.want, is)
		}
	}
	// No AS at all.
	_, is := build(t, `set interfaces 1/0/5 unit 0 family inet address 10.1.1.1/30
set protocols bgp group g type external
set protocols bgp group g peer-as 65001
set protocols bgp group g neighbor 10.1.1.2`, nil)
	if !strings.Contains(is.String(), "no autonomous system") {
		t.Errorf("missing AS not reported:\n%s", is)
	}
}

func TestRoutingWarnings(t *testing.T) {
	_, is := build(t, routingBase+`
set policy-options policy-statement unused term a then accept
set policy-options policy-statement p term a then accept
set policy-options policy-statement p term b from protocol static
set policy-options policy-statement p term b then reject
set protocols ospf area 0 interface 1/0/5.0
set protocols ospf export p
`, nil)
	s := is.String()
	if is.HasErrors() || !strings.Contains(s, "unused") || !strings.Contains(s, "term b") || !strings.Contains(s, "never reached") {
		t.Fatalf("warnings:\n%s", s)
	}
	// No IPv4 address: no router id.
	_, is = build(t, `set interfaces 1/0/5 unit 0 family inet6 address 2001:db8::1/64
set protocols ospf3 area 0 interface 1/0/5.0`, nil)
	if !strings.Contains(is.String(), "no router id") {
		t.Fatalf("router id warning missing:\n%s", is)
	}
}

func TestRoutingInInstances(t *testing.T) {
	c, is := build(t, routingBase+`
set interfaces 1/0/7 unit 0 family inet address 10.9.9.1/30
set routing-instances red interface 1/0/7.0
set routing-instances red routing-options autonomous-system 65100
set routing-instances red protocols bgp group g type external
set routing-instances red protocols bgp group g peer-as 65200
set routing-instances red protocols bgp group g neighbor 10.9.9.2
set routing-instances red protocols ospf area 0 interface 1/0/7.0
`, nil)
	if is.HasErrors() {
		t.Fatalf("errors:\n%s", is)
	}
	r := c.Instances["red"].Routing
	if r == nil || r.AS != 65100 || c.RouterID(r) != netip.MustParseAddr("10.9.9.1") {
		t.Fatalf("instance routing %+v", r)
	}
	// An interface of the default instance in an instance's OSPF.
	_, is = build(t, routingBase+`
set interfaces 1/0/7 unit 0 family inet address 10.9.9.1/30
set routing-instances red interface 1/0/7.0
set routing-instances red protocols ospf area 0 interface 1/0/5.0
`, nil)
	if !strings.Contains(is.String(), "is in the default instance") {
		t.Fatalf("cross-instance interface:\n%s", is)
	}
}

func TestASPathExpressions(t *testing.T) {
	for _, tc := range []struct {
		expr string
		path []uint32
		want bool
	}{
		{"^65001 .*", []uint32{65001, 3, 4}, true},
		{"^65001 .*", []uint32{3, 65001}, false},
		{"^$", nil, true},
		{"^$", []uint32{1}, false},
		{"65001", []uint32{1, 65001, 2}, true},
		{"65001", []uint32{1, 650011}, false},
		{".* 65001$", []uint32{1, 2, 65001}, true},
		{"^65001 65002$", []uint32{65001, 65002}, true},
		{"^65001 65002$", []uint32{65001, 65002, 3}, false},
		{"^[65000-65010]", []uint32{65005}, true},
		{"^[65000-65010]", []uint32{65011}, false},
		{"^65001+ 2$", []uint32{65001, 65001, 2}, true},
		{"^. .$", []uint32{1, 2}, true},
		{"^. .$", []uint32{1, 2, 3}, false},
		{"^(1|2) 3", []uint32{2, 3}, true},
		{"^.{2}$", []uint32{7, 8}, true},
	} {
		re, err := CompileASPath(tc.expr)
		if err != nil {
			t.Fatalf("%q: %v", tc.expr, err)
		}
		if got := MatchASPath(re, tc.path); got != tc.want {
			t.Errorf("%q on %v: %v, want %v (regexp %s)", tc.expr, tc.path, got, tc.want, re)
		}
	}
	for _, bad := range []string{"65001 ^", "abc", "[1-x]", "$ 1"} {
		if _, err := CompileASPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// cpuInv is an inventory that knows only the checking member's CPU cores.
type cpuInv struct{ member, cpus int }

func (cpuInv) Ports(int) (map[string]PortInfo, bool) { return nil, false }
func (i cpuInv) CPUs() (int, int)                    { return i.member, i.cpus }

func TestBFDIntervalCheck(t *testing.T) {
	conf := routingBase + `
set protocols ospf area 0 interface 1/0/5.0 bfd-liveness-detection minimum-interval 50
set protocols ospf area 0 interface irb.10 bfd-liveness-detection minimum-interval 99
set protocols ospf area 0 interface 1/0/6.0 bfd-liveness-detection minimum-interval 100
set protocols bgp group up neighbor 10.1.1.2 peer-as 65001
set protocols bgp group up bfd-liveness-detection minimum-interval 60
`
	const msg = "BFD below 100 ms"
	paths := func(is Issues) []string {
		var out []string
		for _, i := range is {
			if strings.Contains(i.Msg, msg) {
				if i.Severity != Warning {
					t.Errorf("%s: severity %v, want a warning", i.Path, i.Severity)
				}
				out = append(out, i.Path)
			}
		}
		return out
	}
	// Member 1 with 2 cores runs all three fast sessions.
	_, is := build(t, conf, cpuInv{1, 2})
	got := strings.Join(paths(is), "\n")
	for _, want := range []string{"interface 1/0/5.0 bfd", "interface irb.10 bfd", "neighbor 10.1.1.2 bfd"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing warning for %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "1/0/6.0") {
		t.Errorf("100 ms must not warn:\n%s", got)
	}
	// Member 2 does not own 1/0/5 but may become master (irb, BGP).
	_, is = build(t, conf, cpuInv{2, 2})
	if got := paths(is); len(got) != 2 || strings.Contains(strings.Join(got, " "), "1/0/5.0") {
		t.Errorf("member 2 warnings = %v", got)
	}
	// Enough cores, or no CPU count: no warning.
	for _, inv := range []Inventory{cpuInv{1, 4}, cpuInv{1, 0}, nil} {
		if _, is := build(t, conf, inv); len(paths(is)) != 0 {
			t.Errorf("%v: unexpected warnings %v", inv, paths(is))
		}
	}
}

// Changes that need a new BGP session are named at commit check; others
// (policies, description, multipath) are not.
func TestBGPChangeWarnings(t *testing.T) {
	base := routingBase + `
set protocols bgp group ext type external
set protocols bgp group ext peer-as 65001
set protocols bgp group ext neighbor 10.1.1.2
set protocols bgp group ext neighbor 10.1.1.3 peer-as 65003
`
	old, is := build(t, base, nil)
	if is.HasErrors() {
		t.Fatalf("base:\n%s", is)
	}
	cases := []struct {
		change string
		want   string // "" no warning
	}{
		{"set protocols bgp group ext neighbor 10.1.1.2 description x", ""},
		{"set protocols bgp group ext multipath", ""},
		{"set protocols bgp group ext neighbor 10.1.1.2 hold-time 30", "neighbor 10.1.1.2: this change resets the session (hold-time)"},
		{"set protocols bgp group ext neighbor 10.1.1.2 authentication-key k\nset protocols bgp group ext neighbor 10.1.1.2 passive",
			"(authentication-key, passive)"},
		{"set protocols bgp group ext peer-as 65009", "neighbor 10.1.1.2: this change resets the session (peer-as)"},
		{"set routing-options autonomous-system 65010", "resets every BGP session"},
		{"set protocols bgp group ext neighbor 10.1.1.9", ""}, // a new neighbour
		{"set system memory allocation arp slots 4", "the memory slots change at the next 'request system reload'"},
	}
	for _, c := range cases {
		cand, is := build(t, base+c.change+"\n", nil)
		if is.HasErrors() {
			t.Fatalf("%q:\n%s", c.change, is)
		}
		got := ChangeWarnings(old, cand).String()
		switch {
		case c.want == "" && got != "":
			t.Errorf("%q: unexpected %s", c.change, got)
		case c.want != "" && !strings.Contains(got, c.want):
			t.Errorf("%q: want %q, got %q", c.change, c.want, got)
		}
		if c.change == "set protocols bgp group ext peer-as 65009" && strings.Contains(got, "10.1.1.3") {
			t.Errorf("10.1.1.3 has its own peer-as: %s", got)
		}
	}
}
