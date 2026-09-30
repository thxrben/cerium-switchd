package lldp

import (
	"net"
	"net/netip"
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := &PDU{
		ChassisSubtype: ChassisMAC, ChassisID: []byte{2, 1, 2, 3, 4, 5},
		PortSubtype: PortIfName, PortID: []byte("2/0/3"), TTL: 120,
		PortDesc: "uplink", SysName: "core", SysDesc: "cerOS test",
		Caps: CapBridge | CapRouter, Enabled: CapBridge,
		MgmtAddrs: []netip.Addr{netip.MustParseAddr("10.5.176.95"), netip.MustParseAddr("fd00::95")},
		PVID:      10, Agg: &Agg{Capable: true, Enabled: true, PortID: 7}, MaxFrame: 9014,
	}
	f := p.Frame(net.HardwareAddr{2, 0, 0, 0, 0, 1})
	if len(f) < 60 || f[12] != 0x88 || f[13] != 0xcc {
		t.Fatalf("frame header: % x", f[:14])
	}
	got, err := Parse(f[14:])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, p)
	}
	if s := IDString(got.ChassisSubtype, got.ChassisID, true); s != "02:01:02:03:04:05" {
		t.Errorf("chassis ID: %s", s)
	}
	if s := IDString(got.PortSubtype, got.PortID, false); s != "2/0/3" {
		t.Errorf("port ID: %s", s)
	}
	if s := CapString(got.Caps); s != "Bridge Router" {
		t.Errorf("capabilities: %s", s)
	}
}

func TestShutdownAndErrors(t *testing.T) {
	p := &PDU{ChassisSubtype: ChassisMAC, ChassisID: []byte{2, 0, 0, 0, 0, 9}, PortSubtype: PortIfName, PortID: []byte("1/0/0"), SysName: "x"}
	got, err := Parse(p.Marshal())
	if err != nil || got.TTL != 0 || got.SysName != "" {
		t.Errorf("shutdown LLDPDU: %+v %v", got, err)
	}
	if _, err := Parse([]byte{0x04, 0x01, 5}); err != ErrMandatory {
		t.Errorf("port ID first: %v", err)
	}
	if _, err := Parse([]byte{0x02, 0x07, 4}); err != ErrShort {
		t.Errorf("truncated: %v", err)
	}
}

func FuzzParse(f *testing.F) {
	p := &PDU{ChassisSubtype: ChassisMAC, ChassisID: []byte{2, 0, 0, 0, 0, 9}, PortSubtype: PortIfName, PortID: []byte("1/0/0"), TTL: 120, SysName: "x", PVID: 1}
	f.Add(p.Marshal())
	f.Fuzz(func(t *testing.T, b []byte) {
		if q, err := Parse(b); err == nil {
			if _, err := Parse(q.Marshal()); err != nil {
				t.Fatalf("re-parse of %+v: %v", q, err)
			}
		}
	})
}
