package ospf

import (
	"bytes"
	"encoding/hex"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	rid1 = ID(0x01010101)
	rid2 = ID(0x02020202)
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func ip(s string) netip.Addr    { return netip.MustParseAddr(s) }

func TestFletcher(t *testing.T) {
	l := (&LSA{V: V2, LSAHeader: LSAHeader{Age: 1, Options: OptE, Type: V2Router, ID: rid1, AdvRtr: rid1, Seq: InitialSeq},
		Links: []RouterLink{{ID: IDFrom(ip("10.0.0.0")), Data: IDFrom(Mask(24)), Type: LinkStub, Metric: 10}}}).Encode()
	if !lsaChecksumOK(l.Raw) {
		t.Fatalf("checksum does not verify: % x", l.Raw)
	}
	// Cross-check with an independent implementation of RFC 1008 §7.2.
	if want := rfc1008(l.Raw[2:], 15); l.Checksum != want {
		t.Fatalf("checksum %04x, RFC 1008 algorithm %04x", l.Checksum, want)
	}
	if !lsaChecksumOK(l.WithAge(1234).Raw) {
		t.Fatal("age changes the checksum")
	}
	bad := bytes.Clone(l.Raw)
	bad[25] ^= 1
	if _, _, err := ParseLSA(V2, bad); err != ErrLSAChecksum {
		t.Fatalf("corrupted LSA parsed: %v", err)
	}
	for n := range 300 {
		l := (&LSA{V: V3, LSAHeader: LSAHeader{Type: V3InterAreaPrefix, ID: ID(n), AdvRtr: rid2, Seq: int32(n)},
			Prefix: netip.PrefixFrom(ip("2001:db8::"), n%129).Masked(), Metric: uint32(n * 7919)}).Encode()
		if !lsaChecksumOK(l.Raw) || l.Checksum != rfc1008(l.Raw[2:], 15) {
			t.Fatalf("LSA %d: checksum %04x", n, l.Checksum)
		}
	}
}

// rfc1008 is the checksum as written in RFC 1008 §7.2 (k: the 1-based
// position of the first checksum byte).
func rfc1008(b []byte, k int) uint16 {
	b = bytes.Clone(b)
	b[k-1], b[k] = 0, 0
	c0, c1 := 0, 0
	for _, v := range b {
		c0 += int(v)
		c1 += c0
	}
	c0 %= 255
	c1 %= 255
	l := len(b)
	x := ((l-k)*c0 - c1) % 255
	y := (c1 - (l-k+1)*c0) % 255
	if x < 0 {
		x += 255
	}
	if y < 0 {
		y += 255
	}
	if x == 0 {
		x = 255
	}
	if y == 0 {
		y = 255
	}
	return uint16(x)<<8 | uint16(y)
}

func testLSAs(v Version) []*LSA {
	if v == V2 {
		return []*LSA{
			{V: V2, LSAHeader: LSAHeader{Options: OptE, Type: V2Router, ID: rid1, AdvRtr: rid1, Seq: InitialSeq}, Flags: FlagB | FlagE,
				Links: []RouterLink{
					{ID: rid2, Data: IDFrom(ip("10.0.0.1")), Type: LinkP2P, Metric: 1},
					{ID: IDFrom(ip("10.0.0.0")), Data: IDFrom(Mask(30)), Type: LinkStub, Metric: 1},
					{ID: IDFrom(ip("10.1.0.2")), Data: IDFrom(ip("10.1.0.1")), Type: LinkTransit, Metric: 10},
				}},
			{V: V2, LSAHeader: LSAHeader{Options: OptE, Type: V2Network, ID: IDFrom(ip("10.1.0.2")), AdvRtr: rid2, Seq: 5},
				MaskBits: 24, Attached: []ID{rid1, rid2}},
			{V: V2, LSAHeader: LSAHeader{Type: V2Summary, ID: IDFrom(ip("10.2.0.0")), AdvRtr: rid1, Seq: 1},
				Prefix: pfx("10.2.0.0/16"), Metric: 20},
			{V: V2, LSAHeader: LSAHeader{Type: V2ASBRSummary, ID: rid2, AdvRtr: rid1, Seq: 1}, DestRouter: rid2, Metric: 30},
			{V: V2, LSAHeader: LSAHeader{Age: 7, Options: OptE, Type: V2External, ID: 0, AdvRtr: rid1, Seq: 1},
				Prefix: pfx("0.0.0.0/0"), E2: true, Metric: 1, Tag: 7, HasTag: true},
			{V: V2, LSAHeader: LSAHeader{Options: OptE, Type: V2External, ID: IDFrom(ip("192.0.2.0")), AdvRtr: rid1, Seq: 1},
				Prefix: pfx("192.0.2.0/24"), Metric: 5, Forward: ip("10.0.0.9"), HasTag: true},
		}
	}
	return []*LSA{
		{V: V3, LSAHeader: LSAHeader{Type: V3Router, ID: 0, AdvRtr: rid1, Seq: InitialSeq}, Flags: FlagB | FlagE,
			Options: OptV6 | OptE | OptR,
			Links: []RouterLink{
				{Type: LinkP2P, Metric: 1, IfID: 5, NbrIfID: 7, NbrRouter: rid2},
				{Type: LinkTransit, Metric: 10, IfID: 6, NbrIfID: 3, NbrRouter: rid2},
			}},
		{V: V3, LSAHeader: LSAHeader{Type: V3Network, ID: 3, AdvRtr: rid2, Seq: 5}, Options: OptV6 | OptE | OptR,
			Attached: []ID{rid2, rid1}},
		{V: V3, LSAHeader: LSAHeader{Type: V3InterAreaPrefix, ID: 1, AdvRtr: rid1, Seq: 1}, Prefix: pfx("2001:db8:2::/48"), Metric: 20},
		{V: V3, LSAHeader: LSAHeader{Type: V3InterAreaRouter, ID: 2, AdvRtr: rid1, Seq: 1}, Options: OptV6 | OptE | OptR,
			DestRouter: rid2, Metric: 30},
		{V: V3, LSAHeader: LSAHeader{Age: 7, Type: V3External, ID: 1, AdvRtr: rid1, Seq: 1}, Prefix: pfx("::/0"), E2: true, Metric: 1},
		{V: V3, LSAHeader: LSAHeader{Type: V3External, ID: 2, AdvRtr: rid1, Seq: 1}, Prefix: pfx("2001:db8:ff::/64"), Metric: 7,
			Forward: ip("2001:db8::9"), Tag: 42, HasTag: true},
		{V: V3, LSAHeader: LSAHeader{Type: V3Link, ID: 5, AdvRtr: rid1, Seq: 1}, Priority: 1, Options: OptV6 | OptE | OptR,
			LinkLocal: ip("fe80::1"), Prefixes: []PrefixEntry{{Prefix: pfx("2001:db8:1::/64")}, {Prefix: pfx("2001:db8:1:1::/127")}}},
		{V: V3, LSAHeader: LSAHeader{Type: V3IntraAreaPrefix, ID: 0, AdvRtr: rid1, Seq: 1}, RefType: V3Router, RefAdvRtr: rid1,
			Prefixes: []PrefixEntry{{Prefix: pfx("2001:db8:9::1/128"), Options: PrefixLA}, {Prefix: pfx("2001:db8:a::/64"), Metric: 10}}},
	}
}

func TestLSARoundTrip(t *testing.T) {
	for _, v := range []Version{V2, V3} {
		for _, l := range testLSAs(v) {
			l.Encode()
			q, n, err := ParseLSA(v, append(bytes.Clone(l.Raw), 0xde, 0xad))
			if err != nil || n != len(l.Raw) {
				t.Fatalf("v%d %s: %v (%d of %d bytes)", v, v.TypeName(l.Type), err, n, len(l.Raw))
			}
			if !reflect.DeepEqual(q, l) {
				t.Fatalf("v%d %s:\n got %+v\nwant %+v", v, v.TypeName(l.Type), q, l)
			}
		}
	}
}

func testPackets(v Version) []*Packet {
	lsas := testLSAs(v)
	var hdrs []LSAHeader
	for _, l := range lsas {
		l.Encode()
		hdrs = append(hdrs, l.LSAHeader)
	}
	hello := &Hello{MaskBits: 24, Interval: 10, Options: OptE, Priority: 1, Dead: 40,
		DR: IDFrom(ip("10.1.0.2")), BDR: IDFrom(ip("10.1.0.1")), Neighbors: []ID{rid2}}
	if v == V3 {
		hello = &Hello{InterfaceID: 5, Interval: 10, Options: OptV6 | OptE | OptR, Priority: 1, Dead: 40,
			DR: rid2, BDR: rid1, Neighbors: []ID{rid2}}
	}
	pkts := []*Packet{
		{Type: TypeHello, Hello: hello},
		{Type: TypeDD, DD: &DD{MTU: 1500, Options: OptE, Flags: DDInit | DDMore | DDMaster, Seq: 4711}},
		{Type: TypeDD, DD: &DD{MTU: 9000, Options: OptE, Flags: DDMore, Seq: 4712, Headers: hdrs}},
		{Type: TypeLSR, LSR: []LSRef{{Type: lsas[0].Type, ID: rid1, AdvRtr: rid1}, {Type: lsas[4].Type, ID: 0, AdvRtr: rid2}}},
		{Type: TypeLSU, LSU: lsas},
		{Type: TypeLSAck, Ack: hdrs},
	}
	for _, p := range pkts {
		p.V, p.RouterID, p.AreaID = v, rid1, Backbone
		if v == V3 {
			p.InstanceID = 0
		}
	}
	return pkts
}

func TestPacketRoundTrip(t *testing.T) {
	auths := map[Version][]*Auth{V2: {nil, {Type: AuthSimple, Simple: "secret"},
		{Type: AuthCrypto, Keys: map[uint8]string{1: "old", 2: "new key"}, SendKey: 2}}, V3: {nil}}
	for _, v := range []Version{V2, V3} {
		for _, auth := range auths[v] {
			for _, p := range testPackets(v) {
				p.CryptoSeq = 99
				raw := p.Encode(auth)
				q, err := Decode(v, raw)
				if err != nil {
					t.Fatalf("v%d %s %v: %v", v, TypeName(p.Type), auth, err)
				}
				if !auth.Verify(raw, q) {
					t.Fatalf("v%d %s %v: authentication does not verify", v, TypeName(p.Type), auth)
				}
				q.AuType, q.KeyID, q.CryptoSeq, q.authField, q.digest = p.AuType, p.KeyID, p.CryptoSeq, p.authField, p.digest
				if !reflect.DeepEqual(q, p) {
					t.Fatalf("v%d %s:\n got %+v\nwant %+v", v, TypeName(p.Type), q, p)
				}
			}
		}
	}
	if _, err := Decode(V3, testPackets(V2)[0].Encode(nil)); err != ErrVersion {
		t.Fatalf("v2 packet as v3: %v", err)
	}
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// OSPFv3 field placement against RFC 5340 A.3.1/A.3.2, written byte by
// byte (round trips cannot catch a field the encoder and decoder place
// wrongly in the same way).
func TestV3HelloLayout(t *testing.T) {
	p := &Packet{V: V3, Type: TypeHello, RouterID: rid1, AreaID: 1, InstanceID: 3, Hello: &Hello{
		InterfaceID: 0x0a0b0c0d, Priority: 7, Options: 0x000013, Interval: 10, Dead: 40, DR: rid2, BDR: rid1,
		Neighbors: []ID{rid2}}}
	want := unhex(t, `
		03 01 0028  01010101  00000001  0000 03 00
		0a0b0c0d  07 000013  000a 0028  02020202  01010101  02020202`)
	got := p.Encode(nil)
	if !bytes.Equal(got, want) {
		t.Fatalf("hello\n got % x\nwant % x", got, want)
	}
	if q, err := Decode(V3, want); err != nil || !reflect.DeepEqual(q, p) {
		t.Fatalf("decode: %+v %v", q, err)
	}
}

func TestV3RouterLSALayout(t *testing.T) {
	l := (&LSA{V: V3, LSAHeader: LSAHeader{Age: 1, Type: V3Router, ID: 0, AdvRtr: rid1, Seq: InitialSeq},
		Flags: FlagE, Options: 0x000013,
		Links: []RouterLink{{Type: LinkTransit, Metric: 10, IfID: 5, NbrIfID: 6, NbrRouter: rid2}}}).Encode()
	want := unhex(t, `
		0001 2001 00000000 01010101 80000001 0000 0028
		02 000013
		02 00 000a 00000005 00000006 02020202`)
	// The checksum is computed; compare without it.
	got := bytes.Clone(l.Raw)
	got[16], got[17] = 0, 0
	if !bytes.Equal(got, want) {
		t.Fatalf("router LSA\n got % x\nwant % x", got, want)
	}
	// A prefix is padded to 32-bit words: /64 is 8 bytes, /0 none, /127 16.
	for bits, n := range map[int]int{0: 4, 1: 8, 32: 8, 33: 12, 64: 12, 127: 20, 128: 20} {
		if b := appendPrefix(nil, netip.PrefixFrom(ip("2001:db8::"), bits).Masked(), 0, 0); len(b) != n {
			t.Errorf("/%d: %d bytes, want %d", bits, len(b), n)
		}
	}
}

func TestChecksum6(t *testing.T) {
	p := testPackets(V3)[0]
	raw := p.Encode(nil)
	src, dst := ip("fe80::1").As16(), AllSPFRouters6.As16()
	cs := Checksum6(src, dst, raw)
	raw[12], raw[13] = byte(cs>>8), byte(cs)
	if Checksum6(src, dst, raw) != 0 {
		t.Fatal("a packet with its checksum does not sum to zero")
	}
}

func TestAuthentication(t *testing.T) {
	p := &Packet{V: V2, Type: TypeHello, RouterID: rid1, Hello: &Hello{MaskBits: 24, Interval: 10, Dead: 40}}
	md5a := &Auth{Type: AuthCrypto, Keys: map[uint8]string{1: "k1"}, SendKey: 1}
	cases := []struct {
		name     string
		send, rx *Auth
		ok       bool
	}{
		{"none/none", nil, nil, true},
		{"none/simple", nil, &Auth{Type: AuthSimple, Simple: "x"}, false},
		{"simple/simple", &Auth{Type: AuthSimple, Simple: "x"}, &Auth{Type: AuthSimple, Simple: "x"}, true},
		{"simple wrong", &Auth{Type: AuthSimple, Simple: "x"}, &Auth{Type: AuthSimple, Simple: "y"}, false},
		{"md5", md5a, md5a, true},
		{"md5 rollover", md5a, &Auth{Type: AuthCrypto, Keys: map[uint8]string{1: "k1", 2: "k2"}, SendKey: 2}, true},
		{"md5 wrong key", md5a, &Auth{Type: AuthCrypto, Keys: map[uint8]string{1: "k2"}, SendKey: 1}, false},
		{"md5 unknown id", md5a, &Auth{Type: AuthCrypto, Keys: map[uint8]string{3: "k1"}, SendKey: 3}, false},
		{"md5/none", md5a, nil, false},
	}
	for _, c := range cases {
		raw := p.Encode(c.send)
		q, err := Decode(V2, raw)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := c.rx.Verify(raw, q); got != c.ok {
			t.Errorf("%s: verify %v, want %v", c.name, got, c.ok)
		}
	}
	raw := p.Encode(nil)
	raw[30] ^= 1
	if _, err := Decode(V2, raw); err != ErrChecksum {
		t.Fatalf("corrupted packet: %v", err)
	}
	raw = p.Encode(md5a)
	raw[30] ^= 1
	if q, err := Decode(V2, raw); err != nil || md5a.Verify(raw, q) {
		t.Fatalf("corrupted md5 packet accepted: %v", err)
	}
}

func TestCompare(t *testing.T) {
	h := LSAHeader{Seq: 5, Checksum: 100, Age: 10}
	with := func(f func(*LSAHeader)) LSAHeader { c := h; f(&c); return c }
	cases := []struct {
		name string
		a    LSAHeader
		want int
	}{
		{"same", h, 0},
		{"higher seq", with(func(c *LSAHeader) { c.Seq = 6 }), 1},
		{"lower seq", with(func(c *LSAHeader) { c.Seq = InitialSeq }), -1},
		{"higher checksum", with(func(c *LSAHeader) { c.Checksum = 101 }), 1},
		{"maxage", with(func(c *LSAHeader) { c.Age = MaxAge }), 1},
		{"age within MaxAgeDiff", with(func(c *LSAHeader) { c.Age = 10 + MaxAgeDiff }), 0},
		{"older than MaxAgeDiff", with(func(c *LSAHeader) { c.Age = 11 + MaxAgeDiff }), -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, h); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
		if got := Compare(h, c.a); got != -c.want {
			t.Errorf("%s reversed: %d, want %d", c.name, got, -c.want)
		}
	}
}

func TestMaskAndIDs(t *testing.T) {
	for b := range 33 {
		if got := MaskBits(Mask(b)); got != b {
			t.Fatalf("/%d -> %d", b, got)
		}
	}
	if MaskBits(ip("255.0.255.0")) != -1 {
		t.Fatal("non-contiguous mask accepted")
	}
	for s, want := range map[string]ID{"0": 0, "0.0.0.1": 1, "1": 1, "10.0.0.1": 0x0a000001} {
		if got, err := ParseID(s); err != nil || got != want {
			t.Errorf("ParseID(%q) = %v %v", s, got, err)
		}
	}
	if V3.Scope(V3Link) != ScopeLink || V3.Scope(V3External) != ScopeAS || V3.Scope(V3Router) != ScopeArea ||
		V2.Scope(V2External) != ScopeAS || V2.Scope(V2Summary) != ScopeArea || V3.Scope(0xa00a) != ScopeArea {
		t.Fatal("scopes")
	}
}

func TestLSDB(t *testing.T) {
	now := time.Unix(1000, 0)
	d := NewLSDB()
	l := testLSAs(V2)[0].Encode()
	if !d.Install(l, now) {
		t.Fatal("new LSA not a change")
	}
	if g := d.Get(l.Ref(), now.Add(100*time.Second)); g.Age != 100 || !lsaChecksumOK(g.Raw) {
		t.Fatalf("aged copy: age %d", g.Age)
	}
	l2 := *l
	l2.Seq++
	if d.Install(l2.Encode(), now) {
		t.Fatal("refresh counted as a change")
	}
	l3 := *l
	l3.Seq += 2
	l3.Links = l3.Links[:1]
	if !d.Install(l3.Encode(), now) {
		t.Fatal("different links not a change")
	}
	if got := d.MaxAged(now.Add(MaxAge * time.Second)); len(got) != 1 {
		t.Fatalf("maxaged %v", got)
	}
	for _, x := range testLSAs(V2)[1:] {
		d.Install(x.Encode(), now)
	}
	all := d.All(now)
	if len(all) != 6 || all[0].Type != V2Router || all[5].Type != V2External || len(d.OfType(V2Network, now)) != 1 {
		t.Fatalf("all %d", len(all))
	}
	d.Delete(l.Ref())
	if d.Len() != 5 || d.Get(l.Ref(), now) != nil {
		t.Fatal("delete")
	}
}

func FuzzDecode(f *testing.F) {
	for _, v := range []Version{V2, V3} {
		for _, p := range testPackets(v) {
			f.Add(byte(v), p.Encode(nil))
		}
	}
	f.Fuzz(func(t *testing.T, ver byte, b []byte) {
		v := V2
		if ver%2 == 1 {
			v = V3
		}
		p, err := Decode(v, b)
		if err != nil || p.AuType == AuthCrypto {
			return
		}
		// Whatever decodes must encode again and decode to the same packet.
		q, err := Decode(v, p.Encode(nil))
		if err != nil {
			t.Fatalf("re-decode: %v", err)
		}
		q.AuType, q.authField = p.AuType, p.authField
		if !reflect.DeepEqual(q, p) {
			t.Fatalf("re-encode\n got %+v\nwant %+v", q, p)
		}
	})
}

func FuzzParseLSA(f *testing.F) {
	for _, v := range []Version{V2, V3} {
		for _, l := range testLSAs(v) {
			f.Add(byte(v), l.Encode().Raw)
		}
	}
	f.Fuzz(func(t *testing.T, ver byte, b []byte) {
		v := V2
		if ver%2 == 1 {
			v = V3
		}
		l, n, err := ParseLSA(v, b)
		if err != nil {
			return
		}
		if n > len(b) || !lsaChecksumOK(l.Raw) {
			t.Fatal("accepted a bad LSA")
		}
	})
}
