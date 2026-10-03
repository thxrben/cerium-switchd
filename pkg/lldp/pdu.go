// Package lldp implements the Link Layer Discovery Protocol (IEEE
// 802.1AB) for the stack (reference 5.5 protocols lldp): LLDPDUs with the
// chassis-wide identity, sent on every LLDP port of a member, and the
// neighbours learned from received ones.
package lldp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"unicode"
)

// EtherType and destination (nearest bridge) of LLDP frames.
const EtherType = 0x88cc

// Dest is the nearest-bridge group address; bridges never forward it.
var Dest = net.HardwareAddr{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e}

// TLV types.
const (
	tlvEnd       = 0
	tlvChassisID = 1
	tlvPortID    = 2
	tlvTTL       = 3
	tlvPortDesc  = 4
	tlvSysName   = 5
	tlvSysDesc   = 6
	tlvSysCap    = 7
	tlvMgmtAddr  = 8
	tlvOrg       = 127
)

// Subtypes of chassis and port IDs.
const (
	ChassisMAC    = 4
	ChassisIfName = 6
	PortIfName    = 5
	PortMAC       = 3
	PortLocal     = 7
)

// System capabilities.
const (
	CapBridge = 0x0004
	CapRouter = 0x0010
)

var (
	oui8021 = [3]byte{0x00, 0x80, 0xc2}
	oui8023 = [3]byte{0x00, 0x12, 0x0f}
)

// Agg is the 802.3 link aggregation TLV.
type Agg struct {
	Capable, Enabled bool
	PortID           uint32 // ifIndex of the aggregate (0: none)
}

// PDU is one LLDPDU.
type PDU struct {
	ChassisSubtype byte
	ChassisID      []byte
	PortSubtype    byte
	PortID         []byte
	TTL            uint16
	PortDesc       string
	SysName        string
	SysDesc        string
	Caps, Enabled  uint16
	MgmtAddrs      []netip.Addr
	PVID           uint16 // 0: not sent
	Agg            *Agg
	MaxFrame       uint16 // 0: not sent
}

func putTLV(b []byte, typ int, v []byte) []byte {
	h := uint16(typ)<<9 | uint16(len(v))
	b = binary.BigEndian.AppendUint16(b, h)
	return append(b, v...)
}

// maxString bounds string TLVs (the 9-bit length).
const maxString = 255

func str(s string) []byte {
	if len(s) > maxString {
		s = s[:maxString]
	}
	return []byte(s)
}

// Marshal encodes the TLVs (the frame payload).
func (p *PDU) Marshal() []byte {
	var b []byte
	b = putTLV(b, tlvChassisID, append([]byte{p.ChassisSubtype}, p.ChassisID...))
	b = putTLV(b, tlvPortID, append([]byte{p.PortSubtype}, p.PortID...))
	b = putTLV(b, tlvTTL, binary.BigEndian.AppendUint16(nil, p.TTL))
	if p.TTL == 0 {
		return putTLV(b, tlvEnd, nil) // shutdown LLDPDU
	}
	if p.PortDesc != "" {
		b = putTLV(b, tlvPortDesc, str(p.PortDesc))
	}
	if p.SysName != "" {
		b = putTLV(b, tlvSysName, str(p.SysName))
	}
	if p.SysDesc != "" {
		b = putTLV(b, tlvSysDesc, str(p.SysDesc))
	}
	if p.Caps != 0 {
		b = putTLV(b, tlvSysCap, binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, p.Caps), p.Enabled))
	}
	for _, a := range p.MgmtAddrs {
		sub := byte(1)
		if a.Is6() {
			sub = 2
		}
		raw := a.AsSlice()
		v := []byte{byte(len(raw) + 1), sub}
		v = append(v, raw...)
		v = append(v, 1, 0, 0, 0, 0, 0) // interface numbering unknown, number 0, no OID
		b = putTLV(b, tlvMgmtAddr, v)
	}
	if p.PVID != 0 {
		b = putTLV(b, tlvOrg, binary.BigEndian.AppendUint16(append(oui8021[:], 1), p.PVID))
	}
	if p.Agg != nil {
		st := byte(0)
		if p.Agg.Capable {
			st |= 1
		}
		if p.Agg.Enabled {
			st |= 2
		}
		b = putTLV(b, tlvOrg, binary.BigEndian.AppendUint32(append(oui8023[:], 3, st), p.Agg.PortID))
	}
	if p.MaxFrame != 0 {
		b = putTLV(b, tlvOrg, binary.BigEndian.AppendUint16(append(oui8023[:], 4), p.MaxFrame))
	}
	return putTLV(b, tlvEnd, nil)
}

// Frame returns the Ethernet frame sent from src.
func (p *PDU) Frame(src net.HardwareAddr) []byte {
	f := make([]byte, 0, 128)
	f = append(f, Dest...)
	f = append(f, src...)
	f = binary.BigEndian.AppendUint16(f, EtherType)
	f = append(f, p.Marshal()...)
	for len(f) < 60 {
		f = append(f, 0)
	}
	return f
}

// Errors of Parse.
var (
	ErrShort     = errors.New("lldp: truncated TLV")
	ErrMandatory = errors.New("lldp: chassis ID, port ID and TTL must come first")
)

// Parse decodes an LLDPDU (the frame payload after the EtherType). Unknown
// TLVs are skipped.
func Parse(b []byte) (*PDU, error) {
	p := &PDU{}
	n := 0
	for len(b) > 0 {
		if len(b) < 2 {
			return nil, ErrShort
		}
		h := binary.BigEndian.Uint16(b)
		typ, l := int(h>>9), int(h&0x1ff)
		if len(b) < 2+l {
			return nil, ErrShort
		}
		v := b[2 : 2+l]
		b = b[2+l:]
		// The first three TLVs are mandatory, in order.
		if n < 3 && typ != n+1 {
			return nil, ErrMandatory
		}
		n++
		switch typ {
		case tlvEnd:
			return p, nil
		case tlvChassisID:
			if l < 2 {
				return nil, ErrMandatory
			}
			p.ChassisSubtype, p.ChassisID = v[0], append([]byte(nil), v[1:]...)
		case tlvPortID:
			if l < 2 {
				return nil, ErrMandatory
			}
			p.PortSubtype, p.PortID = v[0], append([]byte(nil), v[1:]...)
		case tlvTTL:
			if l < 2 {
				return nil, ErrMandatory
			}
			p.TTL = binary.BigEndian.Uint16(v)
		case tlvPortDesc:
			p.PortDesc = string(v)
		case tlvSysName:
			p.SysName = string(v)
		case tlvSysDesc:
			p.SysDesc = string(v)
		case tlvSysCap:
			if l >= 4 {
				p.Caps, p.Enabled = binary.BigEndian.Uint16(v), binary.BigEndian.Uint16(v[2:])
			}
		case tlvMgmtAddr:
			if l >= 2 && int(v[0]) >= 1 && 1+int(v[0]) <= l {
				if a, ok := netip.AddrFromSlice(v[2 : 1+int(v[0])]); ok && (v[1] == 1 || v[1] == 2) {
					p.MgmtAddrs = append(p.MgmtAddrs, a)
				}
			}
		case tlvOrg:
			if l < 4 {
				continue
			}
			oui, sub, d := [3]byte(v[:3]), v[3], v[4:]
			switch {
			case oui == oui8021 && sub == 1 && len(d) >= 2:
				p.PVID = binary.BigEndian.Uint16(d)
			case oui == oui8023 && sub == 3 && len(d) >= 5:
				p.Agg = &Agg{Capable: d[0]&1 != 0, Enabled: d[0]&2 != 0, PortID: binary.BigEndian.Uint32(d[1:])}
			case oui == oui8023 && sub == 4 && len(d) >= 2:
				p.MaxFrame = binary.BigEndian.Uint16(d)
			}
		}
	}
	if n < 3 {
		return nil, ErrMandatory
	}
	return p, nil
}

// IDString renders a chassis or port ID for display.
func IDString(subtype byte, id []byte, chassis bool) string {
	isMAC := (chassis && subtype == ChassisMAC) || (!chassis && subtype == PortMAC)
	if isMAC && len(id) == 6 {
		return net.HardwareAddr(id).String()
	}
	if chassis && subtype == 5 || !chassis && subtype == 4 { // network address
		if len(id) > 1 {
			if a, ok := netip.AddrFromSlice(id[1:]); ok {
				return a.String()
			}
		}
	}
	s := string(id)
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0 {
		return s
	}
	return fmt.Sprintf("%x", id)
}

// CapString renders capability bits ("Bridge Router").
func CapString(c uint16) string {
	names := []string{"Other", "Repeater", "Bridge", "WLAN-AP", "Router", "Telephone", "DOCSIS", "Station"}
	var out []string
	for i, n := range names {
		if c&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, " ")
}
