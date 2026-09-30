// Package link implements the reliable stream over one stacking cable
// (docs/stack-protocol.md): ordered, loss-free delivery of bytes between
// two members without IP, exposed as a net.Conn for TLS on top.
package link

import (
	"encoding/binary"
	"errors"
)

// Frame types.
const (
	tHello = 1
	tData  = 2
	tAck   = 3
	tReset = 4
	tBFD   = 5
	// Path MTU probing (docs/stack-protocol.md "Path MTU"): a probe is
	// padded to the size under test, its reply is small and names the size.
	tProbe      = 6
	tProbeReply = 7
)

const (
	magic     = 0x5354
	version   = 1
	HeaderLen = 24
)

// frame is a decoded stacking frame.
type frame struct {
	Type      byte
	Epoch     uint32 // sender epoch
	PeerEpoch uint32 // receiver epoch as seen by the sender (0 = unknown)
	Seq       uint32
	Ack       uint32
	Window    uint32 // bytes
	Payload   []byte
}

var errBadFrame = errors.New("malformed stacking frame")

func (f *frame) encode() []byte {
	b := make([]byte, HeaderLen+len(f.Payload))
	be := binary.BigEndian
	be.PutUint16(b[0:], magic)
	b[2] = version
	b[3] = f.Type
	be.PutUint32(b[4:], f.Epoch)
	be.PutUint32(b[8:], f.PeerEpoch)
	be.PutUint32(b[12:], f.Seq)
	be.PutUint32(b[16:], f.Ack)
	w := f.Window / 64
	if w > 0xffff {
		w = 0xffff
	}
	be.PutUint16(b[20:], uint16(w))
	be.PutUint16(b[22:], uint16(len(f.Payload)))
	copy(b[HeaderLen:], f.Payload)
	return b
}

func decode(b []byte) (*frame, error) {
	be := binary.BigEndian
	if len(b) < HeaderLen || be.Uint16(b[0:]) != magic || b[2] != version {
		return nil, errBadFrame
	}
	n := int(be.Uint16(b[22:]))
	if len(b) < HeaderLen+n {
		return nil, errBadFrame // Ethernet padding may add bytes, never remove them
	}
	f := &frame{Type: b[3], Epoch: be.Uint32(b[4:]), PeerEpoch: be.Uint32(b[8:]), Seq: be.Uint32(b[12:]),
		Ack: be.Uint32(b[16:]), Window: uint32(be.Uint16(b[20:])) * 64}
	if f.Type < tHello || f.Type > tProbeReply || f.Epoch == 0 {
		return nil, errBadFrame
	}
	f.Payload = append([]byte(nil), b[HeaderLen:HeaderLen+n]...)
	return f, nil
}

// seqLess compares sequence numbers with wrap-around.
func seqLess(a, b uint32) bool { return int32(a-b) < 0 }
