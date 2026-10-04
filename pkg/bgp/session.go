package bgp

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// sessState is the state of one connection (RFC 4271 §8.2.2; Idle,
// Connect and Active are the peer's, before it has a connection).
type sessState uint8

const (
	sOpenSent sessState = iota
	sOpenConfirm
	sEstablished
)

// session is one TCP connection with a neighbour: a reader that hands
// messages to the event loop and a writer with a queue, so the loop never
// blocks on the network.
type session struct {
	p        *peer
	conn     net.Conn
	incoming bool
	state    sessState
	open     *peerOpen // the neighbour's OPEN (nil: not received)
	holdAt   time.Time // OPEN wait / hold timer

	mu     sync.Mutex
	queue  [][]byte
	wake   chan struct{}
	closed bool
}

func newSession(p *peer, c net.Conn, incoming bool) *session {
	return &session{p: p, conn: c, incoming: incoming, wake: make(chan struct{}, 1)}
}

func (s *session) remote() netip.Addr {
	if a, ok := s.conn.RemoteAddr().(*net.TCPAddr); ok {
		return addrOf(a.IP)
	}
	return netip.Addr{}
}

func (s *session) local() netip.Addr {
	if a, ok := s.conn.LocalAddr().(*net.TCPAddr); ok {
		return addrOf(a.IP)
	}
	return netip.Addr{}
}

// start runs the reader and the writer.
func (s *session) start(sp *Speaker) {
	go s.write()
	go s.read(sp)
}

// read delivers every message (or the end of the connection) to the loop.
func (s *session) read(sp *Speaker) {
	hdr := make([]byte, bgp.BGP_HEADER_LENGTH)
	for {
		if _, err := io.ReadFull(s.conn, hdr); err != nil {
			sp.do(func() { s.p.closed(s, err) })
			return
		}
		l := int(binary.BigEndian.Uint16(hdr[16:18]))
		if l < bgp.BGP_HEADER_LENGTH || l > maxMsgLen {
			sp.do(func() {
				s.p.fail(s, bgp.NewMessageError(bgp.BGP_ERROR_MESSAGE_HEADER_ERROR, bgp.BGP_ERROR_SUB_BAD_MESSAGE_LENGTH, hdr[16:18], "bad length").(*bgp.MessageError))
			})
			return
		}
		body := make([]byte, l-bgp.BGP_HEADER_LENGTH)
		if _, err := io.ReadFull(s.conn, body); err != nil {
			sp.do(func() { s.p.closed(s, err) })
			return
		}
		h := &bgp.BGPHeader{}
		_ = h.DecodeFromBytes(hdr)
		msg, err := bgp.ParseBGPBody(h, body)
		sp.do(func() { s.p.message(s, msg, err) })
	}
}

// send queues a message.
func (s *session) send(m *bgp.BGPMessage) {
	b, err := m.Serialize()
	if err != nil {
		s.p.sp.Log.Warn("bgp: message not encoded", "neighbor", s.p.n.Addr, "err", err)
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.queue = append(s.queue, b)
	}
	s.mu.Unlock()
	if m.Header.Type == bgp.BGP_MSG_UPDATE {
		s.p.stats.UpdatesOut++
	}
	s.p.stats.MsgsOut++
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *session) write() {
	for {
		<-s.wake
		s.mu.Lock()
		q := s.queue
		s.queue = nil
		closed := s.closed
		s.mu.Unlock()
		for _, b := range q {
			_ = s.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := s.conn.Write(b); err != nil {
				s.conn.Close()
				return
			}
		}
		if closed {
			s.conn.Close()
			return
		}
	}
}

// close ends the connection after the queued messages (a NOTIFICATION).
func (s *session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	// A writer stuck on a dead peer: the deadline ends it; make sure the
	// reader ends too.
	time.AfterFunc(2*time.Second, func() { s.conn.Close() })
}

// notify sends a NOTIFICATION and closes.
func (s *session) notify(code, sub uint8, data []byte) {
	s.send(bgp.NewBGPNotificationMessage(code, sub, data))
	s.close()
}
