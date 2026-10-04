package macsec

import (
	"encoding/binary"
	"testing"

	"github.com/vishvananda/netlink/nl"
)

func u8(v uint8) []byte   { return []byte{v} }
func u32(v uint32) []byte { return nl.Uint32Attr(v) }
func u64(v uint64) []byte { return nl.Uint64Attr(v) }

// nested makes a nested attribute (with the flag the kernel sets on some
// of them: the parser must mask it).
func nested(typ int, children ...*nl.RtAttr) *nl.RtAttr {
	a := nl.NewRtAttr(typ|int(nl.NLA_F_NESTED), nil)
	for _, c := range children {
		a.AddChild(c)
	}
	return a
}

func attr(typ int, v []byte) *nl.RtAttr { return nl.NewRtAttr(typ, v) }

func sa(index int, an uint8, active bool, pn uint64) *nl.RtAttr {
	act := uint8(0)
	if active {
		act = 1
	}
	return nested(index, attr(saAttrAN, u8(an)), attr(saAttrActive, u8(act)), attr(saAttrPN, u64(pn)))
}

// A reply as the kernel sends it for a device with offload "mac", two
// transmit SAs, and two receive SCs whose counters are summed.
func TestParseDevice(t *testing.T) {
	sciTx := binary.BigEndian.AppendUint64(nil, 0x5254001234560001)
	sciRx := binary.BigEndian.AppendUint64(nil, 0x5254009876540001)
	var b []byte
	for _, a := range []*nl.RtAttr{
		attr(attrIfindex, u32(42)),
		nested(attrSecY, attr(secyAttrSCI, sciTx), attr(secyAttrEncodingSA, u8(1))),
		nested(attrOffload, attr(offloadAttrType, u8(2))),
		nested(attrTxSAList, sa(2, 1, true, 900), sa(1, 0, false, 77)),
		nested(attrTxSCStats, attr(1, u64(10)), attr(2, u64(9)), attr(4, u64(1500))),
		nested(attrSecYStats, attr(4, u64(3)), attr(5, u64(2)), attr(6, u64(1))),
		nested(attrRxSCList,
			nested(1, attr(rxscAttrSCI, sciRx), attr(rxscAttrActive, u8(1)),
				nested(rxscAttrSAList, sa(1, 1, true, 50)),
				nested(rxscAttrStats, attr(5, u64(7)), attr(7, u64(1)), attr(9, u64(4)))),
			nested(2, attr(rxscAttrSCI, sciTx), attr(rxscAttrActive, u8(0)),
				nested(rxscAttrStats, attr(5, u64(5))))),
	} {
		b = append(b, a.Serialize()...)
	}
	d, err := parseDevice(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Ifindex != 42 || d.SCI != "5254001234560001" || d.EncodingSA != 1 || d.Offload != "mac" {
		t.Fatalf("device %+v", d)
	}
	if len(d.TxSAs) != 2 || d.TxSAs[0].AN != 0 || d.TxSAs[1] != (SAStatus{AN: 1, Active: true, PN: 900}) {
		t.Fatalf("tx SAs %+v", d.TxSAs)
	}
	if len(d.RxSCs) != 2 || d.RxSCs[0].SCI != "5254009876540001" || !d.RxSCs[0].Active || d.RxSCs[1].Active ||
		len(d.RxSCs[0].SAs) != 1 || d.RxSCs[0].SAs[0].PN != 50 {
		t.Fatalf("rx SCs %+v", d.RxSCs)
	}
	want := map[string]uint64{"OutPktsProtected": 10, "OutPktsEncrypted": 9, "OutOctetsEncrypted": 1500,
		"InPktsNoTag": 3, "InPktsBadTag": 2, "InPktsUnknownSCI": 1, "InPktsOK": 12, "InPktsLate": 1, "InPktsNoSA": 4}
	for k, v := range want {
		if d.Counters[k] != v {
			t.Errorf("%s = %d, want %d", k, d.Counters[k], v)
		}
	}
	if len(d.Counters) != len(want) {
		t.Errorf("counters %v", d.Counters)
	}
}

// A device without offload support in the kernel (no offload attribute)
// is "off"; a reply without an index is an error.
func TestParseDeviceMinimal(t *testing.T) {
	d, err := parseDevice(attr(attrIfindex, u32(7)).Serialize())
	if err != nil || d.Offload != "off" || d.Ifindex != 7 {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := parseDevice(nested(attrSecY).Serialize()); err == nil {
		t.Fatal("no ifindex accepted")
	}
}
