// Package lacp implements IEEE 802.1AX link aggregation control (LACP):
// the LACPDU format and the per-port state machines (receive, periodic
// transmission, selection, mux with coupled control, transmit), driven by
// explicit time so that they can be tested without real timers. The I/O
// (AF_PACKET) and the kernel side (which ports carry traffic) live outside.
package lacp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
)

// EtherType and destination address of slow protocols (802.3 Annex 57A).
const (
	EtherType = 0x8809
	subtype   = 0x01 // LACP
	version   = 0x01
	pduLen    = 110 // after the EtherType
)

// Dest is the slow-protocols multicast address LACPDUs are sent to.
var Dest = net.HardwareAddr{0x01, 0x80, 0xc2, 0x00, 0x00, 0x02}

// State is a port state octet.
type State uint8

// State bits (802.1AX 6.4.2.3).
const (
	Activity    State = 1 << iota // active LACP
	Timeout                       // short timeout
	Aggregation                   // aggregatable
	Sync                          // in sync
	Collecting
	Distributing
	Defaulted
	Expired
)

func (s State) Has(b State) bool { return s&b != 0 }

func (s State) String() string {
	var out []string
	for i, n := range []string{"activity", "timeout", "aggregation", "sync", "collecting", "distributing", "defaulted", "expired"} {
		if s&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return strings.Join(out, ",")
}

// SystemID identifies an LACP system.
type SystemID struct {
	Priority uint16
	MAC      [6]byte
}

func (s SystemID) String() string {
	return fmt.Sprintf("%d,%s", s.Priority, net.HardwareAddr(s.MAC[:]))
}

// Info is the actor or partner information of an LACPDU.
type Info struct {
	System       SystemID
	Key          uint16
	PortPriority uint16
	Port         uint16
	State        State
}

// PDU is an LACPDU.
type PDU struct {
	Actor, Partner    Info
	CollectorMaxDelay uint16
}

func putInfo(b []byte, typ byte, i Info) {
	b[0], b[1] = typ, 20
	binary.BigEndian.PutUint16(b[2:], i.System.Priority)
	copy(b[4:10], i.System.MAC[:])
	binary.BigEndian.PutUint16(b[10:], i.Key)
	binary.BigEndian.PutUint16(b[12:], i.PortPriority)
	binary.BigEndian.PutUint16(b[14:], i.Port)
	b[16] = byte(i.State)
}

func getInfo(b []byte) Info {
	var i Info
	i.System.Priority = binary.BigEndian.Uint16(b[2:])
	copy(i.System.MAC[:], b[4:10])
	i.Key = binary.BigEndian.Uint16(b[10:])
	i.PortPriority = binary.BigEndian.Uint16(b[12:])
	i.Port = binary.BigEndian.Uint16(b[14:])
	i.State = State(b[16])
	return i
}

// Marshal returns the PDU without Ethernet header (starting with the
// subtype).
func (p *PDU) Marshal() []byte {
	b := make([]byte, pduLen)
	b[0], b[1] = subtype, version
	putInfo(b[2:22], 0x01, p.Actor)
	putInfo(b[22:42], 0x02, p.Partner)
	b[42], b[43] = 0x03, 16
	binary.BigEndian.PutUint16(b[44:], p.CollectorMaxDelay)
	// b[58], b[59]: terminator (0, 0); the rest is reserved (zero).
	return b
}

// Frame returns a complete Ethernet frame from src.
func (p *PDU) Frame(src net.HardwareAddr) []byte {
	f := make([]byte, 14, 14+pduLen)
	copy(f, Dest)
	copy(f[6:], src)
	binary.BigEndian.PutUint16(f[12:], EtherType)
	return append(f, p.Marshal()...)
}

var errNotLACP = errors.New("not an LACPDU")

// Unmarshal parses an LACPDU (starting with the subtype). Later versions
// are accepted as long as the version 1 fields are there (802.1AX 6.4.2.4).
func Unmarshal(b []byte) (*PDU, error) {
	if len(b) < 60 || b[0] != subtype {
		return nil, errNotLACP
	}
	if b[1] == 0 || b[2] != 0x01 || b[3] != 20 || b[22] != 0x02 || b[23] != 20 || b[42] != 0x03 || b[43] != 16 {
		return nil, fmt.Errorf("malformed LACPDU")
	}
	return &PDU{Actor: getInfo(b[2:22]), Partner: getInfo(b[22:42]), CollectorMaxDelay: binary.BigEndian.Uint16(b[44:])}, nil
}

// ParseFrame parses an Ethernet frame carrying an LACPDU.
func ParseFrame(f []byte) (*PDU, error) {
	if len(f) < 14 || binary.BigEndian.Uint16(f[12:]) != EtherType {
		return nil, errNotLACP
	}
	return Unmarshal(f[14:])
}
