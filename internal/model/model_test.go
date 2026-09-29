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
set stack member 1 management interface eth0
set stack member 1 management address 192.168.1.11/24
set stack member 1 vtep-address 10.255.0.1
set stack member 2 host-name sw-b
set stack member 2 management interface eth0
set stack member 2 management address 192.168.1.12/24
set stack member 2 vtep-address 10.255.0.2
set interfaces 1/eth1 ether-options 802.3ad ae0
set interfaces 2/eth1 ether-options 802.3ad ae0
set interfaces 1/eth2 ether-options 802.3ad ae1
set interfaces 2/eth2 ether-options 802.3ad ae1
set interfaces ae0 mtu 9216
set interfaces ae0 aggregated-ether-options lacp active
set interfaces ae1 mtu 9000
set interfaces ae1 aggregated-ether-options lacp active
set interfaces ae1 aggregated-ether-options mclag
set interfaces ae1 unit 0 family ethernet-switching interface-mode trunk
set interfaces ae1 unit 0 family ethernet-switching vlan members [ users 20 ]
set interfaces ae1 native-vlan-id users
set interfaces 1/eth3 unit 0 family ethernet-switching vlan members storage
set interfaces 1/eth4 description mirror-target
set vlans users vlan-id 10
set vlans users vxlan vni 10010
set vlans storage vlan-id 20
set vlans storage mtu 9000
set mclag domain 1 members [ 1 2 ]
set mclag domain 1 peer-link ae0
set protocols rstp interface ae1 edge
set forwarding-options analyzer dbg input ingress interface 1/eth3
set forwarding-options analyzer dbg output interface 1/eth4
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
	if strings.Join(ae1.MemberPorts, ",") != "1/eth2,2/eth2" || len(ae1.MemberIDs) != 2 {
		t.Errorf("ae1 members = %v %v", ae1.MemberPorts, ae1.MemberIDs)
	}
	if c.Interfaces["1/eth2"].MTU != 9000 {
		t.Errorf("member port must inherit ae MTU")
	}
	if c.Interfaces["1/eth3"].AccessVLAN != 20 {
		t.Errorf("access vlan = %d", c.Interfaces["1/eth3"].AccessVLAN)
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
	c, issues := build(t, "set interfaces 1/eth0 unit 0 family ethernet-switching vlan members v\nset vlans v vlan-id 5\n", nil)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", issues)
	}
	if _, ok := c.Members[1]; !ok {
		t.Fatal("implicit member 1 missing")
	}
	_, issues = build(t, "set interfaces 2/eth0 disable\n", nil)
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
		{"missing vlan", "set interfaces 1/eth3 unit 0 family ethernet-switching vlan members nosuch", `vlan "nosuch" is not defined`},
		{"undefined id range", "set interfaces ae1 unit 0 family ethernet-switching vlan members 30-32", "vlan-id 30-32 is not defined"},
		{"access two vlans", "set interfaces 1/eth3 unit 0 family ethernet-switching vlan members users", "exactly one VLAN"},
		{"native on access", "set interfaces 1/eth3 native-vlan-id users", "only valid in trunk mode"},
		{"dup vlan id", "set vlans dup vlan-id 10", "already used by vlan"},
		{"vlan without id", "set vlans noid description x", "vlan-id is required"},
		{"dup vni", "set vlans storage vxlan vni 10010", "vni 10010 is already used"},
		{"vtep missing", "delete stack member 2 vtep-address", "vtep-address is required"},
		{"unknown ae", "set interfaces 1/eth5 ether-options 802.3ad ae9", "ae9 is not configured"},
		{"member with family", "set interfaces 1/eth1 unit 0 family ethernet-switching", "cannot have 'unit 0 family"},
		{"ae ether-options", "set interfaces ae1 ether-options flow-control", "only valid on physical ports"},
		{"phys agg options", "set interfaces 1/eth3 aggregated-ether-options lacp active", "only valid on ae interfaces"},
		{"span without mclag", "delete interfaces ae1 aggregated-ether-options mclag", "require 'aggregated-ether-options mclag'"},
		{"rstp timers", "set protocols rstp max-age 40", "timers violate"},
		{"bpdu-block unknown", "set protocols layer2-control bpdu-block interface 1/eth9", "1/eth9 is not configured"},
		{"mclag without lacp", "delete interfaces ae1 aggregated-ether-options lacp", "require 'lacp'"},
		{"peer-link mtu", "set interfaces ae0 mtu 1500", "peer-link MTU 1500 is smaller"},
		{"domain members", "set mclag domain 1 members 3", "exactly two members"},
		{"no peer-link", "delete mclag domain 1 peer-link", "peer-link is required"},
		{"analyzer same port", "set forwarding-options analyzer dbg input ingress interface 1/eth4", "both input and output"},
		{"analyzer no output", "delete forwarding-options analyzer dbg output", "output interface is required"},
		{"analyzer cross member", "set interfaces 2/eth9 description x\nset forwarding-options analyzer dbg input egress interface 2/eth9", "ports on the output's member"},
		{"analyzer mclag output", "set forwarding-options analyzer dbg output interface ae1", "cannot be a mirror output"},
		{"rstp on member port", "set protocols rstp interface 1/eth1 edge", "configure RSTP on the aggregated interface"},
		{"mgmt is switch port", "set interfaces 1/eth0 unit 0 family ethernet-switching vlan members storage", "cannot carry an IP interface"},
		{"dup hostname", "set stack member 2 host-name sw-a", "already used by member 1"},
		{"three members", "set stack member 3 host-name sw-c\nset interfaces 3/eth2 ether-options 802.3ad ae1", "at most two"},
		{"witness ports", "set stack member 2 role witness", "is a witness"},
		{"mgmt vlan undefined", "set stack member 1 management vlan 99", "vlan-id 99 is not defined"},
		{"dhcp and static v4", "set stack member 1 management dhcp", "either 'dhcp' or a static IPv4"},
		{"two v4 gateways", "set stack member 1 management gateway [ 192.168.1.1 192.168.1.2 ]", "at most one gateway"},
		{"address without attach", "set stack member 3 host-name c\nset stack member 3 management address 10.1.1.1/24", "require 'vlan' or 'interface'"},
		{"mgmt and underlay same vlan", "set stack member 1 management vlan storage\nset stack member 1 underlay vlan storage", "cannot share a VLAN"},
		{"underlay in vxlan vlan", "set stack member 1 underlay vlan users", "cannot carry the VXLAN underlay"},
		{"range overlap", "set interface-range a member-range 1/eth10 to 1/eth12\nset interface-range b member-range 1/eth12 to 1/eth13", "already part of interface-range a"},
		{"range bad ends", "set interface-range a member-range 1/eth10 to 2/eth12", "same member"},
		{"range reversed", "set interface-range a member-range 1/eth12 to 1/eth10", "comes before"},
		{"range prefix", "set interface-range a member-range 1/eth1 to 1/enp2", "share a name prefix"},
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
		{"set interfaces 1/eth3 mtu 1500\nset vlans storage mtu 9000", "larger frames are dropped"},
		{"set interfaces ae1 aggregated-ether-options minimum-links 3", "can never come up"},
		{"set system login user bob class operator", "cannot log in"},
		{"set interfaces ae1 aggregated-ether-options lacp system-priority 100", "ignored on MC-LAG interfaces"},
		{"set system name-server [ 1.1.1.1 1.0.0.1 8.8.8.8 9.9.9.9 ]", "only the first 3"},
		{"set stack member 1 underlay interface eth5\nset stack member 1 underlay address 10.9.0.1/24\nset interfaces 1/eth5 mtu 1514", "underlay MTU 1514 is below 1564"},
		{"set stack member 1 management gateway 192.168.1.1\nset stack member 1 management gateway 2001:db8::1", "has no address of its family"},
		{"set vlans lonely vlan-id 77\nset stack member 2 management vlan lonely", "no switch port of member 2 carries vlan-id 77"},
		{"set protocols layer2-control bpdu-block interface ae1\nset protocols rstp interface ae1 cost 10\ndelete protocols rstp interface ae1 edge", "non-edge port"},
		{"set interfaces 1/eth4 unit 0 family ethernet-switching vlan members storage", "carries switched traffic"},
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

// fakeInv describes member 1's ports: Linux name -> max Linux MTU. A
// negative value marks a stacking port.
type fakeInv map[string]int

func (f fakeInv) Ports(member int) (map[string]PortInfo, bool) {
	if member != 1 {
		return nil, false
	}
	out := map[string]PortInfo{}
	for name, mtu := range f {
		if mtu == -2 {
			out[name] = PortInfo{MTU: 1500, MaxMTU: 9000, HasIP: true} // OS management NIC
		} else if mtu < 0 {
			out[name] = PortInfo{MTU: 1500, MaxMTU: 1500, StackPort: true}
		} else {
			out[name] = PortInfo{MTU: 1500, MaxMTU: mtu}
		}
	}
	return out, true
}

func TestInventoryChecks(t *testing.T) {
	inv := fakeInv{"eth0": 9000, "eth1": 9216, "eth2": 9000, "eth3": 1500, "eth4": 9216, "stk0": -1}
	_, issues := build(t, valid+"set interfaces 1/eth3 mtu 9000\nset interfaces 1/eth7 disable\nset interfaces 1/stk0 disable\n", inv)
	s := issues.String()
	if !strings.Contains(s, "stk0 is a stacking port") {
		t.Errorf("missing stacking port error:\n%s", s)
	}
	if !strings.Contains(s, "exceeds the hardware maximum of 1514") {
		t.Errorf("missing hardware MTU error:\n%s", s)
	}
	if !strings.Contains(s, "port eth7 does not exist") {
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
		Build(tr, fakeInv{"eth1": 1500})
	})
}

func TestInterfaceRange(t *testing.T) {
	cfg := valid + `set interface-range servers member-range 1/eth10 to 1/eth12
set interface-range servers member "1/enp1s*"
set interface-range servers description "server port"
set interface-range servers mtu 9014
set interface-range servers unit 0 family ethernet-switching vlan members storage
set interfaces 1/eth11 mtu 1514
set interfaces 1/eth11 unit 0 family ethernet-switching vlan members users
`
	inv := fakeInv{"eth0": 9000, "enp1s0": 9000, "enp1s1": 9000, "enp2s0": 9000, "stk0": -1}
	c, issues := build(t, cfg+"set interface-range all member \"1/*\"\n", inv)
	if !strings.Contains(issues.String(), "already part of interface-range") {
		t.Errorf("overlapping wildcard range must be reported:\n%s", issues)
	}
	c, issues = build(t, cfg, inv)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", issues)
	}
	for _, name := range []string{"1/eth10", "1/eth12", "1/enp1s0", "1/enp1s1"} {
		i := c.Interfaces[name]
		if i == nil || i.Range != "servers" || i.MTU != 9014 || i.AccessVLAN != 20 || i.Description != "server port" {
			t.Errorf("%s not built from range: %+v", name, i)
		}
	}
	if c.Interfaces["1/enp2s0"] != nil {
		t.Error("enp2s0 must not match 1/enp1s*")
	}
	// Explicit statements win over the range template.
	e := c.Interfaces["1/eth11"]
	if e.MTU != 1514 || e.AccessVLAN != 10 || e.Description != "server port" || !e.Explicit {
		t.Errorf("explicit override wrong: %+v", e)
	}

	// Wildcards never take stacking, management or underlay ports.
	c, issues = build(t, valid+"set interface-range all member \"*/*\"\nset interface-range all disable\n", inv)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", issues)
	}
	if c.Interfaces["1/stk0"] != nil || c.Interfaces["1/eth0"] != nil {
		t.Error("wildcard swallowed a stacking or management port")
	}
	if c.Interfaces["1/enp2s0"] == nil {
		t.Error("wildcard */* must match 1/enp2s0")
	}
}

func TestExpandMemberRange(t *testing.T) {
	got, err := expandMemberRange("1/eth08", "1/eth11")
	if err != nil || strings.Join(got, ",") != "1/eth08,1/eth09,1/eth10,1/eth11" {
		t.Errorf("padded range = %v %v", got, err)
	}
	got, err = expandMemberRange("2/enp1s0", "2/enp1s2")
	if err != nil || strings.Join(got, ",") != "2/enp1s0,2/enp1s1,2/enp1s2" {
		t.Errorf("range = %v %v", got, err)
	}
	if _, err := expandMemberRange("1/eth0", "1/eth99999"); err == nil {
		t.Error("oversized range must fail")
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
	inv := fakeInv{"eth0": 9000, "eth1": 9000, "eth9": -2}
	cfg, issues := build(t, "set vlans v vlan-id 10\n"+
		"set interface-range all member \"1/eth*\"\n"+
		"set interface-range all unit 0 family ethernet-switching vlan members v\n", inv)
	if issues.HasErrors() {
		t.Fatal(issues)
	}
	if _, ok := cfg.Interfaces["1/eth9"]; ok {
		t.Error("wildcard selected the port with OS IP addresses")
	}
	if _, ok := cfg.Interfaces["1/eth0"]; !ok {
		t.Error("wildcard did not select eth0")
	}
	_, issues = build(t, "set interfaces 1/eth9 disable\n", inv)
	if !strings.Contains(issues.String(), "eth9 has IP addresses configured by the operating system") {
		t.Errorf("no warning for an explicit management NIC:\n%s", issues)
	}
}
