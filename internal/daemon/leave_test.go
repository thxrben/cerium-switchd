package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"mclag/internal/config"
)

func TestStandaloneRewrite(t *testing.T) {
	tr, err := config.ParseSet(`set virtual-chassis member 1 host-name a
set virtual-chassis member 2 host-name b
set mclag domain 1 members 1
set mclag domain 1 members 2
set vlans v10 vlan-id 10
set vlans v10 l3-interface irb.10
set interfaces 2/0/1 unit 0 family ethernet-switching vlan members v10
set interfaces 2/0/2 mtu 9014
set interfaces 1/0/3 disable
set interfaces 3/0/1 disable
set interfaces ae1 aggregated-ether-options lacp active
set interfaces ae1 aggregated-ether-options mclag
set interfaces 2/0/4 ether-options 802.3ad ae1
set interfaces irb unit 10 family inet address 10.0.0.2/24 member 2
set interfaces irb unit 10 family inet address 10.0.0.3/24 member 3
set interfaces irb unit 10 family inet address 10.0.0.1/24
set interface-range r member 2/0/*
set interface-range r member 3/0/*
set interface-range r member */1/*
set interface-range r member-range 2/0/6 to 2/0/8
set routing-instances blue interface 2/0/2.0
set routing-instances blue interface 1/0/3.0
set forwarding-options analyzer a input ingress interface 2/0/1
set forwarding-options analyzer a input ingress interface 3/0/1
set forwarding-options analyzer a output interface 2/0/2
`)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	raw, _ := json.Marshal(config.ToJSON(tr.Root))
	json.Unmarshal(raw, &m)
	(&standalone{from: "2"}).Rewrite(m)
	raw, _ = json.Marshal(m)
	out, err := config.FromJSON(raw)
	if err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, raw)
	}
	got := config.FormatSet(out)
	for _, want := range []string{
		"set interfaces 1/0/1 unit 0 family ethernet-switching vlan members v10",
		"set interfaces 1/0/2 mtu 9014", "set interfaces 1/0/4 ether-options 802.3ad ae1",
		"set interfaces irb unit 10 family inet address 10.0.0.2/24\n", "set interfaces irb unit 10 family inet address 10.0.0.1/24\n",
		"set interface-range r member 1/0/*", "set interface-range r member */1/*", "member-range 1/0/6 to 1/0/8",
		"set routing-instances blue interface 1/0/2.0", "ingress interface 1/0/1", "output interface 1/0/2",
		"set vlans v10 l3-interface irb.10", "aggregated-ether-options lacp active",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, gone := range []string{"virtual-chassis", "mclag", "3/0/", "10.0.0.3", "member 2", "member 3", "2/0/", "1/0/3", "irb unit 10 family inet address 10.0.0.2/24 member"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q left in:\n%s", gone, got)
		}
	}
}
