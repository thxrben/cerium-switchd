//go:build teamlab

package netdev

import (
	"testing"

	"github.com/vishvananda/netlink"
)

// Run on a lab switch in a private network namespace:
//
//	unshare -n ./netdev.test -test.run TestTeamLab -test.v
func TestTeamLab(t *testing.T) {
	if err := CreateTeam("tdbg", "layer3+4"); err != nil {
		t.Fatal(err)
	}
	l, _ := netlink.LinkByName("tdbg")
	if p := TeamHashPolicy(l.Attrs().Index); p != "layer3+4" {
		t.Errorf("hash policy read back: %q", p)
	}
	if err := SetTeamHash(l.Attrs().Index, "layer2"); err != nil || TeamHashPolicy(l.Attrs().Index) != "layer2" {
		t.Errorf("hash change: %v %q", err, TeamHashPolicy(l.Attrs().Index))
	}
	// A port (a dummy device) starts disabled and follows SetTeamPort.
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "pdbg"}}); err != nil {
		t.Fatal(err)
	}
	p, _ := netlink.LinkByName("pdbg")
	if err := netlink.LinkSetMaster(p, l); err != nil {
		t.Fatal(err)
	}
	if err := TeamPortInit(l.Attrs().Index, p.Attrs().Index); err != nil {
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
