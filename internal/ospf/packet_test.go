package ospf

import (
	"bytes"
	"net/netip"
	"reflect"
	"testing"
	"time"
)

var (
	rid1 = netip.MustParseAddr("1.1.1.1")
	rid2 = netip.MustParseAddr("2.2.2.2")
	bb   = netip.MustParseAddr("0.0.0.0")
)

func TestFletcher(t *testing.T) {
	l := (&LSA{LSAHeader: LSAHeader{Age: 1, Options: OptE, Type: LSARouter, ID: rid1, AdvRtr: rid1, Seq: InitialSeq},
		Links: []RouterLink{{ID: netip.MustParseAddr("10.0.0.0"), Data: Mask(24), Type: LinkStub, Metric: 10}}}).Encode()
	if !lsaChecksumOK(l.Raw) {
		t.Fatalf("checksum does not verify: % x", l.Raw)
	}
	// Cross-check with an independent implementation of RFC 1008 §7.2
	// (interop with FRR is checked by test/interop/test-ospf.sh).
	if want := rfc1008(l.Raw[2:], 15); l.Checksum != want {
		t.Fatalf("checksum %04x, RFC 1008 algorithm %04x", l.Checksum, want)
	}
	if !lsaChecksumOK(l.WithAge(1234).Raw) {
		t.Fatal("age changes the checksum")
	}
	bad := bytes.Clone(l.Raw)
	bad[25] ^= 1
	if lsaChecksumOK(bad) {
		t.Fatal("corrupted LSA verifies")
	}
	if _, _, err := ParseLSA(bad); err != ErrLSAChecksum {
		t.Fatalf("corrupted LSA parsed: %v", err)
	}
	// Exhaustive-ish: many LSAs of different lengths and contents.
	for n := range 300 {
		l := (&LSA{LSAHeader: LSAHeader{Type: LSASummaryNet, ID: netip.AddrFrom4([4]byte{byte(n), 1, 2, 0}), AdvRtr: rid2, Seq: int32(n)},
			Mask: Mask(n % 33), Metric: uint32(n * 7919)}).Encode()
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

func testLSAs() []*LSA {
	return []*LSA{
		{LSAHeader: LSAHeader{Options: OptE, Type: LSARouter, ID: rid1, AdvRtr: rid1, Seq: InitialSeq}, Flags: FlagB | FlagE,
			Links: []RouterLink{
				{ID: rid2, Data: netip.MustParseAddr("10.0.0.1"), Type: LinkP2P, Metric: 1},
				{ID: netip.MustParseAddr("10.0.0.0"), Data: Mask(30), Type: LinkStub, Metric: 1},
				{ID: netip.MustParseAddr("10.1.0.2"), Data: netip.MustParseAddr("10.1.0.1"), Type: LinkTransit, Metric: 10},
			}},
		{LSAHeader: LSAHeader{Options: OptE, Type: LSANetwork, ID: netip.MustParseAddr("10.1.0.2"), AdvRtr: rid2, Seq: 5},
			Mask: Mask(24), Attached: []netip.Addr{rid1, rid2}},
		{LSAHeader: LSAHeader{Type: LSASummaryNet, ID: netip.MustParseAddr("10.2.0.0"), AdvRtr: rid1, Seq: 1},
			Mask: Mask(16), Metric: 20},
		{LSAHeader: LSAHeader{Type: LSASummaryASBR, ID: rid2, AdvRtr: rid1, Seq: 1}, Mask: bb, Metric: 30},
		{LSAHeader: LSAHeader{Age: 7, Options: OptE, Type: LSAExternal, ID: bb, AdvRtr: rid1, Seq: 1},
			Mask: bb, External: ExternalInfo{E2: true, Metric: 1, Forward: bb, Tag: 7}},
	}
}

func TestLSARoundTrip(t *testing.T) {
	for _, l := range testLSAs() {
		l.Encode()
		q, n, err := ParseLSA(append(bytes.Clone(l.Raw), 0xde, 0xad))
		if err != nil || n != len(l.Raw) {
			t.Fatalf("type %d: %v (%d of %d bytes)", l.Type, err, n, len(l.Raw))
		}
		if !reflect.DeepEqual(q, l) {
			t.Fatalf("type %d:\n got %+v\nwant %+v", l.Type, q, l)
		}
	}
}

func TestPacketRoundTrip(t *testing.T) {
	lsas := testLSAs()
	for _, l := range lsas {
		l.Encode()
	}
	var hdrs []LSAHeader
	for _, l := range lsas {
		hdrs = append(hdrs, l.LSAHeader)
	}
	pkts := []*Packet{
		{Type: TypeHello, Hello: &Hello{Mask: Mask(24), Interval: 10, Options: OptE, Priority: 1, Dead: 40,
			DR: netip.MustParseAddr("10.1.0.2"), BDR: netip.MustParseAddr("10.1.0.1"), Neighbors: []netip.Addr{rid2}}},
		{Type: TypeHello, Hello: &Hello{Mask: Mask(30), Interval: 1, Dead: 3, DR: bb, BDR: bb}},
		{Type: TypeDD, DD: &DD{MTU: 1500, Options: OptE, Flags: DDInit | DDMore | DDMaster, Seq: 4711}},
		{Type: TypeDD, DD: &DD{MTU: 9000, Options: OptE | OptO, Flags: DDMore, Seq: 4712, Headers: hdrs}},
		{Type: TypeLSR, LSR: []LSRef{{Type: LSARouter, ID: rid1, AdvRtr: rid1}, {Type: LSAExternal, ID: bb, AdvRtr: rid2}}},
		{Type: TypeLSU, LSU: lsas},
		{Type: TypeLSAck, Ack: hdrs},
	}
	auths := []*Auth{nil, {Type: AuthSimple, Simple: "secret"},
		{Type: AuthCrypto, Keys: map[uint8]string{1: "old", 2: "new key"}, SendKey: 2}}
	for _, auth := range auths {
		for _, p := range pkts {
			p.RouterID, p.AreaID = rid1, bb
			p.CryptoSeq = 99
			raw := p.Encode(auth)
			q, err := Decode(raw)
			if err != nil {
				t.Fatalf("%s %v: %v", TypeName(p.Type), auth, err)
			}
			if !auth.Verify(raw, q) {
				t.Fatalf("%s %v: authentication does not verify", TypeName(p.Type), auth)
			}
			if auth != nil && auth.Type == AuthCrypto && (q.KeyID != 2 || q.CryptoSeq != 99) {
				t.Fatalf("crypto fields %d %d", q.KeyID, q.CryptoSeq)
			}
			q.AuType, q.KeyID, q.CryptoSeq, q.authField, q.digest = p.AuType, p.KeyID, p.CryptoSeq, p.authField, p.digest
			if !reflect.DeepEqual(q, p) {
				t.Fatalf("%s:\n got %+v\nwant %+v", TypeName(p.Type), q, p)
			}
		}
	}
}

func TestAuthentication(t *testing.T) {
	p := &Packet{Type: TypeHello, RouterID: rid1, AreaID: bb, Hello: &Hello{Mask: Mask(24), Interval: 10, Dead: 40, DR: bb, BDR: bb}}
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
		q, err := Decode(raw)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := c.rx.Verify(raw, q); got != c.ok {
			t.Errorf("%s: verify %v, want %v", c.name, got, c.ok)
		}
	}
	// A modified packet fails: checksum (null/simple) or digest (md5).
	raw := p.Encode(nil)
	raw[30] ^= 1
	if _, err := Decode(raw); err != ErrChecksum {
		t.Fatalf("corrupted packet: %v", err)
	}
	raw = p.Encode(md5a)
	raw[30] ^= 1
	if q, err := Decode(raw); err != nil || md5a.Verify(raw, q) {
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

func TestMask(t *testing.T) {
	for b := range 33 {
		if got := MaskBits(Mask(b)); got != b {
			t.Fatalf("/%d -> %d", b, got)
		}
	}
	if MaskBits(netip.MustParseAddr("255.0.255.0")) != -1 {
		t.Fatal("non-contiguous mask accepted")
	}
}

func TestLSDB(t *testing.T) {
	now := time.Unix(1000, 0)
	d := NewLSDB()
	l := testLSAs()[0].Encode()
	if !d.Install(l, now) {
		t.Fatal("new LSA not a change")
	}
	if g := d.Get(l.Ref(), now.Add(100*time.Second)); g.Age != 100 || !lsaChecksumOK(g.Raw) {
		t.Fatalf("aged copy: age %d", g.Age)
	}
	// Same contents, new sequence number: no change for SPF.
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
	if d.Get(l.Ref(), now.Add(2*MaxAge*time.Second)).Age != MaxAge {
		t.Fatal("age beyond MaxAge")
	}
	for _, x := range testLSAs()[1:] {
		d.Install(x.Encode(), now)
	}
	all := d.All(now)
	if len(all) != 5 || all[0].Type != LSARouter || all[4].Type != LSAExternal || len(d.OfType(LSANetwork, now)) != 1 {
		t.Fatalf("all %d", len(all))
	}
	d.Delete(l.Ref())
	if d.Len() != 4 || d.Get(l.Ref(), now) != nil {
		t.Fatal("delete")
	}
}

func FuzzDecode(f *testing.F) {
	lsas := testLSAs()
	for _, l := range lsas {
		l.Encode()
	}
	for _, p := range []*Packet{
		{Type: TypeHello, Hello: &Hello{Mask: Mask(24), Interval: 10, Dead: 40, Neighbors: []netip.Addr{rid2}}},
		{Type: TypeDD, DD: &DD{MTU: 1500, Flags: DDInit, Headers: []LSAHeader{lsas[0].LSAHeader}}},
		{Type: TypeLSR, LSR: []LSRef{{Type: 1, ID: rid1, AdvRtr: rid1}}},
		{Type: TypeLSU, LSU: lsas},
		{Type: TypeLSAck, Ack: []LSAHeader{lsas[1].LSAHeader}},
	} {
		p.RouterID, p.AreaID = rid1, bb
		f.Add(p.Encode(nil))
		f.Add(p.Encode(&Auth{Type: AuthCrypto, Keys: map[uint8]string{1: "k"}, SendKey: 1}))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := Decode(b)
		if err != nil {
			return
		}
		// Whatever decodes must encode again and decode to the same packet.
		if p.AuType == AuthCrypto {
			return
		}
		q, err := Decode(p.Encode(nil))
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
	for _, l := range testLSAs() {
		f.Add(l.Encode().Raw)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		l, n, err := ParseLSA(b)
		if err != nil {
			return
		}
		if n > len(b) || !lsaChecksumOK(l.Raw) {
			t.Fatal("accepted a bad LSA")
		}
	})
}
