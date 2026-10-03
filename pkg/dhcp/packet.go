// Package dhcp is switchd's DHCPv4 client for `family inet dhcp`
// (reference 5.3.2): packet format, the RFC 2131 client state machine as
// pure logic, and a Linux runner on raw sockets.
package dhcp

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"time"
)

// Message types (option 53).
const (
	Discover = 1
	Offer    = 2
	Request  = 3
	Decline  = 4
	Ack      = 5
	Nak      = 6
	Release  = 7
)

// Options used.
const (
	optSubnetMask = 1
	optRouter     = 3
	optDNS        = 6
	optHostName   = 12
	optDomainName = 15
	optRequested  = 50
	optLeaseTime  = 51
	optMsgType    = 53
	optServerID   = 54
	optParams     = 55
	optRenewal    = 58
	optRebinding  = 59
	optClientID   = 61
	optEnd        = 255
	optPad        = 0
)

var magic = []byte{99, 130, 83, 99}

// Packet is a DHCP message (the fields the client uses).
type Packet struct {
	Op        byte // 1 request, 2 reply
	XID       uint32
	Secs      uint16
	Broadcast bool
	CIAddr    netip.Addr
	YIAddr    netip.Addr
	SIAddr    netip.Addr
	GIAddr    netip.Addr
	CHAddr    [6]byte
	Options   map[byte][]byte
}

// Type returns the message type.
func (p *Packet) Type() byte {
	if v := p.Options[optMsgType]; len(v) == 1 {
		return v[0]
	}
	return 0
}

func addr4(b []byte) netip.Addr {
	if len(b) < 4 {
		return netip.Addr{}
	}
	return netip.AddrFrom4([4]byte(b[:4]))
}

func (p *Packet) addrOpt(o byte) netip.Addr { return addr4(p.Options[o]) }

func (p *Packet) addrsOpt(o byte) []netip.Addr {
	var out []netip.Addr
	v := p.Options[o]
	for len(v) >= 4 {
		out = append(out, addr4(v))
		v = v[4:]
	}
	return out
}

func (p *Packet) secsOpt(o byte) time.Duration {
	if v := p.Options[o]; len(v) == 4 {
		return time.Duration(binary.BigEndian.Uint32(v)) * time.Second
	}
	return 0
}

func put4(b []byte, a netip.Addr) {
	if a.Is4() {
		v := a.As4()
		copy(b, v[:])
	}
}

// Marshal encodes the message (BOOTP header, magic cookie, options).
func (p *Packet) Marshal() []byte {
	b := make([]byte, 240, 300)
	b[0], b[1], b[2] = p.Op, 1, 6
	binary.BigEndian.PutUint32(b[4:], p.XID)
	binary.BigEndian.PutUint16(b[8:], p.Secs)
	if p.Broadcast {
		b[10] = 0x80
	}
	put4(b[12:], p.CIAddr)
	put4(b[16:], p.YIAddr)
	put4(b[20:], p.SIAddr)
	put4(b[24:], p.GIAddr)
	copy(b[28:], p.CHAddr[:])
	copy(b[236:], magic)
	// Message type first (some servers expect it).
	keys := []byte{optMsgType}
	for k := range p.Options {
		if k != optMsgType {
			keys = append(keys, k)
		}
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 1 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	for _, k := range keys {
		v, ok := p.Options[k]
		if !ok {
			continue
		}
		b = append(b, k, byte(len(v)))
		b = append(b, v...)
	}
	b = append(b, optEnd)
	for len(b) < 300 {
		b = append(b, optPad) // BOOTP minimum size
	}
	return b
}

var errPacket = errors.New("invalid DHCP message")

// Unmarshal decodes a message.
func Unmarshal(b []byte) (*Packet, error) {
	if len(b) < 240 || string(b[236:240]) != string(magic) || b[1] != 1 || b[2] != 6 {
		return nil, errPacket
	}
	p := &Packet{Op: b[0], XID: binary.BigEndian.Uint32(b[4:]), Secs: binary.BigEndian.Uint16(b[8:]),
		Broadcast: b[10]&0x80 != 0, CIAddr: addr4(b[12:]), YIAddr: addr4(b[16:]), SIAddr: addr4(b[20:]),
		GIAddr: addr4(b[24:]), Options: map[byte][]byte{}}
	copy(p.CHAddr[:], b[28:34])
	o := b[240:]
	for len(o) > 0 {
		k := o[0]
		if k == optEnd {
			break
		}
		if k == optPad {
			o = o[1:]
			continue
		}
		if len(o) < 2 || len(o) < 2+int(o[1]) {
			return nil, errPacket
		}
		p.Options[k] = append(p.Options[k], o[2:2+int(o[1])]...) // long options are concatenated (RFC 3396)
		o = o[2+int(o[1]):]
	}
	return p, nil
}
