package daemon

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/dataplane"
	"github.com/thxrben/cerium-switchd/lib/conf/config"
	"github.com/thxrben/cerium-switchd/lib/conf/model"
	"github.com/thxrben/cerium-switchd/lib/rib"
)

const ribTestConfig = `
set virtual-chassis member 1 host-name a
set virtual-chassis member 2 host-name b
set system management-instance oob
set interfaces 1/2/0 management
set interfaces cme unit 0 family inet address 10.5.20.76/16
set routing-instances oob interface cme.0
set routing-instances oob routing-options static route 0.0.0.0/0 next-hop 10.5.0.1
set vlans v10 vlan-id 10
set vlans v10 l3-interface irb.10
set interfaces irb unit 10 family inet address 10.1.0.1/24
set interfaces 1/0/0 unit 0 family ethernet-switching vlan members v10
set interfaces 1/0/6 unit 0 family inet dhcp
set routing-options static route 10.9.0.0/16 next-hop 10.1.0.254
set routing-options static route 192.0.2.0/24 discard
`

func ribTestCfg(t *testing.T) *model.Config {
	t.Helper()
	tr, err := config.ParseSet(ribTestConfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg, issues := model.Build(tr, nil)
	if issues.HasErrors() {
		t.Fatal(issues)
	}
	return cfg
}

func hasRoute(rs []rib.Route, p string, proto rib.Protocol) *rib.Route {
	for i, r := range rs {
		if r.Prefix == netip.MustParsePrefix(p) && r.Protocol == proto {
			return &rs[i]
		}
	}
	return nil
}

// The routing table's input: connected, static and lease routes; the
// management instance only on the master (reference 1.8).
func TestRIBConfig(t *testing.T) {
	cfg := ribTestCfg(t)
	linux := func(n string) (string, bool) { return map[string]string{"1/0/6": "eth6"}[n], n == "1/0/6" }
	leases := map[string]dataplane.DHCPLease{"eth6": {Addr: netip.MustParsePrefix("10.6.0.9/24"), Router: netip.MustParseAddr("10.6.0.1")}}
	c := ribConfig(cfg, 1, linux, true, leases)
	def := c.Instances[""]
	if def.Devices["irb.10"] != "irb.10" || def.Devices["1/0/6.0"] != "eth6" {
		t.Errorf("devices %v", def.Devices)
	}
	if hasRoute(def.Routes, "10.1.0.0/24", rib.Direct) == nil || hasRoute(def.Routes, "10.1.0.1/32", rib.Local) == nil {
		t.Errorf("connected routes %+v", def.Routes)
	}
	if r := hasRoute(def.Routes, "10.9.0.0/16", rib.Static); r == nil || r.Preference != rib.PrefStatic || r.NextHops[0].Interface != "irb.10" {
		t.Errorf("static route %+v", r)
	}
	if r := hasRoute(def.Routes, "192.0.2.0/24", rib.Static); r == nil || !r.Discard {
		t.Errorf("discard route %+v", r)
	}
	if r := hasRoute(def.Routes, "0.0.0.0/0", rib.DHCP); r == nil || r.Preference != rib.PrefDHCP || r.NextHops[0].Gateway.String() != "10.6.0.1" ||
		hasRoute(def.Routes, "10.6.0.0/24", rib.Direct) == nil {
		t.Errorf("lease routes %+v", def.Routes)
	}
	oob, ok := c.Instances["oob"]
	if !ok || oob.VRF != "oob" || hasRoute(oob.Routes, "0.0.0.0/0", rib.Static) == nil {
		t.Errorf("management instance on the master: %+v", oob)
	}
	// Another member: no management instance; the default instance stays.
	c = ribConfig(cfg, 2, func(string) (string, bool) { return "", false }, false, nil)
	if _, ok := c.Instances["oob"]; ok {
		t.Error("management instance on a member that is not the master")
	}
	if def := c.Instances[""]; hasRoute(def.Routes, "10.9.0.0/16", rib.Static) == nil || len(def.Devices) != 1 {
		t.Errorf("member 2: %+v", def)
	}
	if !slices.ContainsFunc(c.Instances[""].Routes, func(r rib.Route) bool { return r.Protocol == rib.Static }) {
		t.Error("no static routes on member 2")
	}
}
