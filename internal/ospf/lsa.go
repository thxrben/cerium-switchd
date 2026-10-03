package ospf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// LSA types (OSPFv2) and architectural constants (RFC 2328 B).
const (
	LSARouter      = 1
	LSANetwork     = 2
	LSASummaryNet  = 3
	LSASummaryASBR = 4
	LSAExternal    = 5
	MaxAge         = 3600
	MaxAgeDiff     = 900
	LSRefreshTime  = 1800
	MinLSInterval  = 5
	MinLSArrival   = 1
	CheckAge       = 300
	InitialSeq     = int32(-0x7fffffff) // 0x80000001
	MaxSeq         = int32(0x7fffffff)
	LSInfinity     = 0xffffff
	lsaHeaderLen   = 20
)

var lsaTypeNames = [...]string{"", "Router", "Network", "Summary", "ASBRSum", "Extern"}

// LSATypeName returns the name of an LSA type.
func LSATypeName(t uint8) string {
	if int(t) < len(lsaTypeNames) && t > 0 {
		return lsaTypeNames[t]
	}
	return fmt.Sprintf("type-%d", t)
}

// Router LSA link types (RFC 2328 A.4.2).
const (
	LinkP2P     = 1
	LinkTransit = 2
	LinkStub    = 3
	LinkVirtual = 4
)

// Router LSA flags.
const (
	FlagB = 0x01 // area border router
	FlagE = 0x02 // AS boundary router
	FlagV = 0x04 // virtual link endpoint
)

// LSAHeader is the common LSA header (RFC 2328 A.4.1).
type LSAHeader struct {
	Age      uint16
	Options  uint8
	Type     uint8
	ID       netip.Addr
	AdvRtr   netip.Addr
	Seq      int32
	Checksum uint16
	Length   uint16
}

// Ref returns the key of the LSA.
func (h LSAHeader) Ref() LSRef { return LSRef{Type: h.Type, ID: h.ID, AdvRtr: h.AdvRtr} }

func decodeLSAHeader(b []byte) LSAHeader {
	return LSAHeader{
		Age:      binary.BigEndian.Uint16(b),
		Options:  b[2],
		Type:     b[3],
		ID:       addr4(b[4:]),
		AdvRtr:   addr4(b[8:]),
		Seq:      int32(binary.BigEndian.Uint32(b[12:])),
		Checksum: binary.BigEndian.Uint16(b[16:]),
		Length:   binary.BigEndian.Uint16(b[18:]),
	}
}

func (h LSAHeader) append(b []byte) []byte {
	var f [lsaHeaderLen]byte
	binary.BigEndian.PutUint16(f[0:], h.Age)
	f[2], f[3] = h.Options, h.Type
	put4(f[4:], h.ID)
	put4(f[8:], h.AdvRtr)
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

// RouterLink is a link of a router LSA.
type RouterLink struct {
	ID     netip.Addr
	Data   netip.Addr
	Type   uint8
	Metric uint16
}

// ExternalInfo is the body of an AS-external LSA.
type ExternalInfo struct {
	E2      bool // type 2 metric
	Metric  uint32
	Forward netip.Addr
	Tag     uint32
}

// LSA is a link state advertisement: the header, the parsed body of the
// known types and the encoded form (Raw, checksum included).
type LSA struct {
	LSAHeader
	// Router LSA.
	Flags uint8
	Links []RouterLink
	// Network, summary and external LSAs.
	Mask netip.Addr
	// Network LSA.
	Attached []netip.Addr
	// Summary LSAs (types 3, 4).
	Metric uint32
	// External LSA.
	External ExternalInfo

	Raw []byte
}

// Ref returns the key of the LSA.
func (l *LSA) Ref() LSRef { return l.LSAHeader.Ref() }

// Encode builds Raw from the header and body and computes the length and
// checksum (both also set in the header).
func (l *LSA) Encode() *LSA {
	b := l.LSAHeader.append(make([]byte, 0, 64))
	switch l.Type {
	case LSARouter:
		b = append(b, l.Flags, 0)
		b = binary.BigEndian.AppendUint16(b, uint16(len(l.Links)))
		for _, k := range l.Links {
			var f [12]byte
			put4(f[0:], k.ID)
			put4(f[4:], k.Data)
			f[8] = k.Type
			binary.BigEndian.PutUint16(f[10:], k.Metric)
			b = append(b, f[:]...)
		}
	case LSANetwork:
		b = append(b, 0, 0, 0, 0)
		put4(b[len(b)-4:], l.Mask)
		for _, a := range l.Attached {
			b = append(b, 0, 0, 0, 0)
			put4(b[len(b)-4:], a)
		}
	case LSASummaryNet, LSASummaryASBR:
		b = append(b, 0, 0, 0, 0)
		put4(b[len(b)-4:], l.Mask)
		b = binary.BigEndian.AppendUint32(b, l.Metric&LSInfinity)
	case LSAExternal:
		e := l.External
		var f [16]byte
		put4(f[0:], l.Mask)
		binary.BigEndian.PutUint32(f[4:], e.Metric&LSInfinity)
		if e.E2 {
			f[4] |= 0x80
		}
		put4(f[8:], e.Forward)
		binary.BigEndian.PutUint32(f[12:], e.Tag)
		b = append(b, f[:]...)
	}
	binary.BigEndian.PutUint16(b[18:], uint16(len(b)))
	binary.BigEndian.PutUint16(b[16:], 0)
	cs := fletcher(b[2:], 14)
	binary.BigEndian.PutUint16(b[16:], cs)
	l.Length, l.Checksum, l.Raw = uint16(len(b)), cs, b
	return l
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

// ErrLSAChecksum is returned for an LSA whose checksum does not verify.
var ErrLSAChecksum = errors.New("ospf: bad LSA checksum")

// ParseLSA parses one LSA at the start of b; used is its length.
func ParseLSA(b []byte) (l *LSA, used int, err error) {
	if len(b) < lsaHeaderLen {
		return nil, 0, ErrShort
	}
	h := decodeLSAHeader(b)
	n := int(h.Length)
	if n < lsaHeaderLen || n > len(b) || n%4 != 0 {
		return nil, 0, ErrInvalid
	}
	raw := b[:n]
	if !lsaChecksumOK(raw) {
		return nil, 0, ErrLSAChecksum
	}
	l = &LSA{LSAHeader: h, Raw: append([]byte(nil), raw...)}
	body := raw[lsaHeaderLen:]
	switch h.Type {
	case LSARouter:
		if len(body) < 4 {
			return nil, 0, ErrInvalid
		}
		l.Flags = body[0] & 7
		cnt := int(binary.BigEndian.Uint16(body[2:]))
		rest := body[4:]
		for range cnt {
			if len(rest) < 12 {
				return nil, 0, ErrInvalid
			}
			k := RouterLink{ID: addr4(rest), Data: addr4(rest[4:]), Type: rest[8], Metric: binary.BigEndian.Uint16(rest[10:])}
			// Skip the TOS metrics (RFC 2328 A.4.2; always 0 in practice).
			tos := int(rest[9])
			if len(rest) < 12+4*tos {
				return nil, 0, ErrInvalid
			}
			rest = rest[12+4*tos:]
			l.Links = append(l.Links, k)
		}
	case LSANetwork:
		if len(body) < 8 || len(body)%4 != 0 {
			return nil, 0, ErrInvalid
		}
		l.Mask = addr4(body)
		for i := 4; i < len(body); i += 4 {
			l.Attached = append(l.Attached, addr4(body[i:]))
		}
	case LSASummaryNet, LSASummaryASBR:
		if len(body) < 8 {
			return nil, 0, ErrInvalid
		}
		l.Mask = addr4(body)
		l.Metric = binary.BigEndian.Uint32(body[4:]) & LSInfinity
	case LSAExternal:
		if len(body) < 16 {
			return nil, 0, ErrInvalid
		}
		l.Mask = addr4(body)
		l.External = ExternalInfo{E2: body[4]&0x80 != 0, Metric: binary.BigEndian.Uint32(body[4:]) & LSInfinity,
			Forward: addr4(body[8:]), Tag: binary.BigEndian.Uint32(body[12:])}
	}
	// Other types (opaque) are kept as Raw only and flooded unchanged.
	return l, n, nil
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
	return netip.AddrFrom4([4]byte(binary.BigEndian.AppendUint32(nil, v)))
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
