// Package ospf implements OSPF version 2 (RFC 2328) and OSPFv3 (RFC 5340)
// for switchd (reference 5.13).
//
// The protocol core (packets, interface and neighbour state machines,
// DR election, link-state database, flooding, SPF) is pure: it is driven
// by received packets, a clock and timers, and sends through an interface.
// The Linux layer (raw IP sockets) is separate.
package ospf

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// Packet types (RFC 2328 A.3.1).
const (
	TypeHello = 1
	TypeDD    = 2
	TypeLSR   = 3
	TypeLSU   = 4
	TypeLSAck = 5
	headerLen = 24
	helloLen  = 20 // fixed part
	ddLen     = 8
	lsrLen    = 12 // one request entry
	md5Len    = 16
	allSPF    = "224.0.0.5"
	allDR     = "224.0.0.6"
	ProtoOSPF = 89
)

// AllSPFRouters and AllDRouters are the OSPFv2 multicast groups.
var (
	AllSPFRouters = netip.MustParseAddr(allSPF)
	AllDRouters   = netip.MustParseAddr(allDR)
)

var typeNames = [...]string{"", "Hello", "DD", "LSR", "LSU", "LSAck"}

// TypeName returns the name of a packet type.
func TypeName(t uint8) string {
	if int(t) < len(typeNames) && t > 0 {
		return typeNames[t]
	}
	return fmt.Sprintf("type %d", t)
}

// Options bits (RFC 2328 A.2).
const (
	OptE  = 0x02 // AS-external LSAs flooded (not a stub area)
	OptMC = 0x04
	OptNP = 0x08
	OptL  = 0x10 // link-local signalling (RFC 5613), used by graceful restart helpers
	OptDC = 0x20
	OptO  = 0x40 // opaque LSAs (RFC 5250)
)

// Authentication types (RFC 2328 D).
const (
	AuthNone   = 0
	AuthSimple = 1
	AuthCrypto = 2
)

// DD flags (RFC 2328 A.3.3).
const (
	DDMaster = 0x01
	DDMore   = 0x02
	DDInit   = 0x04
)

// Packet is an OSPFv2 packet. Exactly one of the body fields is set,
// according to Type.
type Packet struct {
	Type     uint8
	RouterID netip.Addr
	AreaID   netip.Addr

	Hello *Hello
	DD    *DD
	LSR   []LSRef     // link state request
	LSU   []*LSA      // link state update
	Ack   []LSAHeader // link state acknowledgment

	// Authentication as received (Decode) or used (Encode by Auth).
	AuType    uint16
	KeyID     uint8
	CryptoSeq uint32
	authField [8]byte
	digest    []byte
}

// Hello is the body of a hello packet (RFC 2328 A.3.2).
type Hello struct {
	Mask      netip.Addr
	Interval  uint16 // seconds
	Options   uint8
	Priority  uint8
	Dead      uint32 // seconds
	DR, BDR   netip.Addr
	Neighbors []netip.Addr
}

// DD is the body of a database description packet (RFC 2328 A.3.3).
type DD struct {
	MTU     uint16
	Options uint8
	Flags   uint8
	Seq     uint32
	Headers []LSAHeader
}

// LSRef names an LSA (link state request entry; also the LSDB key).
type LSRef struct {
	Type   uint8
	ID     netip.Addr
	AdvRtr netip.Addr
}

func (r LSRef) String() string {
	return fmt.Sprintf("%s %s %s", LSATypeName(r.Type), r.ID, r.AdvRtr)
}

// Errors of Decode.
var (
	ErrShort    = errors.New("ospf: packet too short")
	ErrVersion  = errors.New("ospf: not version 2")
	ErrChecksum = errors.New("ospf: bad checksum")
	ErrInvalid  = errors.New("ospf: invalid packet")
	ErrAuth     = errors.New("ospf: authentication failed")
)

func addr4(b []byte) netip.Addr { return netip.AddrFrom4([4]byte(b[:4])) }

func put4(b []byte, a netip.Addr) {
	if a.Is4() {
		v := a.As4()
		copy(b, v[:])
	} else {
		clear(b[:4])
	}
}

// Decode parses an OSPF packet (the IP payload). The checksum is verified
// for null and simple authentication; cryptographic authentication is
// checked by Auth.Verify.
func Decode(b []byte) (*Packet, error) {
	if len(b) < headerLen {
		return nil, ErrShort
	}
	if b[0] != 2 {
		return nil, ErrVersion
	}
	length := int(binary.BigEndian.Uint16(b[2:]))
	if length < headerLen || length > len(b) {
		return nil, ErrInvalid
	}
	p := &Packet{
		Type:     b[1],
		RouterID: addr4(b[4:]),
		AreaID:   addr4(b[8:]),
		AuType:   binary.BigEndian.Uint16(b[14:]),
	}
	copy(p.authField[:], b[16:24])
	switch p.AuType {
	case AuthNone, AuthSimple:
		if ipChecksum(b[:length], true) != 0 {
			return nil, ErrChecksum
		}
	case AuthCrypto:
		p.KeyID = b[18]
		if b[19] != md5Len {
			return nil, ErrInvalid
		}
		p.CryptoSeq = binary.BigEndian.Uint32(b[20:])
		if len(b) < length+md5Len {
			return nil, ErrShort
		}
		p.digest = append([]byte(nil), b[length:length+md5Len]...)
	default:
		return nil, fmt.Errorf("ospf: authentication type %d not supported", p.AuType)
	}
	body := b[headerLen:length]
	var err error
	switch p.Type {
	case TypeHello:
		p.Hello, err = decodeHello(body)
	case TypeDD:
		p.DD, err = decodeDD(body)
	case TypeLSR:
		if len(body)%lsrLen != 0 {
			return nil, ErrInvalid
		}
		for i := 0; i < len(body); i += lsrLen {
			t := binary.BigEndian.Uint32(body[i:])
			if t == 0 || t > 255 {
				return nil, ErrInvalid
			}
			p.LSR = append(p.LSR, LSRef{Type: uint8(t), ID: addr4(body[i+4:]), AdvRtr: addr4(body[i+8:])})
		}
	case TypeLSU:
		if len(body) < 4 {
			return nil, ErrShort
		}
		n := int(binary.BigEndian.Uint32(body))
		rest := body[4:]
		if n > len(rest)/lsaHeaderLen {
			return nil, ErrInvalid
		}
		for range n {
			l, used, err := ParseLSA(rest)
			if err != nil {
				return nil, err
			}
			p.LSU = append(p.LSU, l)
			rest = rest[used:]
		}
	case TypeLSAck:
		if len(body)%lsaHeaderLen != 0 {
			return nil, ErrInvalid
		}
		for i := 0; i < len(body); i += lsaHeaderLen {
			p.Ack = append(p.Ack, decodeLSAHeader(body[i:]))
		}
	default:
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func decodeHello(b []byte) (*Hello, error) {
	if len(b) < helloLen || (len(b)-helloLen)%4 != 0 {
		return nil, ErrInvalid
	}
	h := &Hello{
		Mask:     addr4(b),
		Interval: binary.BigEndian.Uint16(b[4:]),
		Options:  b[6],
		Priority: b[7],
		Dead:     binary.BigEndian.Uint32(b[8:]),
		DR:       addr4(b[12:]),
		BDR:      addr4(b[16:]),
	}
	for i := helloLen; i < len(b); i += 4 {
		h.Neighbors = append(h.Neighbors, addr4(b[i:]))
	}
	return h, nil
}

func decodeDD(b []byte) (*DD, error) {
	if len(b) < ddLen || (len(b)-ddLen)%lsaHeaderLen != 0 {
		return nil, ErrInvalid
	}
	d := &DD{MTU: binary.BigEndian.Uint16(b), Options: b[2], Flags: b[3] & 7, Seq: binary.BigEndian.Uint32(b[4:])}
	for i := ddLen; i < len(b); i += lsaHeaderLen {
		d.Headers = append(d.Headers, decodeLSAHeader(b[i:]))
	}
	return d, nil
}

// Auth is an interface's authentication (nil or Type AuthNone: none).
type Auth struct {
	Type   uint16
	Simple string // AuthSimple: up to 8 bytes
	// AuthCrypto: the keys by id; SendKey is used to send, every key is
	// accepted (key rollover).
	Keys    map[uint8]string
	SendKey uint8
}

// Encode serialises the packet with authentication (auth may be nil).
// For cryptographic authentication the sequence number is p.CryptoSeq and
// the MD5 digest is appended after the OSPF packet (RFC 2328 D.4.3).
func (p *Packet) Encode(auth *Auth) []byte {
	b := make([]byte, headerLen, 256)
	b[0], b[1] = 2, p.Type
	put4(b[4:], p.RouterID)
	put4(b[8:], p.AreaID)
	switch p.Type {
	case TypeHello:
		h := p.Hello
		var f [helloLen]byte
		put4(f[0:], h.Mask)
		binary.BigEndian.PutUint16(f[4:], h.Interval)
		f[6], f[7] = h.Options, h.Priority
		binary.BigEndian.PutUint32(f[8:], h.Dead)
		put4(f[12:], h.DR)
		put4(f[16:], h.BDR)
		b = append(b, f[:]...)
		for _, n := range h.Neighbors {
			b = append(b, 0, 0, 0, 0)
			put4(b[len(b)-4:], n)
		}
	case TypeDD:
		d := p.DD
		var f [ddLen]byte
		binary.BigEndian.PutUint16(f[0:], d.MTU)
		f[2], f[3] = d.Options, d.Flags
		binary.BigEndian.PutUint32(f[4:], d.Seq)
		b = append(b, f[:]...)
		for _, h := range d.Headers {
			b = h.append(b)
		}
	case TypeLSR:
		for _, r := range p.LSR {
			var f [lsrLen]byte
			binary.BigEndian.PutUint32(f[0:], uint32(r.Type))
			put4(f[4:], r.ID)
			put4(f[8:], r.AdvRtr)
			b = append(b, f[:]...)
		}
	case TypeLSU:
		b = binary.BigEndian.AppendUint32(b, uint32(len(p.LSU)))
		for _, l := range p.LSU {
			b = append(b, l.Raw...)
		}
	case TypeLSAck:
		for _, h := range p.Ack {
			b = h.append(b)
		}
	}
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	typ := uint16(AuthNone)
	if auth != nil {
		typ = auth.Type
	}
	binary.BigEndian.PutUint16(b[14:], typ)
	switch typ {
	case AuthNone, AuthSimple:
		binary.BigEndian.PutUint16(b[12:], ipChecksum(b, true))
		if typ == AuthSimple {
			copy(b[16:24], auth.Simple)
		}
	case AuthCrypto:
		b[18], b[19] = auth.SendKey, md5Len
		binary.BigEndian.PutUint32(b[20:], p.CryptoSeq)
		b = append(b, md5Digest(b, auth.Keys[auth.SendKey])...)
	}
	return b
}

// md5Digest is the digest of RFC 2328 D.4.3: MD5 over the packet followed
// by the key padded to 16 bytes.
func md5Digest(pkt []byte, key string) []byte {
	var k [md5Len]byte
	copy(k[:], key)
	h := md5.New()
	h.Write(pkt)
	h.Write(k[:])
	return h.Sum(nil)
}

// Verify checks a received packet's authentication (raw: the IP payload
// the packet was decoded from). The cryptographic sequence number is
// checked by the neighbour (it must not decrease).
func (a *Auth) Verify(raw []byte, p *Packet) bool {
	typ := uint16(AuthNone)
	if a != nil {
		typ = a.Type
	}
	if p.AuType != typ {
		return false
	}
	switch typ {
	case AuthSimple:
		var want [8]byte
		copy(want[:], a.Simple)
		return subtle.ConstantTimeCompare(want[:], p.authField[:]) == 1
	case AuthCrypto:
		key, ok := a.Keys[p.KeyID]
		if !ok {
			return false
		}
		length := int(binary.BigEndian.Uint16(raw[2:]))
		return subtle.ConstantTimeCompare(md5Digest(raw[:length], key), p.digest) == 1
	}
	return true
}

// ipChecksum is the Internet checksum of an OSPF packet; skipAuth leaves
// out the 64-bit authentication field (RFC 2328 D.4.1). Over a packet
// with a correct checksum it returns 0.
func ipChecksum(b []byte, skipAuth bool) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		if skipAuth && i >= 16 && i < 24 {
			continue
		}
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
