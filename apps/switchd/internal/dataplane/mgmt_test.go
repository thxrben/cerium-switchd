package dataplane

import (
	"slices"
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/lib/conf/config"
	"github.com/thxrben/cerium-switchd/lib/conf/model"
)

const mgmtConfig = `
set virtual-chassis member 1 host-name a
set virtual-chassis member 2 host-name b
set system management-instance oob
set interfaces 1/2/0 management
set interfaces 1/2/1 management
set interfaces 2/2/0 management
set interfaces cme unit 0 family inet address 10.5.20.76/16
set interfaces cme unit 0 family inet6 address fd00::76/64
set routing-instances oob interface cme.0
set routing-instances oob interface irb.99
set routing-instances oob routing-options static route 0.0.0.0/0 next-hop 10.5.0.1
set vlans mgmt vlan-id 99
set vlans mgmt l3-interface irb.99
set interfaces irb unit 99 family inet address 192.168.99.1/24
set interfaces 1/0/0 unit 0 family ethernet-switching vlan members mgmt
set interfaces 2/0/0 unit 0 family ethernet-switching vlan members mgmt
set routing-options static route 10.9.0.0/16 discard
`

func mgmtCfg(t *testing.T) *model.Config {
	t.Helper()
	tr, err := config.ParseSet(mgmtConfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg, issues := model.Build(tr, nil)
	if issues.HasErrors() {
		t.Fatal(issues)
	}
	return cfg
}

func linuxNames(name string) (string, bool) {
	return "lx" + strings.ReplaceAll(name, "/", ""), true
}

// The management address and in-band addresses
// exist only on the master; cme goes onto the first management port with a
// link (reference 1.8).
func TestManagementRole(t *testing.T) {
	cfg := mgmtCfg(t)
	mac := CMEMAC("stack")
	carrier := map[string]bool{"lx121": true}
	has := func(linux string) bool { return carrier[linux] }

	s, _ := Compute(cfg, 1, linuxNames)
	Management(s, cfg, 1, linuxNames, true, has, mac, []string{"lx150"})
	c := s.L3.CME
	if c == nil || c.Parent != "lx121" || c.VRF != "oob" || !c.Up || len(c.Addrs) != 2 || c.MAC.String() != mac.String() {
		t.Fatalf("cme on the master: %+v", c)
	}
	if !slices.Equal(s.L3.Bare, []string{"lx120", "lx121", "lx150"}) {
		t.Errorf("bare ports: %v", s.L3.Bare)
	}
	if !slices.ContainsFunc(s.L3.Ifs, func(i L3If) bool { return i.Name == "irb.99" }) {
		t.Errorf("master lacks the in-band address: %+v", s.L3)
	}
	for _, i := range s.L3.Ifs {
		if i.Name == CMEName {
			t.Error("cme listed as an ordinary routed interface")
		}
	}
	// Management ports are plain, up ports outside the bridge.
	if l := s.Links["lx120"]; l == nil || !l.Up || l.Master != "" {
		t.Errorf("management port: %+v", l)
	}

	// The first port by name that has a link wins.
	carrier["lx120"] = true
	s, _ = Compute(cfg, 1, linuxNames)
	Management(s, cfg, 1, linuxNames, true, has, mac, nil)
	if s.L3.CME == nil || s.L3.CME.Parent != "lx120" {
		t.Errorf("cme not on the first port with a link: %+v", s.L3.CME)
	}

	// No link on any management port: no cme, even on the master.
	s, _ = Compute(cfg, 1, linuxNames)
	Management(s, cfg, 1, linuxNames, true, func(string) bool { return false }, mac, nil)
	if s.L3.CME != nil {
		t.Errorf("cme without a link: %+v", s.L3.CME)
	}

	// Another member: no cme, no management addresses (the instance's
	// routes: cer-ribd's configuration, daemon.ribConfig).
	s, _ = Compute(cfg, 2, linuxNames)
	Management(s, cfg, 2, linuxNames, false, func(string) bool { return true }, mac, nil)
	if s.L3.CME != nil {
		t.Errorf("cme on a member that is not the master: %+v", s.L3.CME)
	}
	for _, i := range s.L3.Ifs {
		if i.VRF == "oob" {
			t.Errorf("management interface on a member that is not the master: %+v", i)
		}
	}
	if !slices.Equal(s.L3.Bare, []string{"lx220"}) {
		t.Errorf("bare ports of member 2: %v", s.L3.Bare)
	}

	// A disabled management port is never used.
	tr, _ := config.ParseSet(mgmtConfig + "set interfaces 1/2/0 disable\n")
	cfg2, _ := model.Build(tr, nil)
	s, _ = Compute(cfg2, 1, linuxNames)
	Management(s, cfg2, 1, linuxNames, true, has, mac, nil)
	if s.L3.CME == nil || s.L3.CME.Parent != "lx121" {
		t.Errorf("cme on a disabled port: %+v", s.L3.CME)
	}
}

func TestCMEMAC(t *testing.T) {
	a, b := CMEMAC("x"), CMEMAC("y")
	if len(a) != 6 || a[0]&3 != 2 || a.String() == b.String() || a.String() == GatewayMAC("x").String() {
		t.Errorf("cme MACs: %s %s (gateway %s)", a, b, GatewayMAC("x"))
	}
}
