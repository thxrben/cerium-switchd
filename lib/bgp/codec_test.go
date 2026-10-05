package bgp

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// roundTrip serialises messages and decodes them as a receiver would.
func roundTrip(t *testing.T, ms []*bgp.BGPMessage) []decoded {
	t.Helper()
	var out []decoded
	for _, m := range ms {
		b, err := m.Serialize()
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > maxMsgLen {
			t.Fatalf("message of %d bytes", len(b))
		}
		pm, err := bgp.ParseBGPMessage(b)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, decodeUpdate(pm.Body.(*bgp.BGPUpdate), false))
	}
	return out
}

func TestCodecIPv6LinkLocal(t *testing.T) {
	med := uint32(7)
	a := Attrs{Origin: OriginIncomplete, ASPath: []Segment{{ASNs: []uint32{65001, 4200000000}}, {Set: true, ASNs: []uint32{1, 2}}},
		NextHop: netip.MustParseAddr("2001:db8::1"), LinkLocal: netip.MustParseAddr("fe80::1"), MED: &med,
		Communities: []uint32{65001<<16 | 7}, Large: [][3]uint32{{65001, 1, 2}}}
	ds := roundTrip(t, updateMsgs(IPv6Unicast, &a, true, []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/48")}))
	if len(ds) != 1 || len(ds[0].Reach) != 1 {
		t.Fatalf("decoded %+v", ds)
	}
	r, got := ds[0].Reach[0], ds[0].Attrs
	if r.Prefix != netip.MustParsePrefix("2001:db8:1::/48") || r.NextHop != a.NextHop || r.LinkLocal != a.LinkLocal {
		t.Fatalf("reach %+v", r)
	}
	if got.PathString() != "65001 4200000000 {1 2} ?" || *got.MED != 7 || !slices.Equal(got.Communities, a.Communities) ||
		!slices.Equal(got.Large, a.Large) {
		t.Fatalf("attrs %+v (%s)", got, got.PathString())
	}
	// Withdrawal.
	ds = roundTrip(t, withdrawMsgs(IPv6Unicast, []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/48")}))
	if len(ds[0].Withdrawn) != 1 || ds[0].Withdrawn[0] != netip.MustParsePrefix("2001:db8:1::/48") {
		t.Fatalf("withdrawn %+v", ds[0])
	}
}

// A neighbour without 4-byte AS support gets AS_TRANS and AS4_PATH; the
// receiver rebuilds the path.
func TestCodecAS4ForTwoByteNeighbor(t *testing.T) {
	a := Attrs{ASPath: []Segment{{ASNs: []uint32{65010, 4200000001, 65020}}}, NextHop: netip.MustParseAddr("10.0.0.1")}
	ds := roundTrip(t, updateMsgs(IPv4Unicast, &a, false, []netip.Prefix{netip.MustParsePrefix("10.9.0.0/16")}))
	d := ds[0]
	if (&d.Attrs).PathString() != fmt.Sprintf("65010 %d 65020 I", asTrans) {
		t.Fatalf("AS_PATH %s", (&d.Attrs).PathString())
	}
	merged := Attrs{ASPath: mergeAS4(d.Attrs.ASPath, d.as4Path)}
	if merged.PathString() != "65010 4200000001 65020 I" {
		t.Fatalf("merged %s", merged.PathString())
	}
	// A 2-byte speaker in front prepended its own AS without AS4_PATH.
	path := []Segment{{ASNs: []uint32{65099, 65010, asTrans, 65020}}}
	merged = Attrs{ASPath: mergeAS4(path, d.as4Path)}
	if merged.PathString() != "65099 65010 4200000001 65020 I" {
		t.Fatalf("merged behind a 2-byte speaker %s", merged.PathString())
	}
}

// Many prefixes are split into messages of at most 4096 bytes.
func TestCodecChunks(t *testing.T) {
	var ps []netip.Prefix
	for i := range 2000 {
		ps = append(ps, netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 0}), 24))
	}
	a := Attrs{ASPath: []Segment{{ASNs: []uint32{65001}}}, NextHop: netip.MustParseAddr("10.0.0.1")}
	ms := updateMsgs(IPv4Unicast, &a, true, ps)
	n := 0
	for _, d := range roundTrip(t, ms) {
		n += len(d.Reach)
	}
	if n != 2000 || len(ms) < 2 {
		t.Fatalf("%d prefixes in %d messages", n, len(ms))
	}
	var ws []*bgp.BGPMessage
	ws = withdrawMsgs(IPv4Unicast, ps)
	n = 0
	for _, d := range roundTrip(t, ws) {
		n += len(d.Withdrawn)
	}
	if n != 2000 {
		t.Fatalf("%d withdrawn", n)
	}
}

// FuzzUpdate: any UPDATE body is parsed and decoded without a panic (a
// neighbour's malformed message must never crash the speaker).
func FuzzUpdate(f *testing.F) {
	a := Attrs{ASPath: []Segment{{ASNs: []uint32{65001}}}, NextHop: netip.MustParseAddr("10.0.0.1"), Communities: []uint32{1}}
	for _, m := range updateMsgs(IPv4Unicast, &a, true, []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}) {
		b, _ := m.Serialize()
		f.Add(b[bgp.BGP_HEADER_LENGTH:])
	}
	a6 := Attrs{NextHop: netip.MustParseAddr("2001:db8::1"), LinkLocal: netip.MustParseAddr("fe80::1")}
	for _, m := range updateMsgs(IPv6Unicast, &a6, false, []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}) {
		b, _ := m.Serialize()
		f.Add(b[bgp.BGP_HEADER_LENGTH:])
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		h := &bgp.BGPHeader{Type: bgp.BGP_MSG_UPDATE, Len: uint16(len(body) + bgp.BGP_HEADER_LENGTH)}
		m, err := bgp.ParseBGPBody(h, body)
		if m == nil {
			return
		}
		u, ok := m.Body.(*bgp.BGPUpdate)
		if !ok {
			return
		}
		withdraw := false
		if err != nil {
			w, _, notify := errorHandling(err)
			if notify != nil {
				return
			}
			withdraw = w
		}
		d := decodeUpdate(u, withdraw)
		_ = mergeAS4(d.Attrs.ASPath, d.as4Path)
	})
}
