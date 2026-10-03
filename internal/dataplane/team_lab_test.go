//go:build teamlab

package dataplane

import (
	"github.com/thxrben/cerium-switchd/pkg/netdev"
	"testing"

	"github.com/vishvananda/netlink"
)

// Run on a lab switch in a private network namespace:
//
//	unshare -n ./dataplane.test -test.run TestTeamLab -test.v
func TestTeamLab(t *testing.T) {
	if err := netdev.CreateTeam("tdbg", "layer3+4"); err != nil {
		t.Fatal(err)
	}
	l, _ := netlink.LinkByName("tdbg")
	if p := netdev.TeamHashPolicy(l.Attrs().Index); p != "layer3+4" {
		t.Errorf("hash policy read back: %q", p)
	}
	if err := netdev.SetTeamHash(l.Attrs().Index, "layer2"); err != nil || netdev.TeamHashPolicy(l.Attrs().Index) != "layer2" {
		t.Errorf("hash change: %v %q", err, netdev.TeamHashPolicy(l.Attrs().Index))
	}
	// A port (a dummy device) starts disabled and follows SetTeamPort.
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "pdbg"}}); err != nil {
		t.Fatal(err)
	}
	p, _ := netlink.LinkByName("pdbg")
	if err := netlink.LinkSetMaster(p, l); err != nil {
		t.Fatal(err)
	}
	if err := netdev.TeamPortInit(l.Attrs().Index, p.Attrs().Index); err != nil {
		t.Fatal(err)
	}
	en, err := TeamPortsEnabled("tdbg")
	if err != nil || en["pdbg"] {
		t.Fatalf("new port enabled: %v %v", en, err)
	}
	netlink.LinkSetUp(p)
	netlink.LinkSetUp(l)
	carrier := func() bool {
		x, _ := netlink.LinkByName("tdbg")
		return x.Attrs().OperState == netlink.OperUp
	}
	if carrier() {
		t.Error("team up without a distributing port")
	}
	if err := SetTeamPort("tdbg", "pdbg", true); err != nil {
		t.Fatal(err)
	}
	if en, _ := TeamPortsEnabled("tdbg"); !en["pdbg"] || !carrier() {
		t.Errorf("after enabling: %v carrier %v", en, carrier())
	}
	if err := SetTeamPort("tdbg", "pdbg", false); err != nil {
		t.Fatal(err)
	}
	if en, _ := TeamPortsEnabled("tdbg"); en["pdbg"] || carrier() {
		t.Errorf("after disabling: %v carrier %v", en, carrier())
	}
}
