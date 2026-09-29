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
		{"analyzer cross member", "set interfaces 2/eth9 description x\nset forwarding-options analyzer dbg input egress interface 2/eth9", "same stack member"},
		{"rstp on member port", "set protocols rstp interface 1/eth1 edge", "configure RSTP on the aggregated interface"},
		{"mgmt is switch port", "set interfaces 1/eth0 unit 0 family ethernet-switching vlan members storage", "cannot be the management interface"},
		{"dup hostname", "set stack member 2 host-name sw-a", "already used by member 1"},
		{"three members", "set stack member 3 host-name sw-c\nset interfaces 3/eth2 ether-options 802.3ad ae1", "at most two"},
		{"witness ports", "set stack member 2 role witness", "is a witness"},
		{"mgmt vlan undefined", "delete stack member 1 management interface\nset stack member 1 management vlan 99", "vlan-id 99 is not defined"},
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
		{"set stack member 1 underlay interface eth5\nset interfaces 1/eth5 mtu 1500", "underlay MTU 1500 is below 1550"},
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

type fakeInv map[string]int

func (f fakeInv) Port(member int, linux string) (PortInfo, bool, bool) {
	if member != 1 {
		return PortInfo{}, false, false
	}
	mtu, ok := f[linux]
	return PortInfo{MaxMTU: mtu}, ok, true
}

func TestInventoryChecks(t *testing.T) {
	inv := fakeInv{"eth0": 9000, "eth1": 9216, "eth2": 9000, "eth3": 1500, "eth4": 9216}
	_, issues := build(t, valid+"set interfaces 1/eth3 mtu 9000\nset interfaces 1/eth7 disable\n", inv)
	s := issues.String()
	if !strings.Contains(s, "exceeds the hardware maximum of 1500") {
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
