//go:build linux

package link

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// EtherType of stacking frames (IEEE 802 local experimental).
const EtherType = 0x88b5

// PacketIO carries stacking frames on one port through an AF_PACKET
// socket bound to that port and EtherType only: nothing is received from,
// or sent to, any other port (reference 5.2: the stacking plane never
// listens on data ports).
type PacketIO struct {
	fd      int
	ifindex int
	mtu     int
	frames  chan []byte

	mu     sync.Mutex
	peer   net.HardwareAddr // learned from the first valid frame; broadcast until then
	once   sync.Once
	closed atomic.Bool
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// OpenPacket opens the stacking socket on a port.
func OpenPacket(ifname string) (*PacketIO, error) {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, int(htons(EtherType)))
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(EtherType), Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	// Ignore our own transmitted frames; wake up regularly so Close ends
	// the receiver.
	_ = unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_IGNORE_OUTGOING, 1)
	tv := unix.NsecToTimeval((200 * time.Millisecond).Nanoseconds())
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	p := &PacketIO{fd: fd, ifindex: ifi.Index, mtu: ifi.MTU, frames: make(chan []byte, 1024)}
	go p.recv()
	return p, nil
}

func (p *PacketIO) recv() {
	defer close(p.frames)
	buf := make([]byte, 65536)
	for {
		n, from, err := unix.Recvfrom(p.fd, buf, 0)
		if p.closed.Load() {
			return
		}
		if err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			return
		}
		ll, ok := from.(*unix.SockaddrLinklayer)
		if !ok || ll.Pkttype == unix.PACKET_OUTGOING || n < HeaderLen || binary.BigEndian.Uint16(buf) != magic {
			continue
		}
		mac := net.HardwareAddr(append([]byte(nil), ll.Addr[:ll.Halen]...))
		p.mu.Lock()
		if !bytes.Equal(p.peer, mac) {
			p.peer = mac // the cable is 1:1; the latest sender is the peer
		}
		p.mu.Unlock()
		select {
		case p.frames <- append([]byte(nil), buf[:n]...):
		default: // the link is behind: dropped, recovered by retransmission
		}
	}
}

// Send transmits a payload to the peer (broadcast until it is known).
func (p *PacketIO) Send(payload []byte) error {
	p.mu.Lock()
	dst := p.peer
	p.mu.Unlock()
	sa := &unix.SockaddrLinklayer{Protocol: htons(EtherType), Ifindex: p.ifindex, Halen: 6}
	if dst == nil {
		copy(sa.Addr[:], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	} else {
		copy(sa.Addr[:], dst)
	}
	return unix.Sendto(p.fd, payload, 0, sa)
}

// Frames delivers received payloads (Ethernet padding included).
func (p *PacketIO) Frames() <-chan []byte { return p.frames }

// MTU is the port's MTU.
func (p *PacketIO) MTU() int { return p.mtu }

// Peer returns the peer's MAC address once known.
func (p *PacketIO) Peer() net.HardwareAddr {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peer
}

// Close closes the socket.
func (p *PacketIO) Close() error {
	var err error
	p.once.Do(func() {
		p.closed.Store(true)
		err = unix.Close(p.fd)
	})
	return err
}
