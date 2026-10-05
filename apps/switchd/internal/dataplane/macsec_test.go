package dataplane

import (
	"slices"
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/lib/conf/model"
)

// A port secured with MACsec (reference 5.15): before MKA secured the link
// it carries nothing (out of the bridge, mtu + 32); the MACsec device that
// MKA makes takes its place in the bridge, with its VLANs and limits.
func TestComputeSecuredPort(t *testing.T) {
	ca := &model.MACsecCA{Name: "ca1", Cipher: "gcm-aes-128"}
	cfg := &model.Config{Interfaces: map[string]*model.Interface{
		"1/0/0": {Name: "1/0/0", Member: 1, MTU: 1514, Switching: true, Mode: "access", AccessVLAN: 10, VLANs: []int{10}, MACLimit: 5},
		"1/0/1": {Name: "1/0/1", Member: 1, MTU: 1514, Switching: true, Mode: "access", AccessVLAN: 10, VLANs: []int{10}},
	}, MACsec: model.MACsec{CAs: map[string]*model.MACsecCA{"ca1": ca}, Ports: map[string]string{"1/0/0": "ca1"}}}

	// The port was an ordinary switch port; MACsec is configured, MKA has
	// not secured it yet.
	k := NewFake(&State{Links: map[string]*Link{
		"eth0": {Name: "eth0", Kind: Physical, MTU: 1500, MaxMTU: 9000, Present: true},
		"eth1": {Name: "eth1", Kind: Physical, MTU: 1500, MaxMTU: 9000, Present: true},
	}})
	plainCfg := &model.Config{Interfaces: cfg.Interfaces}
	before, _ := Compute(plainCfg, 1, testNames)
	cur, _ := k.Read()
	if err := Execute(k, Plan(cur, before, nil)); err != nil {
		t.Fatal(err)
	}
	unsecured, _ := ComputeSecured(cfg, 1, testNames, nil)
	if p := unsecured.Links["eth0"]; p.Master != "" || p.MTU != 1532 || !p.Up || len(p.VLANs) != 0 {
		t.Fatalf("unsecured port: %s", p)
	}
	if !slices.Contains(unsecured.L3.NoIP, "eth0") {
		t.Fatal("the secured port's own link must carry no IP")
	}
	cur, _ = k.Read()
	ops := Plan(cur, unsecured, names(before))
	if err := Execute(k, ops); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(FormatPlan(ops), "eth1") {
		t.Fatalf("an unrelated port was touched:\n%s", FormatPlan(ops))
	}
	checkConverged(t, k, unsecured)

	// MKA secured the link: wpa_supplicant made macsec0 on eth0.
	k.S.Links["macsec0"] = &Link{Name: "macsec0", Kind: SecPort, Parent: "eth0", MTU: 1500, Present: true}
	cur, _ = k.Read()
	secured, _ := ComputeSecured(cfg, 1, testNames, SecDevices(cur))
	d := secured.Links["macsec0"]
	if d == nil || d.Master != BridgeName || d.MTU != 1500 || d.MaxLearned != 5 || !d.VLANs[10].PVID || !d.Up {
		t.Fatalf("MACsec device: %v", d)
	}
	ops = Plan(cur, secured, names(unsecured))
	if err := Execute(k, ops); err != nil {
		t.Fatalf("%v\n%s", err, FormatPlan(ops))
	}
	checkConverged(t, k, secured)
	if strings.Contains(FormatPlan(ops), "eth0") || strings.Contains(FormatPlan(ops), "eth1") {
		t.Fatalf("the ports were touched when the device appeared:\n%s", FormatPlan(ops))
	}

	// The data names lead to the device (RSTP, mirroring, routes, ...).
	data := DataNames(cfg, testNames, SecDevices(cur))
	if dev, ok := data("1/0/0"); !ok || dev != "macsec0" {
		t.Fatalf("data name %q %v", dev, ok)
	}
	if dev, ok := data("1/0/1"); !ok || dev != "eth1" {
		t.Fatalf("plain port %q %v", dev, ok)
	}
	if _, ok := DataNames(cfg, testNames, nil)("1/0/0"); ok {
		t.Fatal("an unsecured port has no data device")
	}

	// MKA lost the link (device gone): the port carries nothing again.
	delete(k.S.Links, "macsec0")
	cur, _ = k.Read()
	again, _ := ComputeSecured(cfg, 1, testNames, SecDevices(cur))
	if ops := Plan(cur, again, names(secured)); len(ops) != 0 {
		t.Fatalf("nothing to do when the device went:\n%s", FormatPlan(ops))
	}
}

// A secured member of an LACP bundle: before MKA it is in no bundle (mtu of
// the bundle + 32); its MACsec device then joins the team, the port itself
// stays outside it.
func TestComputeSecuredBundleMember(t *testing.T) {
	ca := &model.MACsecCA{Name: "ca1", Cipher: "gcm-aes-128"}
	cfg := &model.Config{Interfaces: map[string]*model.Interface{
		"ae1":   {Name: "ae1", AE: true, MemberIDs: []int{1}, MTU: 9014, LACP: &model.LACP{Active: true}, Switching: true, Mode: "trunk", VLANs: []int{10}},
		"1/0/0": {Name: "1/0/0", Member: 1, MTU: 1514, Parent: "ae1"},
		"1/0/1": {Name: "1/0/1", Member: 1, MTU: 1514, Parent: "ae1"},
	}, MACsec: model.MACsec{CAs: map[string]*model.MACsecCA{"ca1": ca}, Ports: map[string]string{"1/0/0": "ca1", "1/0/1": "ca1"}}}
	unsecured, _ := ComputeSecured(cfg, 1, testNames, nil)
	if p := unsecured.Links["eth0"]; p.Master != "" || p.MTU != 9000+32 {
		t.Fatalf("unsecured member: %s", p)
	}
	secured, _ := ComputeSecured(cfg, 1, testNames, map[string]string{"eth0": "macsec0"})
	if d := secured.Links["macsec0"]; d == nil || d.Master != "ae1" || d.MTU != 9000 || d.Kind != SecPort {
		t.Fatalf("MACsec device: %v", d)
	}
	if p := secured.Links["eth0"]; p.Master != "" || p.MTU != 9032 {
		t.Fatalf("the port joined the bundle itself: %s", p)
	}
	if p := secured.Links["eth1"]; p.Master != "" {
		t.Fatalf("the unsecured member joined: %s", p)
	}
}
