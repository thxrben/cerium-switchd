package model

import (
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/config"
)

const valid = `
set system host-name core
set system login user alice class super-user
set system login user alice authentication encrypted-password "$6$abc$def"
set virtual-chassis member 1 host-name sw-a
set virtual-chassis member 2 host-name sw-b
set system management-instance oob
set interfaces 1/9/0 management
set interfaces 2/9/0 management
set interfaces cme unit 0 family inet address 192.168.1.11/24
set routing-instances oob interface cme.0
set routing-instances oob routing-options static route 0.0.0.0/0 next-hop 192.168.1.1
set switch-options vxlan source-address 10.255.0.1
set interfaces 1/0/1 ether-options 802.3ad ae1
set interfaces 1/0/2 ether-options 802.3ad ae1
set interfaces 2/0/2 ether-options 802.3ad ae1
set interfaces ae1 mtu 9000
set interfaces ae1 aggregated-ether-options lacp active
set interfaces ae1 unit 0 family ethernet-switching interface-mode trunk
set interfaces ae1 unit 0 family ethernet-switching vlan members [ users 20 ]
set interfaces ae1 native-vlan-id users
set interfaces 1/0/3 unit 0 family ethernet-switching vlan members storage
set interfaces 1/0/4 description mirror-target
set vlans users vlan-id 10
set vlans users vxlan vni 10010
set vlans storage vlan-id 20
set vlans storage mtu 9000
set protocols rstp interface ae1 edge
set forwarding-options analyzer dbg input ingress interface 1/0/3
set forwarding-options analyzer dbg output interface 1/0/4
`

func build(t *testing.T, text string, inv Inventory) (*Config, Issues) {
	t.Helper()
	tr, err := config.ParseSet(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return Build(tr, inv)
}

func TestValidConfig(t *testing.T) {
	c, issues := build(t, valid, nil)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", issues)
	}
	ae1 := c.Interfaces["ae1"]
	if ae1.Mode != "trunk" || len(ae1.VLANs) != 2 || ae1.NativeVLAN != 10 {
		t.Errorf("ae1 = %+v", ae1)
	}
	if strings.Join(ae1.MemberPorts, ",") != "1/0/1,1/0/2,2/0/2" || len(ae1.MemberIDs) != 2 {
		t.Errorf("ae1 members = %v %v", ae1.MemberPorts, ae1.MemberIDs)
	}
	if c.Interfaces["1/0/2"].MTU != 9000 {
		t.Errorf("member port must inherit ae MTU")
	}
	if c.Interfaces["1/0/3"].AccessVLAN != 20 {
		t.Errorf("access vlan = %d", c.Interfaces["1/0/3"].AccessVLAN)
	}
	if c.RSTP == nil || !c.RSTP.Ports["ae1"].Edge {
		t.Errorf("rstp not built")
	}
	if !c.System.Commit.ConfirmRequired || c.System.Commit.TimeoutMinutes != 10 {
		t.Errorf("commit policy defaults wrong: %+v", c.System.Commit)
	}
	if c.VLANs["users"].VNI != 10010 {
		t.Errorf("vni not built")
	}
}

func TestStandaloneDefaults(t *testing.T) {
	c, issues := build(t, "set interfaces 1/0/0 unit 0 family ethernet-switching vlan members v\nset vlans v vlan-id 5\n", nil)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", issues)
	}
	if _, ok := c.Members[1]; !ok {
		t.Fatal("implicit member 1 missing")
	}
	_, issues = build(t, "set interfaces 2/0/0 disable\n", nil)
	if !issues.HasErrors() {
		t.Fatal("member 2 without stack config must fail")
	}
}

func TestInvalidConfigs(t *testing.T) {
	cases := []struct {
		name   string
		mutate string // set/delete lines appended to valid
		want   string
	}{
		{"missing vlan", "set interfaces 1/0/3 unit 0 family ethernet-switching vlan members nosuch", `vlan "nosuch" is not defined`},
		{"undefined id range", "set interfaces ae1 unit 0 family ethernet-switching vlan members 30-32", "vlan-id 30-32 is not defined"},
		{"access two vlans", "set interfaces 1/0/3 unit 0 family ethernet-switching vlan members users", "exactly one VLAN"},
		{"native on access", "set interfaces 1/0/3 native-vlan-id users", "only valid in trunk mode"},
		{"dup vlan id", "set vlans dup vlan-id 10", "already used by vlan"},
		{"vlan without id", "set vlans noid description x", "vlan-id is required"},
		{"dup vni", "set vlans storage vxlan vni 10010", "vni 10010 is already used"},
		{"vtep missing", "delete switch-options vxlan source-address", "needs the stack's VTEP address"},
		{"vtep is irb address", "set switch-options vxlan source-address 192.168.1.11", "the VTEP address must be its own"},
		{"remote vni unmapped", "set switch-options vxlan remote-vtep 10.9.9.9 vni 77", "vni 77 is not mapped"},
		{"snooping unknown port", "set protocols igmp-snooping interface 1/0/9 immediate-leave", "1/0/9 is not configured"},
		{"unknown ae", "set interfaces 1/0/5 ether-options 802.3ad ae9", "ae9 is not configured"},
		{"member with family", "set interfaces 1/0/1 unit 0 family ethernet-switching", "cannot have 'unit 0 family"},
		{"ae ether-options", "set interfaces ae1 ether-options flow-control", "only valid on physical ports"},
		{"phys agg options", "set interfaces 1/0/3 aggregated-ether-options lacp active", "only valid on ae interfaces"},
		{"two peers", "set virtual-chassis member 3 host-name sw-c\nset interfaces 1/0/7 ether-options 802.3ad ae2\nset interfaces 3/0/7 ether-options 802.3ad ae2\nset interfaces ae2 aggregated-ether-options lacp", "must have the same peer"},
		{"rstp timers", "set protocols rstp max-age 40", "timers violate"},
		{"bpdu-block unknown", "set protocols layer2-control bpdu-block interface 1/0/9", "1/0/9 is not configured"},
		{"mclag without lacp", "delete interfaces ae1 aggregated-ether-options lacp", "which needs 'lacp'"},
		{"analyzer same port", "set forwarding-options analyzer dbg input ingress interface 1/0/4", "both input and output"},
		{"analyzer no output", "delete forwarding-options analyzer dbg output", "output interface is required"},
		{"analyzer cross member", "set interfaces 2/0/9 description x\nset forwarding-options analyzer dbg input egress interface 2/0/9", "ports on the output's member"},
		{"analyzer mclag output", "set forwarding-options analyzer dbg output interface ae1", "cannot be a mirror output"},
		{"rstp on member port", "set protocols rstp interface 1/0/1 edge", "configure RSTP on the aggregated interface"},
		{"mgmt is switch port", "set interfaces 1/9/0 unit 0 family ethernet-switching vlan members storage", "carries only cme"},
		{"mgmt port on ae", "set interfaces ae1 management", "must be a physical port"},
		{"cme unit 1", "set interfaces cme unit 1 family inet address 10.9.0.1/24", "cme has only unit 0"},
		{"cme dhcp", "set interfaces cme unit 0 family inet dhcp", "static addresses only"},
		{"cme member address", "set interfaces cme unit 0 family inet address 10.9.0.1/24 member 1", "irb addresses only"},
		{"cme outside mgmt", "delete routing-instances oob interface cme.0", "cme.0 must be in the management instance"},
		{"irb member in mgmt", "set vlans v99 vlan-id 99\nset vlans v99 l3-interface irb.99\nset interfaces irb unit 99 family inet address 10.8.0.1/24 member 1\nset routing-instances oob interface irb.99", "'member' is not valid"},
		{"dup hostname", "set virtual-chassis member 2 host-name sw-a", "already used by member 1"},
		{"three members", "set virtual-chassis member 3 host-name sw-c\nset interfaces 3/0/2 ether-options 802.3ad ae1", "at most two"},
		{"witness ports", "set virtual-chassis member 2 role witness", "is a witness"},
		{"mgmt instance without flag", "delete system management-instance", "cme.0 must be in the management instance"},
		{"flag without mgmt instance", "delete routing-instances", "routing instance oob is not configured"},
		{"unit in two instances", "set routing-instances data interface cme.0", "is already in routing instance"},
		{"non-routed unit in instance", "set routing-instances data interface 1/0/5.0", "is not a routed interface"},
		{"member on port address", "set interfaces 1/0/5 unit 0 family inet address 10.8.0.1/24 member 1", "irb addresses only"},
		{"member not configured", "set vlans v99 vlan-id 99\nset vlans v99 l3-interface irb.99\nset interfaces irb unit 99 family inet address 10.8.0.1/24 member 7", "member 7 is not configured"},
		{"range overlap", "set interface-range a member-range 1/0/10 to 1/0/12\nset interface-range b member-range 1/0/12 to 1/0/13", "already part of interface-range a"},
		{"range bad ends", "set interface-range a member-range 1/0/10 to 2/0/12", "same member"},
		{"range reversed", "set interface-range a member-range 1/0/12 to 1/0/10", "comes before"},
		{"range across cards", "set interface-range a member-range 1/0/1 to 1/1/2", "same member and card"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, issues := build(t, valid+c.mutate+"\n", nil)
			if !issues.HasErrors() || !strings.Contains(issues.String(), c.want) {
				t.Errorf("want error containing %q, got:\n%s", c.want, issues)
			}
		})
	}
}

func TestWarnings(t *testing.T) {
	cases := []struct {
		mutate string
		want   string
	}{
		{"set interfaces 1/0/3 mtu 1500\nset vlans storage mtu 9000", "larger frames are dropped"},
		{"set interfaces ae1 aggregated-ether-options minimum-links 4", "can never come up"},
		{"set system login user bob class operator", "cannot log in"},
		{"set protocols layer2-control bpdu-block interface 1/0/3\nset protocols rstp interface 1/0/3", "runs RSTP as a non-edge port"},
		{"set system services web-management port 8443", "web-management is not implemented yet"},
		{"set switch-options vxlan remote-vtep 10.9.9.9", "no vni listed"},
		{"set interfaces 1/0/8", "1/0/8 is a plain port"},
		{"set interfaces 1/0/5 unit 0 family inet address 10.7.0.1/24", "may carry VXLAN to remote VTEPs"},
		{"set system name-server [ 1.1.1.1 1.0.0.1 8.8.8.8 9.9.9.9 ]", "only the first 3"},
		{"set routing-instances oob routing-options static route ::/0 next-hop 2001:db8::1", "not in a subnet of any routed interface of this instance"},
		{"delete interfaces 1/9/0\ndelete interfaces 2/9/0", "no member has a management port"},
		{"set protocols layer2-control bpdu-block interface ae1\nset protocols rstp interface ae1 cost 10\ndelete protocols rstp interface ae1 edge", "non-edge port"},
		{"set interfaces 1/0/4 unit 0 family ethernet-switching vlan members storage", "carries switched traffic"},
	}
	for _, c := range cases {
		_, issues := build(t, valid+c.mutate+"\n", nil)
		if issues.HasErrors() {
			t.Errorf("%q: unexpected error:\n%s", c.mutate, issues)
		}
		if !strings.Contains(issues.String(), c.want) {
			t.Errorf("%q: want warning %q, got:\n%s", c.mutate, c.want, issues)
		}
	}
}

// fakeInv describes member 1's ports: interface name -> max Linux MTU. A
// negative value marks a stacking port (-1: NIC maximum 16044, -3: 9202,
// i.e. frames of 9216).
type fakeInv map[string]int

func (f fakeInv) Ports(member int) (map[string]PortInfo, bool) {
	if member != 1 {
		return nil, false
	}
	out := map[string]PortInfo{}
	for name, mtu := range f {
		linux := "lx" + strings.ReplaceAll(name, "/", "")
		if mtu == -3 {
			out[name] = PortInfo{Linux: linux, MTU: 9202, MaxMTU: 9202, StackPort: true}
		} else if mtu < 0 {
			out[name] = PortInfo{Linux: linux, MTU: 16044, MaxMTU: 16044, StackPort: true}
		} else {
			out[name] = PortInfo{Linux: linux, MTU: 1500, MaxMTU: mtu}
		}
	}
	return out, true
}

func TestInventoryChecks(t *testing.T) {
	inv := fakeInv{"1/0/0": 9000, "1/0/1": 9216, "1/0/2": 9000, "1/0/3": 1500, "1/0/4": 9216, "1/9/9": -1}
	_, issues := build(t, valid+"set interfaces 1/0/3 mtu 9000\nset interfaces 1/0/7 disable\nset interfaces 1/9/9 disable\n", inv)
	s := issues.String()
	if !strings.Contains(s, "1/9/9 is a stacking port") {
		t.Errorf("missing stacking port error:\n%s", s)
	}
	if !strings.Contains(s, "exceeds the hardware maximum of 1514") {
		t.Errorf("missing hardware MTU error:\n%s", s)
	}
	if !strings.Contains(s, "port 1/0/7 does not exist") {
		t.Errorf("missing nonexistent port warning:\n%s", s)
	}
}

// The stack tunnels need the largest data frame + 58 bytes on every
// stacking port (reference 5.2, stack MTU).
func TestStackMTU(t *testing.T) {
	inv := fakeInv{"1/0/1": 9216, "1/0/2": 9216, "1/0/3": 9216, "1/0/4": 9216, "1/9/0": 9000, "1/9/8": -3, "1/9/9": -1}
	// Hosts with MTU 9000 (mtu 9014) and the reference config's 9000: fine
	// with stacking NICs limited to 9216-byte frames.
	_, issues := build(t, valid+"set interfaces 1/0/3 mtu 9014\n", inv)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", issues)
	}
	for _, c := range []struct{ mutate, where string }{
		{"set interfaces ae1 mtu 9216", "interfaces ae1 mtu"},
		{"set interfaces 1/0/3 mtu 9159", "interfaces 1/0/3 mtu"},
		{"set vlans storage mtu 9200", "vlans storage mtu"},
	} {
		_, issues := build(t, valid+c.mutate+"\n", inv)
		s := issues.String()
		if !strings.Contains(s, "stacking port 1/9/8 of member 1 carries at most 9216") ||
			!strings.Contains(s, "the largest mtu the stack can carry is 9158") || !strings.Contains(s, c.where) {
			t.Errorf("%s: want a stack MTU error at %s, got:\n%s", c.mutate, c.where, s)
		}
		if strings.Contains(s, "1/9/9") {
			t.Errorf("%s: the 16044 stacking port suffices:\n%s", c.mutate, s)
		}
	}
	// A cable that carries less than the ports do: a warning, not an error.
	vinv := verifiedInv{fakeInv: inv, path: 9000}
	_, issues = build(t, valid+"set interfaces 1/0/3 mtu 9158\n", vinv)
	if issues.HasErrors() || !strings.Contains(issues.String(), "the cable at stacking port 1/9/9 of member 1 carries only 9000") ||
		!strings.Contains(issues.String(), "or lower the mtu to 8942") {
		t.Errorf("verified path MTU:\n%s", issues)
	}
	// 9158 is the limit.
	if _, issues := build(t, valid+"set interfaces 1/0/3 mtu 9158\n", inv); issues.HasErrors() {
		t.Errorf("mtu 9158 must fit:\n%s", issues)
	}
	// A standalone switch has no stack tunnels.
	one := "set interfaces 1/0/3 mtu 9216\nset interfaces 1/0/3 unit 0 family ethernet-switching vlan members v\nset vlans v vlan-id 5\n"
	if _, issues := build(t, one, inv); issues.HasErrors() {
		t.Errorf("standalone: %s", issues)
	}
}

// verifiedInv adds a probe-verified cable size to the stacking ports.
type verifiedInv struct {
	fakeInv
	path int
}

func (v verifiedInv) Ports(member int) (map[string]PortInfo, bool) {
	ports, ok := v.fakeInv.Ports(member)
	for n, p := range ports {
		if p.StackPort {
			p.PathMTU = v.path
			ports[n] = p
		}
	}
	return ports, ok
}

// FuzzBuild ensures that model building never panics on any configuration
// the parser accepts.
func FuzzBuild(f *testing.F) {
	for _, l := range strings.Split(valid, "\n") {
		f.Add(l)
	}
	f.Fuzz(func(t *testing.T, extra string) {
		tr, err := config.ParseSet(valid + extra + "\n")
		if err != nil {
			return
		}
		Build(tr, fakeInv{"1/0/1": 1500})
	})
}

func TestInterfaceRange(t *testing.T) {
	cfg := valid + `set interface-range servers member-range 1/0/10 to 1/0/12
set interface-range servers member "1/1/*"
set interface-range servers description "server port"
set interface-range servers mtu 9014
set interface-range servers unit 0 family ethernet-switching vlan members storage
set interfaces 1/0/11 mtu 1514
set interfaces 1/0/11 unit 0 family ethernet-switching vlan members users
`
	inv := fakeInv{"1/9/0": 9000, "1/1/0": 9000, "1/1/1": 9000, "1/2/0": 9000, "1/9/9": -1}
	_, issues0 := build(t, cfg+"set interface-range all member \"1/*/*\"\n", inv)
	if !strings.Contains(issues0.String(), "already part of interface-range") {
		t.Errorf("overlapping wildcard range must be reported:\n%s", issues0)
	}
	c, issues := build(t, cfg, inv)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", issues)
	}
	for _, name := range []string{"1/0/10", "1/0/12", "1/1/0", "1/1/1"} {
		i := c.Interfaces[name]
		if i == nil || i.Range != "servers" || i.MTU != 9014 || i.AccessVLAN != 20 || i.Description != "server port" {
			t.Errorf("%s not built from range: %+v", name, i)
		}
	}
	if c.Interfaces["1/2/0"] != nil {
		t.Error("1/2/0 must not match 1/1/*")
	}
	// Explicit statements win over the range template.
	e := c.Interfaces["1/0/11"]
	if e.MTU != 1514 || e.AccessVLAN != 10 || e.Description != "server port" || !e.Explicit {
		t.Errorf("explicit override wrong: %+v", e)
	}

	// Wildcards never take stacking, management or underlay ports.
	c, issues = build(t, valid+"set interface-range all member \"*/*/*\"\nset interface-range all disable\n", inv)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", issues)
	}
	if c.Interfaces["1/9/9"] != nil || c.Interfaces["1/9/0"].Range != "" {
		t.Error("wildcard swallowed a stacking or management port")
	}
	if c.Interfaces["1/2/0"] == nil {
		t.Error("wildcard */*/* must match 1/2/0")
	}
	c, _ = build(t, valid+"set interface-range r member \"1/[1-2]/[1-5]\"\nset interface-range r disable\n", inv)
	if c.Interfaces["1/1/1"] == nil || c.Interfaces["1/1/0"] != nil || c.Interfaces["1/2/0"] != nil {
		t.Errorf("range pattern selected wrong ports")
	}
}

func TestExpandMemberRange(t *testing.T) {
	got, err := expandMemberRange("1/0/8", "1/0/11")
	if err != nil || strings.Join(got, ",") != "1/0/8,1/0/9,1/0/10,1/0/11" {
		t.Errorf("range = %v %v", got, err)
	}
	got, err = expandMemberRange("2/1/0", "2/1/2")
	if err != nil || strings.Join(got, ",") != "2/1/0,2/1/1,2/1/2" {
		t.Errorf("range = %v %v", got, err)
	}
	if _, err := expandMemberRange("1/0/0", "1/1/3"); err == nil {
		t.Error("range across cards must fail")
	}
}

func TestInactiveIgnored(t *testing.T) {
	tr, err := config.ParseSet(`set vlans v10 vlan-id 10
set vlans dup vlan-id 10
deactivate vlans dup
`)
	if err != nil {
		t.Fatal(err)
	}
	cfg, issues := Build(tr, nil)
	if issues.HasErrors() {
		t.Fatalf("inactive statement validated:\n%s", issues)
	}
	if _, ok := cfg.VLANs["dup"]; ok {
		t.Error("inactive VLAN present in the model")
	}
	if err := config.ApplySetLines(tr, "activate vlans dup"); err != nil {
		t.Fatal(err)
	}
	if _, issues := Build(tr, nil); !issues.HasErrors() {
		t.Error("duplicate vlan-id not reported after activation")
	}
}

func TestLLDP(t *testing.T) {
	c, issues := build(t, valid+"set protocols lldp interface ae1\nset protocols lldp interface 1/0/4 disable\nset protocols lldp interface 1/7/7\n", nil)
	if issues.HasErrors() || !strings.Contains(issues.String(), "1/7/7 is not configured") {
		t.Fatalf("issues:\n%s", issues)
	}
	l := c.LLDP
	if l == nil || l.Interval != 30 || l.Hold != 4 || !l.Runs("1/0/1", "ae1") || l.Runs("1/0/3", "") || l.Runs("1/0/4", "") {
		t.Errorf("lldp: %+v", l)
	}
	c, _ = build(t, valid+"set protocols lldp advertisement-interval 10\n", nil)
	if !c.LLDP.Runs("1/0/3", "") || c.LLDP.Interval != 10 {
		t.Errorf("lldp on all ports: %+v", c.LLDP)
	}
	if c, _ = build(t, valid+"set protocols lldp disable\n", nil); c.LLDP != nil {
		t.Error("disabled LLDP runs")
	}
}

func TestWildcardSkipsManagementPorts(t *testing.T) {
	inv := fakeInv{"1/0/0": 9000, "1/0/1": 9000, "1/0/9": 9000}
	cfg, issues := build(t, "set vlans v vlan-id 10\nset interfaces 1/0/9 management\n"+
		"set interface-range all member \"1/0/*\"\n"+
		"set interface-range all unit 0 family ethernet-switching vlan members v\n", inv)
	if issues.HasErrors() {
		t.Fatal(issues)
	}
	if i := cfg.Interfaces["1/0/9"]; i == nil || i.Switching || !i.Management {
		t.Errorf("wildcard selected the management port: %+v", i)
	}
	if _, ok := cfg.Interfaces["1/0/0"]; !ok {
		t.Error("wildcard did not select 1/0/0")
	}
	if got := cfg.MgmtPorts(1); len(got) != 1 || got[0] != "1/0/9" {
		t.Errorf("management ports: %v", got)
	}
}

func TestL3(t *testing.T) {
	base := `set vlans v10 vlan-id 10
set vlans v10 l3-interface irb.10
set vlans v20 vlan-id 20
set vlans v20 l3-interface irb.20
set interfaces irb unit 10 family inet address 10.0.10.1/24
set interfaces irb unit 10 family inet6 address 2001:db8:10::1/64
set interfaces irb unit 20 family inet address 10.0.20.1/24
set interfaces 1/0/1 unit 0 family ethernet-switching interface-mode trunk
set interfaces 1/0/1 unit 0 family ethernet-switching vlan members [ v10 v20 ]
set interfaces 1/0/5 unit 0 family inet address 10.1.1.1/30
set interfaces 1/0/6 vlan-tagging
set interfaces 1/0/6 unit 0 family inet address 10.2.0.1/24
set interfaces 1/0/6 unit 100 vlan-id 100
set interfaces 1/0/6 unit 100 family inet address 10.3.0.1/24
set routing-options static route 0.0.0.0/0 next-hop 10.1.1.2
set routing-options static route 192.0.2.0/24 discard
set routing-options static route 2001:db8:99::/48 next-hop 2001:db8:10::fe
`
	c, issues := build(t, base, nil)
	if issues.HasErrors() || len(issues) > 0 {
		t.Fatalf("unexpected issues:\n%s", issues)
	}
	irb := c.L3["irb.10"]
	if irb == nil || irb.VLAN != 10 || !irb.IRB() || len(irb.Addrs) != 2 {
		t.Errorf("irb.10: %+v", irb)
	}
	if u := c.L3["1/0/6.100"]; u == nil || u.Tag != 100 || u.Member != 1 || u.Parent != "1/0/6" {
		t.Errorf("subinterface: %+v", u)
	}
	if u := c.L3["1/0/5.0"]; u == nil || u.Tag != 0 || c.Interfaces["1/0/5"].Switching {
		t.Errorf("routed port: %+v", u)
	}
	if len(c.Routes) != 3 || !c.Routes[1].Discard {
		t.Errorf("routes: %+v", c.Routes)
	}
	cases := []struct{ mutate, want string }{
		{"set interfaces 1/0/1 unit 0 family inet address 10.9.0.1/24", "either switched"},
		{"set interfaces 1/0/7 unit 3 family ethernet-switching", "only valid on unit 0"},
		{"set interfaces 1/0/7 unit 3 family inet address 10.9.0.1/24", "need 'vlan-tagging'"},
		{"set interfaces 1/0/6 unit 101 family inet address 10.9.0.1/24", "needs a vlan-id"},
		{"set interfaces 1/0/6 unit 101 vlan-id 100", "already used by unit 100"},
		{"set interfaces 1/0/1 vlan-tagging", "a switch port uses interface-mode trunk"},
		{"set interfaces 1/0/7 unit 0 vlan-id 7", "needs 'vlan-tagging'"},
		{"set interfaces irb unit 30 vlan-id 30", "attached to a VLAN"},
		{"set interfaces irb mtu 9000", "only 'unit'"},
		{"set vlans v30 vlan-id 30\nset vlans v30 l3-interface irb.30", "irb.30 is not configured"},
		{"set vlans v30 vlan-id 30\nset vlans v30 l3-interface irb.10", "already the l3-interface of vlan v10"},
		{"set interfaces irb unit 30 family inet address 10.0.10.9/16", "overlaps 10.0.10.1/24"},
		{"set interfaces 1/0/7 unit 0 family inet address 10.8.0.0/24", "network address"},
		{"set interfaces 1/0/7 unit 0 family inet address 10.8.0.255/24", "broadcast address"},
		{"set interfaces 1/0/7 unit 0 family inet6 address 10.8.0.1/24", "not an address of family inet6"},
		{"set routing-options static route 10.50.0.0/16 next-hop 2001:db8::1", "not of the prefix's address family"},
		{"set routing-options static route 10.50.0.0/16 discard\nset routing-options static route 10.50.0.0/16 next-hop 10.1.1.2", "mutually exclusive"},
		{"set routing-options static route 10.50.0.0/16", "needs 'next-hop' or 'discard'"},
		{"set interfaces 1/0/2 ether-options 802.3ad ae1\nset interfaces 1/0/2 unit 0 family inet address 10.9.0.1/24", "configure routing on ae1"},
	}
	for _, cs := range cases {
		_, issues := build(t, base+cs.mutate+"\n", nil)
		if !issues.HasErrors() || !strings.Contains(issues.String(), cs.want) {
			t.Errorf("%q: want error %q, got:\n%s", cs.mutate, cs.want, issues)
		}
	}
	warnings := []struct{ mutate, want string }{
		{"set interfaces irb unit 30 family inet address 10.0.30.1/24", "not the l3-interface of any VLAN"},
		{"set routing-options static route 10.60.0.0/16 next-hop 172.16.0.1", "stays inactive"},
	}
	for _, cs := range warnings {
		_, issues := build(t, base+cs.mutate+"\n", nil)
		if issues.HasErrors() || !strings.Contains(issues.String(), cs.want) {
			t.Errorf("%q: want warning %q, got:\n%s", cs.mutate, cs.want, issues)
		}
	}
}

type capsInv map[string]PortInfo

func (c capsInv) Ports(member int) (map[string]PortInfo, bool) { return c, member == 1 }

func TestCapabilityChecks(t *testing.T) {
	inv := capsInv{
		"1/0/0": {Linux: "a", MaxSpeedMbps: 1000},
		"1/0/1": {Linux: "b", MaxSpeedMbps: 10000},
		"1/0/2": {Linux: "c", NoPause: true},
		"1/0/3": {Linux: "d", VlanChallenged: true},
	}
	base := "set vlans v vlan-id 10\nset vlans w vlan-id 20\n"
	_, issues := build(t, base+"set interfaces ae0 unit 0 family ethernet-switching vlan members v\n"+
		"set interfaces 1/0/0 ether-options 802.3ad ae0\nset interfaces 1/0/1 ether-options 802.3ad ae0\n", inv)
	if !strings.Contains(issues.String(), "different maximum speeds (1G: 1/0/0; 10G: 1/0/1)") || issues.HasErrors() {
		t.Errorf("bundle speeds:\n%s", issues)
	}
	_, issues = build(t, base+"set interfaces 1/0/2 ether-options flow-control\n", inv)
	if !strings.Contains(issues.String(), "no pause-frame support") || issues.HasErrors() {
		t.Errorf("pause:\n%s", issues)
	}
	_, issues = build(t, base+"set interfaces 1/0/3 unit 0 family ethernet-switching interface-mode trunk\nset interfaces 1/0/3 unit 0 family ethernet-switching vlan members [ v w ]\n", inv)
	if !strings.Contains(issues.String(), "cannot carry VLAN tags") || !issues.HasErrors() {
		t.Errorf("vlan-challenged trunk:\n%s", issues)
	}
	_, issues = build(t, base+"set interfaces 1/0/3 unit 0 family ethernet-switching vlan members v\n", inv)
	if issues.HasErrors() {
		t.Errorf("vlan-challenged access port must be fine:\n%s", issues)
	}
	// Unknown speeds: no check.
	_, issues = build(t, base+"set interfaces ae0 unit 0 family ethernet-switching vlan members v\n"+
		"set interfaces 1/0/0 ether-options 802.3ad ae0\nset interfaces 1/0/2 ether-options 802.3ad ae0\n", inv)
	if strings.Contains(issues.String(), "different maximum speeds") {
		t.Errorf("unknown speed compared:\n%s", issues)
	}
}

func TestRoutingInstances(t *testing.T) {
	cfg := `set virtual-chassis member 1 host-name a
set virtual-chassis member 2 host-name b
set system management-instance oob
set vlans mgmt vlan-id 99
set vlans mgmt l3-interface irb.99
set interfaces irb unit 99 family inet address 10.5.176.95/16
set routing-instances oob interface irb.99
set routing-instances oob routing-options static route 0.0.0.0/0 next-hop 10.5.0.1
set vlans lab vlan-id 98
set vlans lab l3-interface irb.98
set interfaces irb unit 98 family inet address 10.6.0.1/16 member 1
set interfaces irb unit 98 family inet address 10.6.0.2/16 member 2
set routing-instances lab interface irb.98
set vlans v10 vlan-id 10
set vlans v10 l3-interface irb.10
set interfaces irb unit 10 family inet address 10.5.0.2/24
set vlans v20 vlan-id 20
set vlans v20 l3-interface irb.20
set interfaces irb unit 20 family inet address 10.5.0.3/24
set routing-instances blue instance-type virtual-router
set routing-instances blue interface irb.20
set interfaces 1/0/1 unit 0 family ethernet-switching vlan members mgmt
set interfaces 2/0/1 unit 0 family ethernet-switching vlan members mgmt
`
	c, issues := build(t, cfg, nil)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", issues)
	}
	irb := c.L3["irb.99"]
	if irb.Instance != "oob" || len(irb.AddrsOn(2)) != 1 || irb.AddrsOn(2)[0].String() != "10.5.176.95/16" {
		t.Errorf("irb.99: %+v", irb)
	}
	if got := c.MgmtUnits(); len(got) != 1 || got[0].Name != "irb.99" {
		t.Errorf("management units: %v", got)
	}
	if lab := c.L3["irb.98"]; len(lab.AddrsOn(2)) != 1 || lab.AddrsOn(2)[0].String() != "10.6.0.2/16" {
		t.Errorf("irb.98 on member 2: %v", lab.AddrsOn(2))
	}
	// irb.10 (default) and irb.20 (blue) overlap: allowed, different
	// instances; irb.10 and irb.99 overlap within... no: different instances too.
	if c.L3["irb.20"].Instance != "blue" || c.L3["irb.10"].Instance != "" || len(c.Instances["blue"].Units) != 1 {
		t.Errorf("instances: %+v %+v", c.L3["irb.20"], c.Instances["blue"])
	}
	_, issues = build(t, cfg+"set interfaces irb unit 20 family inet address 10.5.0.9/24\nset routing-instances blue interface irb.21\n", nil)
	if !strings.Contains(issues.String(), "irb.21 is not a routed interface") {
		t.Errorf("unknown unit in instance:\n%s", issues)
	}
	// Two members with the same management address is an error.
	_, issues = build(t, strings.Replace(cfg, "10.6.0.2/16 member 2", "10.6.0.1/17 member 2", 1), nil)
	if !issues.HasErrors() {
		t.Errorf("same address on two members accepted:\n%s", issues)
	}
	// Overlap within one instance is still an error.
	_, issues = build(t, cfg+"set interfaces irb unit 30 family inet address 10.5.0.4/24\nset vlans v30 vlan-id 30\nset vlans v30 l3-interface irb.30\n", nil)
	if !strings.Contains(issues.String(), "overlaps 10.5.0.2/24 on irb.10") {
		t.Errorf("overlap in the default instance:\n%s", issues)
	}
}
