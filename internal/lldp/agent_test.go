package lldp

import (
	"fmt"
	"testing"
	"time"
)

func TestNeighborTable(t *testing.T) {
	var tab table
	now := time.Unix(1000, 0)
	d := func(chassis byte, port string, ttl uint16) *PDU {
		return &PDU{ChassisSubtype: ChassisMAC, ChassisID: []byte{2, 0, 0, 0, 0, chassis}, PortSubtype: PortIfName, PortID: []byte(port), TTL: ttl, SysName: "n"}
	}
	if ch, _ := tab.learn("1/0/0", d(1, "eth0", 120), now); !ch {
		t.Fatal("first neighbour not learned")
	}
	tab.learn("1/0/0", d(1, "eth0", 120), now.Add(10*time.Second)) // refresh
	if ns := tab.list(); len(ns) != 1 || ns[0].Chassis != "02:00:00:00:00:01" || !ns[0].FirstSeen.Equal(now) {
		t.Fatalf("after refresh: %+v", ns)
	}
	// A shutdown LLDPDU removes it at once.
	tab.learn("1/0/0", d(1, "eth0", 0), now)
	if len(tab.list()) != 0 {
		t.Fatal("shutdown LLDPDU did not remove the neighbour")
	}
	// At most MaxNeighbors per port.
	for i := 0; i < MaxNeighbors+2; i++ {
		_, disc := tab.learn("1/0/1", d(byte(i), fmt.Sprint(i), 30), now)
		if disc != (i >= MaxNeighbors) {
			t.Errorf("neighbour %d: discarded %v", i, disc)
		}
	}
	// Aging.
	aged := tab.age(now.Add(31 * time.Second))
	if aged["1/0/1"] != MaxNeighbors || len(tab.list()) != 0 {
		t.Errorf("aged %v, left %d", aged, len(tab.list()))
	}
}

func TestPortPDU(t *testing.T) {
	s := &System{ChassisMAC: []byte{2, 9, 9, 9, 9, 9}, Name: "core", Desc: "cerOS x", Caps: CapBridge, Enabled: CapBridge, Interval: 30, Hold: 4}
	p := s.pdu(PortSpec{Name: "3/0/1", Bundle: "ae1", InBundle: true, BundleIndex: 42, MaxFrame: 9014}, s.TTL())
	if p.TTL != 120 || string(p.PortID) != "3/0/1" || p.PortDesc != "3/0/1" || p.SysName != "core" || p.Agg == nil || !p.Agg.Enabled || p.Agg.PortID != 42 {
		t.Errorf("port LLDPDU: %+v", p)
	}
}
