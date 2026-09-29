package model

import (
	"strings"
	"testing"

	"mclag/internal/config"
)

const valid = `
set system host-name core
set system login user alice class super-user
set system login user alice authentication encrypted-password "$6$abc$def"
set stack member 1 host-name sw-a
set stack member 1 management interface 1/9/0
set stack member 1 management address 192.168.1.11/24
set stack member 1 vtep-address 10.255.0.1
set stack member 2 host-name sw-b
set stack member 2 management interface 2/9/0
set stack member 2 management address 192.168.1.12/24
set stack member 2 vtep-address 10.255.0.2
set interfaces 1/0/1 ether-options 802.3ad ae0
set interfaces 2/0/1 ether-options 802.3ad ae0
set interfaces 1/0/2 ether-options 802.3ad ae1
set interfaces 2/0/2 ether-options 802.3ad ae1
set interfaces ae0 mtu 9216
set interfaces ae0 aggregated-ether-options lacp active
set interfaces ae1 mtu 9000
set interfaces ae1 aggregated-ether-options lacp active
set interfaces ae1 aggregated-ether-options mclag
set interfaces ae1 unit 0 family ethernet-switching interface-mode trunk
set interfaces ae1 unit 0 family ethernet-switching vlan members [ users 20 ]
set interfaces ae1 native-vlan-id users
set interfaces 1/0/3 unit 0 family ethernet-switching vlan members storage
set interfaces 1/0/4 description mirror-target
set vlans users vlan-id 10
set vlans users vxlan vni 10010
set vlans storage vlan-id 20
set vlans storage mtu 9000
set mclag domain 1 members [ 1 2 ]
set mclag domain 1 peer-link ae0
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
	if strings.Join(ae1.MemberPorts, ",") != "1/0/2,2/0/2" || len(ae1.MemberIDs) != 2 {
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
		{"vtep missing", "delete stack member 2 vtep-address", "vtep-address is required"},
		{"unknown ae", "set interfaces 1/0/5 ether-options 802.3ad ae9", "ae9 is not configured"},
		{"member with family", "set interfaces 1/0/1 unit 0 family ethernet-switching", "cannot have 'unit 0 family"},
		{"ae ether-options", "set interfaces ae1 ether-options flow-control", "only valid on physical ports"},
		{"phys agg options", "set interfaces 1/0/3 aggregated-ether-options lacp active", "only valid on ae interfaces"},
		{"span without mclag", "delete interfaces ae1 aggregated-ether-options mclag", "require 'aggregated-ether-options mclag'"},
		{"rstp timers", "set protocols rstp max-age 40", "timers violate"},
		{"bpdu-block unknown", "set protocols layer2-control bpdu-block interface 1/0/9", "1/0/9 is not configured"},
		{"mclag without lacp", "delete interfaces ae1 aggregated-ether-options lacp", "require 'lacp'"},
		{"peer-link mtu", "set interfaces ae0 mtu 1500", "peer-link MTU 1500 is smaller"},
		{"domain members", "set mclag domain 1 members 3", "exactly two members"},
		{"no peer-link", "delete mclag domain 1 peer-link", "peer-link is required"},
		{"analyzer same port", "set forwarding-options analyzer dbg input ingress interface 1/0/4", "both input and output"},
		{"analyzer no output", "delete forwarding-options analyzer dbg output", "output interface is required"},
		{"analyzer cross member", "set interfaces 2/0/9 description x\nset forwarding-options analyzer dbg input egress interface 2/0/9", "ports on the output's member"},
		{"analyzer mclag output", "set forwarding-options analyzer dbg output interface ae1", "cannot be a mirror output"},
		{"rstp on member port", "set protocols rstp interface 1/0/1 edge", "configure RSTP on the aggregated interface"},
		{"mgmt is switch port", "set interfaces 1/9/0 unit 0 family ethernet-switching vlan members storage", "cannot carry an IP interface"},
		{"dup hostname", "set stack member 2 host-name sw-a", "already used by member 1"},
		{"three members", "set stack member 3 host-name sw-c\nset interfaces 3/0/2 ether-options 802.3ad ae1", "at most two"},
		{"witness ports", "set stack member 2 role witness", "is a witness"},
		{"mgmt vlan undefined", "set stack member 1 management vlan 99", "vlan-id 99 is not defined"},
		{"dhcp and static v4", "set stack member 1 management dhcp", "either 'dhcp' or a static IPv4"},
		{"two v4 gateways", "set stack member 1 management gateway [ 192.168.1.1 192.168.1.2 ]", "at most one gateway"},
		{"address without attach", "set stack member 3 host-name c\nset stack member 3 management address 10.1.1.1/24", "require 'vlan' or 'interface'"},
		{"mgmt and underlay same vlan", "set stack member 1 management vlan storage\nset stack member 1 underlay vlan storage", "cannot share a VLAN"},
		{"underlay in vxlan vlan", "set stack member 1 underlay vlan users", "cannot carry the VXLAN underlay"},
		{"range overlap", "set interface-range a member-range 1/0/10 to 1/0/12\nset interface-range b member-range 1/0/12 to 1/0/13", "already part of interface-range a"},
		{"range bad ends", "set interface-range a member-range 1/0/10 to 2/0/12", "same member"},
		{"range reversed", "set interface-range a member-range 1/0/12 to 1/0/10", "comes before"},
		{"range across cards", "set interface-range a member-range 1/0/1 to 1/1/2", "same member and card"},
		{"mgmt port of other member", "set stack member 1 management interface 2/0/0", "belongs to member 2"},
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
		{"set interfaces ae1 aggregated-ether-options minimum-links 3", "can never come up"},
		{"set system login user bob class operator", "cannot log in"},
		{"set interfaces ae1 aggregated-ether-options lacp system-priority 100", "ignored on MC-LAG interfaces"},
		{"set system name-server [ 1.1.1.1 1.0.0.1 8.8.8.8 9.9.9.9 ]", "only the first 3"},
		{"set stack member 1 underlay interface 1/0/5\nset stack member 1 underlay address 10.9.0.1/24\nset interfaces 1/0/5 mtu 1514", "underlay MTU 1514 is below 1564"},
		{"set stack member 1 management gateway 192.168.1.1\nset stack member 1 management gateway 2001:db8::1", "has no address of its family"},
		{"set vlans lonely vlan-id 77\nset stack member 2 management vlan lonely", "no switch port of member 2 carries vlan-id 77"},
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
// negative value marks a stacking port, -2 a port with OS IP addresses.
type fakeInv map[string]int

func (f fakeInv) Ports(member int) (map[string]PortInfo, bool) {
	if member != 1 {
		return nil, false
	}
	out := map[string]PortInfo{}
	for name, mtu := range f {
		linux := "lx" + strings.ReplaceAll(name, "/", "")
		if mtu == -2 {
			out[name] = PortInfo{Linux: linux, MTU: 1500, MaxMTU: 9000, HasIP: true} // OS management NIC
		} else if mtu < 0 {
			out[name] = PortInfo{Linux: linux, MTU: 1500, MaxMTU: 1500, StackPort: true}
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
	c, issues := build(t, cfg+"set interface-range all member \"1/*/*\"\n", inv)
	if !strings.Contains(issues.String(), "already part of interface-range") {
		t.Errorf("overlapping wildcard range must be reported:\n%s", issues)
	}
	c, issues = build(t, cfg, inv)
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
	if c.Interfaces["1/9/9"] != nil || c.Interfaces["1/9/0"] != nil {
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

func TestWildcardSkipsPortsWithIP(t *testing.T) {
	inv := fakeInv{"1/0/0": 9000, "1/0/1": 9000, "1/0/9": -2}
	cfg, issues := build(t, "set vlans v vlan-id 10\n"+
		"set interface-range all member \"1/0/*\"\n"+
		"set interface-range all unit 0 family ethernet-switching vlan members v\n", inv)
	if issues.HasErrors() {
		t.Fatal(issues)
	}
	if _, ok := cfg.Interfaces["1/0/9"]; ok {
		t.Error("wildcard selected the port with OS IP addresses")
	}
	if _, ok := cfg.Interfaces["1/0/0"]; !ok {
		t.Error("wildcard did not select 1/0/0")
	}
	_, issues = build(t, "set interfaces 1/0/9 disable\n", inv)
	if !strings.Contains(issues.String(), "1/0/9 (lx109) has IP addresses configured by the operating system") {
		t.Errorf("no warning for an explicit management NIC:\n%s", issues)
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
		{"set stack member 1 management vlan v10\nset stack member 1 management address 10.7.0.1/24", "management VLAN of member 1"},
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
	// A routed data plane next to the OS management NIC in the same instance.
	_, issues = build(t, base, fakeInv{"1/2/0": -2})
	if !strings.Contains(issues.String(), "operating-system management port 1/2/0") {
		t.Errorf("no management warning:\n%s", issues)
	}
}
