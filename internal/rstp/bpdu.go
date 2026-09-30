// Package rstp implements the Rapid Spanning Tree Protocol (IEEE 802.1D-2004
// clause 17, with the dispute mechanism of 802.1Q) as pure logic: the caller
// feeds received BPDUs, link states and a one-second tick, and gets BPDUs to
// send, port states and FDB flushes through callbacks (reference 5.5).
package rstp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// BridgeID is priority (2 bytes, the low 12 bits are the system id
// extension, 0 here) and MAC address.
type BridgeID [8]byte

// MakeBridgeID builds a bridge id.
func MakeBridgeID(priority uint16, mac [6]byte) BridgeID {
	var b BridgeID
	binary.BigEndian.PutUint16(b[:], priority)
	copy(b[2:], mac[:])
	return b
}

func (b BridgeID) Priority() uint16 { return binary.BigEndian.Uint16(b[:]) }

// MAC is the bridge address.
func (b BridgeID) MAC() [6]byte {
	var m [6]byte
	copy(m[:], b[2:])
	return m
}

func (b BridgeID) String() string {
	return fmt.Sprintf("%d.%02x:%02x:%02x:%02x:%02x:%02x", b.Priority(), b[2], b[3], b[4], b[5], b[6], b[7])
}

// PortID is priority (4 bits) and port number (12 bits).
type PortID uint16

func MakePortID(priority, number uint16) PortID {
	return PortID(priority&0xf0)<<8 | PortID(number&0xfff)
}

func (p PortID) Priority() uint16 { return uint16(p>>8) & 0xf0 }
func (p PortID) Number() uint16   { return uint16(p) & 0xfff }
func (p PortID) String() string   { return fmt.Sprintf("%d.%d", p.Priority(), p.Number()) }

// Vector is a spanning tree priority vector (17.6).
type Vector struct {
	Root   BridgeID
	Cost   uint32
	Bridge BridgeID // designated bridge
	Port   PortID   // designated port
	RxPort PortID   // port of this bridge the vector was received on
}

func cmpBID(a, b BridgeID) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// cmp compares the first four components (lower is better).
func (a Vector) cmp(b Vector) int {
	if c := cmpBID(a.Root, b.Root); c != 0 {
		return c
	}
	if a.Cost != b.Cost {
		if a.Cost < b.Cost {
			return -1
		}
		return 1
	}
	if c := cmpBID(a.Bridge, b.Bridge); c != 0 {
		return c
	}
	if a.Port != b.Port {
		if a.Port < b.Port {
			return -1
		}
		return 1
	}
	return 0
}

// cmp5 also compares the receiving port (root path selection).
func (a Vector) cmp5(b Vector) int {
	if c := a.cmp(b); c != 0 {
		return c
	}
	if a.RxPort != b.RxPort {
		if a.RxPort < b.RxPort {
			return -1
		}
		return 1
	}
	return 0
}

// superior (17.6): a is better than b, or it comes from the same designated
// bridge address and port number (updated information from that port).
func superior(a, b Vector) bool {
	return a.cmp(b) < 0 || (a.Bridge.MAC() == b.Bridge.MAC() && a.Port.Number() == b.Port.Number() && a.cmp(b) != 0)
}

// Times are the timer values carried in BPDUs, in seconds.
type Times struct {
	MessageAge, MaxAge, HelloTime, ForwardDelay int
}

// BPDU types.
const (
	TypeConfig = 0x00
	TypeTCN    = 0x80
	TypeRST    = 0x02
)

// Flag bits.
const (
	flagTC       = 0x01
	flagProposal = 0x02
	roleShift    = 2
	flagLearning = 0x10
	flagForward  = 0x20
	flagAgree    = 0x40
	flagTCAck    = 0x80
)

// Encoded port roles.
const (
	encUnknown   = 0
	encAltBackup = 1
	encRoot      = 2
	encDesig     = 3
)

// BPDU is a decoded Configuration, TCN or RST BPDU.
type BPDU struct {
	Version  byte
	Type     byte
	Flags    byte
	Priority Vector // RxPort unused
	Times    Times
}

func (b *BPDU) role() int { return int(b.Flags>>roleShift) & 3 }

// LLC header of BPDUs (DSAP 0x42, SSAP 0x42, UI).
var llc = []byte{0x42, 0x42, 0x03}

// GroupMAC is the bridge group address BPDUs are sent to.
var GroupMAC = [6]byte{0x01, 0x80, 0xc2, 0x00, 0x00, 0x00}

// Marshal encodes the BPDU (without LLC).
func (b *BPDU) Marshal() []byte {
	if b.Type == TypeTCN {
		return []byte{0, 0, 0, TypeTCN}
	}
	n := 35
	if b.Type == TypeRST {
		n = 36
	}
	out := make([]byte, n)
	out[2], out[3], out[4] = b.Version, b.Type, b.Flags
	copy(out[5:13], b.Priority.Root[:])
	binary.BigEndian.PutUint32(out[13:], b.Priority.Cost)
	copy(out[17:25], b.Priority.Bridge[:])
	binary.BigEndian.PutUint16(out[25:], uint16(b.Priority.Port))
	for i, v := range []int{b.Times.MessageAge, b.Times.MaxAge, b.Times.HelloTime, b.Times.ForwardDelay} {
		binary.BigEndian.PutUint16(out[27+2*i:], uint16(v*256))
	}
	return out
}

var errBPDU = errors.New("invalid BPDU")

// Unmarshal decodes a BPDU (without LLC) and validates it (9.3.4).
func Unmarshal(p []byte) (*BPDU, error) {
	if len(p) < 4 || p[0] != 0 || p[1] != 0 {
		return nil, errBPDU
	}
	b := &BPDU{Version: p[2], Type: p[3]}
	switch {
	case b.Type == TypeTCN:
		return b, nil
	case b.Type == TypeConfig && len(p) >= 35:
	case b.Type == TypeRST && b.Version >= 2 && len(p) >= 36:
	default:
		return nil, errBPDU
	}
	b.Flags = p[4]
	copy(b.Priority.Root[:], p[5:13])
	b.Priority.Cost = binary.BigEndian.Uint32(p[13:])
	copy(b.Priority.Bridge[:], p[17:25])
	b.Priority.Port = PortID(binary.BigEndian.Uint16(p[25:]))
	t := make([]int, 4)
	for i := range t {
		t[i] = int(binary.BigEndian.Uint16(p[27+2*i:])+128) / 256
	}
	b.Times = Times{MessageAge: t[0], MaxAge: t[1], HelloTime: t[2], ForwardDelay: t[3]}
	if b.Type == TypeConfig && b.Times.MessageAge >= b.Times.MaxAge {
		return nil, errBPDU
	}
	if b.Type == TypeConfig {
		b.Flags &= flagTC | flagTCAck // a Config BPDU carries a designated role
		b.Flags |= encDesig << roleShift
	}
	return b, nil
}

// Frame builds an Ethernet 802.3/LLC frame with the BPDU from src.
func (b *BPDU) Frame(src [6]byte) []byte {
	pl := append(append([]byte{}, llc...), b.Marshal()...)
	f := make([]byte, 14, 14+len(pl))
	copy(f[0:6], GroupMAC[:])
	copy(f[6:12], src[:])
	binary.BigEndian.PutUint16(f[12:], uint16(len(pl)))
	f = append(f, pl...)
	for len(f) < 60 {
		f = append(f, 0)
	}
	return f
}

// ParseFrame extracts a BPDU from an Ethernet frame (nil, nil: not a BPDU).
func ParseFrame(f []byte) (*BPDU, error) {
	if len(f) < 17 || [6]byte(f[0:6]) != GroupMAC {
		return nil, nil
	}
	l := int(binary.BigEndian.Uint16(f[12:]))
	if l > 1500 || f[14] != 0x42 || f[15] != 0x42 || f[16] != 0x03 {
		return nil, nil
	}
	end := 14 + l
	if end > len(f) {
		end = len(f)
	}
	return Unmarshal(f[17:end])
}
