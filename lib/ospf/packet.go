// Package ospf implements OSPF version 2 (RFC 2328) and OSPFv3 (RFC 5340)
// with one protocol core (reference 5.13, PLAN.md Phase 9c).
//
// The core (packets, interface and neighbour state machines, DR election,
// link-state database, flooding, origination, SPF) is pure: it is driven
// by received packets, a clock and timers, and sends through an interface.
// The versions differ in their packet and LSA formats, in what a router
// originates and in how prefixes attach to the SPF graph; the rest is
// shared. The Linux layer (raw IP sockets) is separate.
package ospf

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	headerLenV2 = 24
	headerLenV3 = 16
	md5Len      = 16
)

// Packet is an OSPF packet of either version. Exactly one of the body
// fields is set, according to Type.
type Packet struct {
	V        Version
	Type     uint8
	RouterID ID
	AreaID   ID
	// InstanceID (v3) separates OSPFv3 instances on one link.
	InstanceID uint8

	Hello *Hello
	DD    *DD
	LSR   []LSRef     // link state request
	LSU   []*LSA      // link state update
	Ack   []LSAHeader // link state acknowledgment

	// v2 authentication as received (Decode) or used (Encode by Auth).
	AuType    uint16
	KeyID     uint8
	CryptoSeq uint32
	authField [8]byte
	digest    []byte
}

// Hello is the body of a hello packet (RFC 2328 A.3.2, RFC 5340 A.3.2).
// DR and BDR are interface addresses in OSPFv2 and router ids in OSPFv3.
type Hello struct {
	MaskBits    int    // v2: the interface's prefix length
	InterfaceID uint32 // v3: the sender's interface id
	Interval    uint16 // seconds
	Options     uint32 // v2: 8 bits, v3: 24 bits
	Priority    uint8
	Dead        uint32 // seconds (v3: 16 bits)
	DR, BDR     ID
	Neighbors   []ID
}

// DD is the body of a database description packet.
type DD struct {
	MTU     uint16
	Options uint32
	Flags   uint8
	Seq     uint32
	Headers []LSAHeader
}

// DD flags (RFC 2328 A.3.3).
const (
	DDMaster = 0x01
	DDMore   = 0x02
	DDInit   = 0x04
)

// Authentication types (OSPFv2, RFC 2328 D).
const (
	AuthNone   = 0
	AuthSimple = 1
	AuthCrypto = 2
)

// Errors of Decode.
var (
	ErrShort    = errors.New("ospf: packet too short")
	ErrVersion  = errors.New("ospf: wrong version")
	ErrChecksum = errors.New("ospf: bad checksum")
	ErrInvalid  = errors.New("ospf: invalid packet")
	ErrAuth     = errors.New("ospf: authentication failed")
)

func (v Version) headerLen() int {
	if v == V3 {
		return headerLenV3
	}
	return headerLenV2
}

// Decode parses an OSPF packet of version v (the IP payload). OSPFv2
// checksums are verified for null and simple authentication;
// cryptographic authentication is checked by Auth.Verify. OSPFv3 checksums
// are verified by the kernel (IPV6_CHECKSUM).
func Decode(v Version, b []byte) (*Packet, error) {
	hl := v.headerLen()
	if len(b) < hl {
		return nil, ErrShort
	}
	if b[0] != byte(v) {
		return nil, ErrVersion
	}
	length := int(binary.BigEndian.Uint16(b[2:]))
	if length < hl || length > len(b) {
		return nil, ErrInvalid
	}
	p := &Packet{V: v, Type: b[1], RouterID: getID(b[4:]), AreaID: getID(b[8:])}
	if v == V2 {
		p.AuType = binary.BigEndian.Uint16(b[14:])
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
	} else {
		p.InstanceID = b[14]
	}
	body := b[hl:length]
	var err error
	switch p.Type {
	case TypeHello:
		p.Hello, err = decodeHello(v, body)
	case TypeDD:
		p.DD, err = decodeDD(v, body)
	case TypeLSR:
		if len(body)%12 != 0 {
			return nil, ErrInvalid
		}
		for i := 0; i < len(body); i += 12 {
			var t LSType
			if v == V2 {
				w := binary.BigEndian.Uint32(body[i:])
				if w == 0 || w > 255 {
					return nil, ErrInvalid
				}
				t = LSType(w)
			} else {
				t = LSType(binary.BigEndian.Uint16(body[i+2:]))
			}
			p.LSR = append(p.LSR, LSRef{Type: t, ID: getID(body[i+4:]), AdvRtr: getID(body[i+8:])})
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
			l, used, err := ParseLSA(v, rest)
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
			p.Ack = append(p.Ack, decodeLSAHeader(v, body[i:]))
		}
	default:
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func decodeHello(v Version, b []byte) (*Hello, error) {
	if len(b) < 20 || (len(b)-20)%4 != 0 {
		return nil, ErrInvalid
	}
	h := &Hello{DR: getID(b[12:]), BDR: getID(b[16:])}
	if v == V2 {
		if h.MaskBits = MaskBits(getID(b).Addr()); h.MaskBits < 0 {
			return nil, ErrInvalid
		}
		h.Interval, h.Options, h.Priority = binary.BigEndian.Uint16(b[4:]), uint32(b[6]), b[7]
		h.Dead = binary.BigEndian.Uint32(b[8:])
	} else {
		h.InterfaceID = binary.BigEndian.Uint32(b)
		h.Priority, h.Options = b[4], get24(b[5:])
		h.Interval, h.Dead = binary.BigEndian.Uint16(b[8:]), uint32(binary.BigEndian.Uint16(b[10:]))
	}
	for i := 20; i < len(b); i += 4 {
		h.Neighbors = append(h.Neighbors, getID(b[i:]))
	}
	return h, nil
}

func ddLen(v Version) int {
	if v == V3 {
		return 12
	}
	return 8
}

func decodeDD(v Version, b []byte) (*DD, error) {
	fl := ddLen(v)
	if len(b) < fl || (len(b)-fl)%lsaHeaderLen != 0 {
		return nil, ErrInvalid
	}
	d := &DD{}
	if v == V2 {
		d.MTU, d.Options, d.Flags, d.Seq = binary.BigEndian.Uint16(b), uint32(b[2]), b[3]&7, binary.BigEndian.Uint32(b[4:])
	} else {
		d.Options, d.MTU, d.Flags, d.Seq = get24(b[1:]), binary.BigEndian.Uint16(b[4:]), b[7]&7, binary.BigEndian.Uint32(b[8:])
	}
	for i := fl; i < len(b); i += lsaHeaderLen {
		d.Headers = append(d.Headers, decodeLSAHeader(v, b[i:]))
	}
	return d, nil
}

// Auth is an OSPFv2 interface's authentication (nil or Type AuthNone:
// none).
type Auth struct {
	Type   uint16
	Simple string // AuthSimple: up to 8 bytes
	// AuthCrypto: the keys by id; SendKey is used to send, every key is
	// accepted (key rollover).
	Keys    map[uint8]string
	SendKey uint8
}

// Encode serialises the packet. auth (OSPFv2 only, may be nil) adds the
// authentication: for cryptographic authentication the sequence number is
// p.CryptoSeq and the MD5 digest is appended (RFC 2328 D.4.3). OSPFv3
// packets leave the checksum to the kernel (IPV6_CHECKSUM).
func (p *Packet) Encode(auth *Auth) []byte {
	v := p.V
	b := make([]byte, v.headerLen(), 256)
	b[0], b[1] = byte(v), p.Type
	binary.BigEndian.PutUint32(b[4:], uint32(p.RouterID))
	binary.BigEndian.PutUint32(b[8:], uint32(p.AreaID))
	if v == V3 {
		b[14] = p.InstanceID
	}
	switch p.Type {
	case TypeHello:
		h := p.Hello
		if v == V2 {
			b = appendID(b, IDFrom(Mask(h.MaskBits)))
			b = binary.BigEndian.AppendUint16(b, h.Interval)
			b = append(b, uint8(h.Options), h.Priority)
			b = binary.BigEndian.AppendUint32(b, h.Dead)
		} else {
			b = binary.BigEndian.AppendUint32(b, h.InterfaceID)
			b = append(b, h.Priority)
			b = put24(b, h.Options)
			b = binary.BigEndian.AppendUint16(b, h.Interval)
			b = binary.BigEndian.AppendUint16(b, uint16(h.Dead))
		}
		b = appendID(b, h.DR)
		b = appendID(b, h.BDR)
		for _, n := range h.Neighbors {
			b = appendID(b, n)
		}
	case TypeDD:
		d := p.DD
		if v == V2 {
			b = binary.BigEndian.AppendUint16(b, d.MTU)
			b = append(b, uint8(d.Options), d.Flags)
		} else {
			b = append(b, 0)
			b = put24(b, d.Options)
			b = binary.BigEndian.AppendUint16(b, d.MTU)
			b = append(b, 0, d.Flags)
		}
		b = binary.BigEndian.AppendUint32(b, d.Seq)
		for _, h := range d.Headers {
			b = h.appendTo(v, b)
		}
	case TypeLSR:
		for _, r := range p.LSR {
			if v == V2 {
				b = binary.BigEndian.AppendUint32(b, uint32(r.Type))
			} else {
				b = append(b, 0, 0)
				b = binary.BigEndian.AppendUint16(b, uint16(r.Type))
			}
			b = appendID(b, r.ID)
			b = appendID(b, r.AdvRtr)
		}
	case TypeLSU:
		b = binary.BigEndian.AppendUint32(b, uint32(len(p.LSU)))
		for _, l := range p.LSU {
			b = append(b, l.Raw...)
		}
	case TypeLSAck:
		for _, h := range p.Ack {
			b = h.appendTo(v, b)
		}
	}
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	if v == V3 {
		return b
	}
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

// Verify checks a received OSPFv2 packet's authentication (raw: the IP
// payload the packet was decoded from). The cryptographic sequence number
// is checked by the neighbour (it must not decrease). OSPFv3 packets have
// none (true).
func (a *Auth) Verify(raw []byte, p *Packet) bool {
	if p.V == V3 {
		return true
	}
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

// ipChecksum is the Internet checksum of an OSPFv2 packet; skipAuth
// leaves out the 64-bit authentication field (RFC 2328 D.4.1). Over a
// packet with a correct checksum it returns 0.
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

// Checksum6 computes the OSPFv3 checksum with the IPv6 pseudo header
// (RFC 5340 A.3.1), for packets that do not pass a raw socket with
// IPV6_CHECKSUM (tests, relayed frames). b's checksum field must be zero.
func Checksum6(src, dst [16]byte, b []byte) uint16 {
	var sum uint32
	add := func(x []byte) {
		for i := 0; i+1 < len(x); i += 2 {
			sum += uint32(x[i])<<8 | uint32(x[i+1])
		}
		if len(x)%2 == 1 {
			sum += uint32(x[len(x)-1]) << 8
		}
	}
	add(src[:])
	add(dst[:])
	var l [8]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(b)))
	l[7] = ProtoOSPF
	add(l[:])
	add(b)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
