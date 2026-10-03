package dataplane

import (
	"net"
	"slices"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/model"
)

const puntConfig = `
set virtual-chassis member 1 host-name a
set virtual-chassis member 2 host-name b
set vlans v10 vlan-id 10
set vlans v10 l3-interface irb.10
set vlans v20 vlan-id 20
set vlans v20 l3-interface irb.20
set vlans v30 vlan-id 30
set interfaces irb unit 10 family inet address 10.0.10.1/24
set interfaces irb unit 20 family inet6 address 2001:db8:20::1/64
set interfaces 1/0/1 unit 0 family ethernet-switching vlan members v10
set interfaces 2/0/1 unit 0 family ethernet-switching vlan members v10
set interfaces 2/0/2 unit 0 family ethernet-switching interface-mode trunk
set interfaces 2/0/2 unit 0 family ethernet-switching vlan members [ v10 v20 v30 ]
set interfaces 2/0/3 unit 0 family ethernet-switching vlan members v30
set interfaces ae1 aggregated-ether-options lacp active
set interfaces ae1 unit 0 family ethernet-switching interface-mode trunk
set interfaces ae1 unit 0 family ethernet-switching vlan members [ v20 v30 ]
set interfaces 1/0/5 ether-options 802.3ad ae1
set interfaces 2/0/5 ether-options 802.3ad ae1
`

// Member 2 passes the protocol frames of its irb VLANs to the master,
// member 1: per port the VLANs that have an irb (tagged), and the access
// VLAN of untagged frames; nothing on the master, nothing for VLAN 30.
func TestComputePunt(t *testing.T) {
	tr, err := config.ParseSet(puntConfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg, issues := model.Build(tr, nil)
	if issues.HasErrors() {
		t.Fatal(issues)
	}
	gw := net.HardwareAddr{0x02, 0, 0, 0, 0, 1}
	s, _ := Compute(cfg, 2, linuxNames)
	p := ComputePunt(cfg, s, 2, 1, gw)
	if p == nil || p.Tunnel != TunnelName(1) {
		t.Fatalf("punt %+v", p)
	}
	byName := map[string]PuntPort{}
	for _, pp := range p.Ports {
		byName[pp.Name] = pp
	}
	if pp := byName["lx201"]; pp.PVID != 10 || len(pp.VLANs) != 0 {
		t.Errorf("access port: %+v", pp)
	}
	if pp := byName["lx202"]; pp.PVID != 0 || !slices.Equal(pp.VLANs, []int{10, 20}) {
		t.Errorf("trunk: %+v", pp)
	}
	if pp, ok := byName["ae1"]; !ok || !slices.Equal(pp.VLANs, []int{20}) {
		t.Errorf("MC-LAG bundle: %+v", pp)
	}
	if _, ok := byName["lx203"]; ok {
		t.Error("a port without irb VLANs passes frames")
	}
	if ComputePunt(cfg, s, 2, 2, gw) != nil || ComputePunt(cfg, s, 2, 0, gw) != nil {
		t.Error("redirection on the master or without one")
	}
}
