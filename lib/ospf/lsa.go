package ospf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

const lsaHeaderLen = 20

// LSAHeader is the common LSA header (RFC 2328 A.4.1, RFC 5340 A.4.2).
// In OSPFv2 the second and third bytes are the options and the type; in
// OSPFv3 they are the 16-bit type (Options is unused).
type LSAHeader struct {
	Age      uint16
	Options  uint8 // v2 only
	Type     LSType
	ID       ID
	AdvRtr   ID
	Seq      int32
	Checksum uint16
	Length   uint16
}

// LSRef names an LSA: the key of the database.
type LSRef struct {
	Type   LSType
	ID     ID
	AdvRtr ID
}

func (r LSRef) String() string { return fmt.Sprintf("0x%04x %s %s", uint16(r.Type), r.ID, r.AdvRtr) }

// Ref returns the key of the LSA.
func (h LSAHeader) Ref() LSRef { return LSRef{Type: h.Type, ID: h.ID, AdvRtr: h.AdvRtr} }

func decodeLSAHeader(v Version, b []byte) LSAHeader {
	h := LSAHeader{
		Age:      binary.BigEndian.Uint16(b),
		ID:       ID(binary.BigEndian.Uint32(b[4:])),
		AdvRtr:   ID(binary.BigEndian.Uint32(b[8:])),
		Seq:      int32(binary.BigEndian.Uint32(b[12:])),
		Checksum: binary.BigEndian.Uint16(b[16:]),
		Length:   binary.BigEndian.Uint16(b[18:]),
	}
	if v == V2 {
		h.Options, h.Type = b[2], LSType(b[3])
	} else {
		h.Type = LSType(binary.BigEndian.Uint16(b[2:]))
	}
	return h
}

func (h LSAHeader) appendTo(v Version, b []byte) []byte {
	var f [lsaHeaderLen]byte
	binary.BigEndian.PutUint16(f[0:], h.Age)
	if v == V2 {
		f[2], f[3] = h.Options, uint8(h.Type)
	} else {
		binary.BigEndian.PutUint16(f[2:], uint16(h.Type))
	}
	binary.BigEndian.PutUint32(f[4:], uint32(h.ID))
	binary.BigEndian.PutUint32(f[8:], uint32(h.AdvRtr))
	binary.BigEndian.PutUint32(f[12:], uint32(h.Seq))
	binary.BigEndian.PutUint16(f[16:], h.Checksum)
	binary.BigEndian.PutUint16(f[18:], h.Length)
	return append(b, f[:]...)
}

// Compare tells which of two instances of an LSA is more recent (RFC 2328
// §13.1): >0 when a is newer, <0 when b is newer, 0 when they are the same
// instance. The ages are the current ages.
func Compare(a, b LSAHeader) int {
	switch {
	case a.Seq != b.Seq:
		if a.Seq > b.Seq {
			return 1
		}
		return -1
	case a.Checksum != b.Checksum:
		if a.Checksum > b.Checksum {
			return 1
		}
		return -1
	case (a.Age >= MaxAge) != (b.Age >= MaxAge):
		if a.Age >= MaxAge {
			return 1
		}
		return -1
	}
	d := int(a.Age) - int(b.Age)
	if d > MaxAgeDiff || d < -MaxAgeDiff {
		if d < 0 {
			return 1 // a is younger
		}
		return -1
	}
	return 0
}

// Router LSA link types (both versions).
const (
	LinkP2P     = 1
	LinkTransit = 2
	LinkStub    = 3 // v2 only
	LinkVirtual = 4
)

// Router LSA flags.
const (
	FlagB  = 0x01 // area border router
	FlagE  = 0x02 // AS boundary router
	FlagV  = 0x04 // virtual link endpoint
	FlagNt = 0x10 // v3: NSSA translator
)

// RouterLink is a link of a router LSA. OSPFv2 uses ID and Data (RFC 2328
// A.4.2), OSPFv3 the interface ids and the neighbour's router id (RFC 5340
// A.4.3).
type RouterLink struct {
	Type   uint8
	Metric uint16
	// v2: link id and link data.
	ID, Data ID
	// v3: this router's interface id, the neighbour's interface id and
	// router id (transit: the DR's interface id and router id).
	IfID, NbrIfID uint32
	NbrRouter     ID
}

// PrefixEntry is an OSPFv3 address prefix with its options and metric
// (RFC 5340 A.4.1).
type PrefixEntry struct {
	Prefix  netip.Prefix
	Options uint8
	Metric  uint16
}

// LSA is a link state advertisement: the header, the parsed body of the
// known types and the encoded form (Raw, checksum included). Which body
// fields are used depends on the version and type.
type LSA struct {
	LSAHeader
	V Version

	// Router LSA (both), v3 network, link and inter-area router LSAs.
	Flags   uint8
	Options uint32 // v3 options in the body
	Links   []RouterLink

	// v2 network LSA: the network's mask; the network is ID/MaskBits.
	MaskBits int
	// Network LSA (both): the attached routers.
	Attached []ID

	// v2 summary (3), v3 inter-area prefix, external (both): the prefix.
	Prefix        netip.Prefix
	PrefixOptions uint8
	// Summary, ASBR summary, inter-area prefix and router, external.
	Metric uint32
	// v3 inter-area router: the router described (v2: the LS id).
	DestRouter ID

	// External (both).
	E2      bool
	Forward netip.Addr // invalid: none
	Tag     uint32
	HasTag  bool // v3: the tag is present (v2 always has one)

	// v3 link LSA.
	Priority  uint8
	LinkLocal netip.Addr
	// v3 link and intra-area prefix LSAs.
	Prefixes []PrefixEntry
	// v3 intra-area prefix LSA: the LSA the prefixes belong to.
	RefType   LSType
	RefID     ID
	RefAdvRtr ID

	Raw []byte
}

// Ref returns the key of the LSA.
func (l *LSA) Ref() LSRef { return l.LSAHeader.Ref() }

// ---- OSPFv3 prefixes (RFC 5340 A.4.1) ----

func prefixWords(bits int) int { return (bits + 31) / 32 }

// appendPrefix writes length, options, the 16-bit field and the prefix.
func appendPrefix(b []byte, p netip.Prefix, opts uint8, field uint16) []byte {
	b = append(b, uint8(p.Bits()), opts)
	b = binary.BigEndian.AppendUint16(b, field)
	a := p.Masked().Addr().As16()
	return append(b, a[:4*prefixWords(p.Bits())]...)
}

// parsePrefix reads a prefix entry; used is its length.
func parsePrefix(b []byte) (p netip.Prefix, opts uint8, field uint16, used int, err error) {
	if len(b) < 4 {
		return p, 0, 0, 0, ErrInvalid
	}
	bits := int(b[0])
	if bits > 128 {
		return p, 0, 0, 0, ErrInvalid
	}
	n := 4 * prefixWords(bits)
	if len(b) < 4+n {
		return p, 0, 0, 0, ErrInvalid
	}
	var a [16]byte
	copy(a[:], b[4:4+n])
	p, err = netip.AddrFrom16(a).Prefix(bits)
	if err != nil {
		return p, 0, 0, 0, ErrInvalid
	}
	return p, b[1], binary.BigEndian.Uint16(b[2:]), 4 + n, nil
}

func put24(b []byte, v uint32) []byte { return append(b, byte(v>>16), byte(v>>8), byte(v)) }

func get24(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }

// ---- encoding ----

// Encode builds Raw from the header and body and computes the length and
// checksum (both also set in the header). l.V selects the version.
func (l *LSA) Encode() *LSA {
	b := l.LSAHeader.appendTo(l.V, make([]byte, 0, 64))
	if l.V == V2 {
		b = l.encodeV2(b)
	} else {
		b = l.encodeV3(b)
	}
	binary.BigEndian.PutUint16(b[18:], uint16(len(b)))
	binary.BigEndian.PutUint16(b[16:], 0)
	cs := fletcher(b[2:], 14)
	binary.BigEndian.PutUint16(b[16:], cs)
	l.Length, l.Checksum, l.Raw = uint16(len(b)), cs, b
	return l
}

func appendID(b []byte, i ID) []byte { return binary.BigEndian.AppendUint32(b, uint32(i)) }

func (l *LSA) encodeV2(b []byte) []byte {
	switch l.Type {
	case V2Router:
		b = append(b, l.Flags, 0)
		b = binary.BigEndian.AppendUint16(b, uint16(len(l.Links)))
		for _, k := range l.Links {
			b = appendID(b, k.ID)
			b = appendID(b, k.Data)
			b = append(b, k.Type, 0)
			b = binary.BigEndian.AppendUint16(b, k.Metric)
		}
	case V2Network:
		b = appendID(b, IDFrom(Mask(l.MaskBits)))
		for _, a := range l.Attached {
			b = appendID(b, a)
		}
	case V2Summary, V2ASBRSummary:
		bits := l.Prefix.Bits()
		if l.Type == V2ASBRSummary {
			bits = 0
		}
		b = appendID(b, IDFrom(Mask(bits)))
		b = binary.BigEndian.AppendUint32(b, l.Metric&LSInfinity)
	case V2External:
		b = appendID(b, IDFrom(Mask(l.Prefix.Bits())))
		m := l.Metric & LSInfinity
		if l.E2 {
			m |= 0x80000000
		}
		b = binary.BigEndian.AppendUint32(b, m)
		b = appendID(b, IDFrom(l.Forward))
		b = binary.BigEndian.AppendUint32(b, l.Tag)
	}
	return b
}

// External flags (OSPFv3, RFC 5340 A.4.7).
const (
	extT = 0x01
	extF = 0x02
	extE = 0x04
)

func (l *LSA) encodeV3(b []byte) []byte {
	switch l.Type {
	case V3Router:
		b = append(b, l.Flags)
		b = put24(b, l.Options)
		for _, k := range l.Links {
			b = append(b, k.Type, 0)
			b = binary.BigEndian.AppendUint16(b, k.Metric)
			b = binary.BigEndian.AppendUint32(b, k.IfID)
			b = binary.BigEndian.AppendUint32(b, k.NbrIfID)
			b = appendID(b, k.NbrRouter)
		}
	case V3Network:
		b = append(b, 0)
		b = put24(b, l.Options)
		for _, a := range l.Attached {
			b = appendID(b, a)
		}
	case V3InterAreaPrefix:
		b = append(b, 0)
		b = put24(b, l.Metric&LSInfinity)
		b = appendPrefix(b, l.Prefix, l.PrefixOptions, 0)
	case V3InterAreaRouter:
		b = append(b, 0)
		b = put24(b, l.Options)
		b = append(b, 0)
		b = put24(b, l.Metric&LSInfinity)
		b = appendID(b, l.DestRouter)
	case V3External:
		var flags uint8
		if l.E2 {
			flags |= extE
		}
		if l.Forward.IsValid() {
			flags |= extF
		}
		if l.HasTag {
			flags |= extT
		}
		b = append(b, flags)
		b = put24(b, l.Metric&LSInfinity)
		b = appendPrefix(b, l.Prefix, l.PrefixOptions, 0) // referenced LS type 0
		if l.Forward.IsValid() {
			a := l.Forward.As16()
			b = append(b, a[:]...)
		}
		if l.HasTag {
			b = binary.BigEndian.AppendUint32(b, l.Tag)
		}
	case V3Link:
		b = append(b, l.Priority)
		b = put24(b, l.Options)
		a := l.LinkLocal.As16()
		b = append(b, a[:]...)
		b = binary.BigEndian.AppendUint32(b, uint32(len(l.Prefixes)))
		for _, p := range l.Prefixes {
			b = appendPrefix(b, p.Prefix, p.Options, 0)
		}
	case V3IntraAreaPrefix:
		b = binary.BigEndian.AppendUint16(b, uint16(len(l.Prefixes)))
		b = binary.BigEndian.AppendUint16(b, uint16(l.RefType))
		b = appendID(b, l.RefID)
		b = appendID(b, l.RefAdvRtr)
		for _, p := range l.Prefixes {
			b = appendPrefix(b, p.Prefix, p.Options, p.Metric)
		}
	}
	return b
}

// WithAge returns a copy of the LSA with another age (the checksum does
// not cover the age).
func (l *LSA) WithAge(age uint16) *LSA {
	c := *l
	c.Raw = append([]byte(nil), l.Raw...)
	c.Age = age
	binary.BigEndian.PutUint16(c.Raw, age)
	return &c
}

// ---- parsing ----

// ErrLSAChecksum is returned for an LSA whose checksum does not verify.
var ErrLSAChecksum = errors.New("ospf: bad LSA checksum")

// ParseLSA parses one LSA of version v at the start of b; used is its
// length. LSAs of unknown types are kept as Raw only (flooded unchanged).
func ParseLSA(v Version, b []byte) (l *LSA, used int, err error) {
	if len(b) < lsaHeaderLen {
		return nil, 0, ErrShort
	}
	h := decodeLSAHeader(v, b)
	n := int(h.Length)
	if n < lsaHeaderLen || n > len(b) || n%4 != 0 {
		return nil, 0, ErrInvalid
	}
	raw := b[:n]
	if !lsaChecksumOK(raw) {
		return nil, 0, ErrLSAChecksum
	}
	l = &LSA{LSAHeader: h, V: v, Raw: append([]byte(nil), raw...)}
	body := raw[lsaHeaderLen:]
	if v == V2 {
		err = l.parseV2(body)
	} else {
		err = l.parseV3(body)
	}
	if err != nil {
		return nil, 0, err
	}
	return l, n, nil
}

func getID(b []byte) ID { return ID(binary.BigEndian.Uint32(b)) }

func (l *LSA) parseV2(body []byte) error {
	switch l.Type {
	case V2Router:
		if len(body) < 4 {
			return ErrInvalid
		}
		l.Flags = body[0] & 7
		cnt := int(binary.BigEndian.Uint16(body[2:]))
		rest := body[4:]
		for range cnt {
			if len(rest) < 12 {
				return ErrInvalid
			}
			k := RouterLink{ID: getID(rest), Data: getID(rest[4:]), Type: rest[8], Metric: binary.BigEndian.Uint16(rest[10:])}
			// Skip the TOS metrics (RFC 2328 A.4.2; always 0 in practice).
			tos := int(rest[9])
			if len(rest) < 12+4*tos {
				return ErrInvalid
			}
			rest = rest[12+4*tos:]
			l.Links = append(l.Links, k)
		}
	case V2Network:
		if len(body) < 8 || len(body)%4 != 0 {
			return ErrInvalid
		}
		if l.MaskBits = MaskBits(getID(body).Addr()); l.MaskBits < 0 {
			return ErrInvalid
		}
		for i := 4; i < len(body); i += 4 {
			l.Attached = append(l.Attached, getID(body[i:]))
		}
	case V2Summary, V2ASBRSummary:
		if len(body) < 8 {
			return ErrInvalid
		}
		bits := MaskBits(getID(body).Addr())
		if bits < 0 {
			return ErrInvalid
		}
		l.Metric = binary.BigEndian.Uint32(body[4:]) & LSInfinity
		if l.Type == V2Summary {
			l.Prefix = netip.PrefixFrom(l.ID.Addr(), bits).Masked()
		} else {
			l.DestRouter = l.ID
		}
	case V2External:
		if len(body) < 16 {
			return ErrInvalid
		}
		bits := MaskBits(getID(body).Addr())
		if bits < 0 {
			return ErrInvalid
		}
		l.Prefix = netip.PrefixFrom(l.ID.Addr(), bits).Masked()
		m := binary.BigEndian.Uint32(body[4:])
		l.E2, l.Metric = m&0x80000000 != 0, m&LSInfinity
		if f := getID(body[8:]); f != 0 {
			l.Forward = f.Addr()
		}
		l.Tag, l.HasTag = binary.BigEndian.Uint32(body[12:]), true
	}
	return nil
}

func (l *LSA) parseV3(body []byte) error {
	switch l.Type {
	case V3Router:
		if len(body) < 4 || (len(body)-4)%16 != 0 {
			return ErrInvalid
		}
		l.Flags, l.Options = body[0], get24(body[1:])
		for i := 4; i < len(body); i += 16 {
			r := body[i:]
			l.Links = append(l.Links, RouterLink{Type: r[0], Metric: binary.BigEndian.Uint16(r[2:]),
				IfID: binary.BigEndian.Uint32(r[4:]), NbrIfID: binary.BigEndian.Uint32(r[8:]), NbrRouter: getID(r[12:])})
		}
	case V3Network:
		if len(body) < 8 || len(body)%4 != 0 {
			return ErrInvalid
		}
		l.Options = get24(body[1:])
		for i := 4; i < len(body); i += 4 {
			l.Attached = append(l.Attached, getID(body[i:]))
		}
	case V3InterAreaPrefix:
		if len(body) < 8 {
			return ErrInvalid
		}
		l.Metric = get24(body[1:])
		p, opts, _, _, err := parsePrefix(body[4:])
		if err != nil {
			return err
		}
		l.Prefix, l.PrefixOptions = p, opts
	case V3InterAreaRouter:
		if len(body) < 12 {
			return ErrInvalid
		}
		l.Options, l.Metric, l.DestRouter = get24(body[1:]), get24(body[5:]), getID(body[8:])
	case V3External:
		if len(body) < 8 {
			return ErrInvalid
		}
		flags := body[0]
		l.E2, l.Metric = flags&extE != 0, get24(body[1:])
		p, opts, ref, used, err := parsePrefix(body[4:])
		if err != nil {
			return err
		}
		l.Prefix, l.PrefixOptions = p, opts
		rest := body[4+used:]
		if flags&extF != 0 {
			if len(rest) < 16 {
				return ErrInvalid
			}
			l.Forward = netip.AddrFrom16([16]byte(rest[:16]))
			rest = rest[16:]
		}
		if flags&extT != 0 {
			if len(rest) < 4 {
				return ErrInvalid
			}
			l.Tag, l.HasTag = binary.BigEndian.Uint32(rest), true
			rest = rest[4:]
		}
		if ref != 0 && len(rest) < 4 {
			return ErrInvalid
		}
	case V3Link:
		if len(body) < 24 {
			return ErrInvalid
		}
		l.Priority, l.Options = body[0], get24(body[1:])
		l.LinkLocal = netip.AddrFrom16([16]byte(body[4:20]))
		n := int(binary.BigEndian.Uint32(body[20:]))
		rest := body[24:]
		for range n {
			p, opts, _, used, err := parsePrefix(rest)
			if err != nil {
				return err
			}
			l.Prefixes = append(l.Prefixes, PrefixEntry{Prefix: p, Options: opts})
			rest = rest[used:]
		}
	case V3IntraAreaPrefix:
		if len(body) < 12 {
			return ErrInvalid
		}
		n := int(binary.BigEndian.Uint16(body))
		l.RefType, l.RefID, l.RefAdvRtr = LSType(binary.BigEndian.Uint16(body[2:])), getID(body[4:]), getID(body[8:])
		rest := body[12:]
		for range n {
			p, opts, metric, used, err := parsePrefix(rest)
			if err != nil {
				return err
			}
			l.Prefixes = append(l.Prefixes, PrefixEntry{Prefix: p, Options: opts, Metric: metric})
			rest = rest[used:]
		}
	}
	return nil
}

// MaskBits returns the prefix length of a netmask (-1: not contiguous).
func MaskBits(m netip.Addr) int {
	if !m.Is4() {
		return -1
	}
	v := binary.BigEndian.Uint32(m.AsSlice())
	n := 0
	for v&0x80000000 != 0 {
		n++
		v <<= 1
	}
	if v != 0 {
		return -1
	}
	return n
}

// Mask returns the netmask of a prefix length.
func Mask(bits int) netip.Addr {
	var v uint32
	if bits > 0 {
		v = ^uint32(0) << (32 - bits)
	}
	return ID(v).Addr()
}

// fletcher computes the ISO 8473 / RFC 1008 checksum of b with the
// checksum field at offset off (two bytes, treated as zero). For LSAs, b
// starts after the age field and off is 14.
func fletcher(b []byte, off int) uint16 {
	var c0, c1 int
	for i, v := range b {
		if i == off || i == off+1 {
			v = 0
		}
		c0 = (c0 + int(v)) % 255
		c1 = (c1 + c0) % 255
	}
	x := ((len(b)-off-1)*c0 - c1) % 255
	if x <= 0 {
		x += 255
	}
	y := 510 - c0 - x
	if y > 255 {
		y -= 255
	}
	return uint16(x)<<8 | uint16(y)
}

// lsaChecksumOK verifies an encoded LSA's checksum: the Fletcher sums over
// the LSA without its age are both zero.
func lsaChecksumOK(raw []byte) bool {
	if binary.BigEndian.Uint16(raw[16:]) == 0 {
		return false // RFC 2328 §13 (1): a zero checksum is never valid
	}
	var c0, c1 int
	for _, v := range raw[2:] {
		c0 = (c0 + int(v)) % 255
		c1 = (c1 + c0) % 255
	}
	return c0 == 0 && c1 == 0
}
