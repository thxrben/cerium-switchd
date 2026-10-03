package mesh

import (
	"encoding/binary"
	"fmt"
	"time"
)

// Reliable streams (docs/stack-protocol.md, "Mesh"): a stream survives a
// change of its path (a stacking cable cut, a member taking another route):
// the sender keeps every message until it is acknowledged and sends it
// again after a timeout or a duplicate acknowledgment; the receiver puts
// messages that overtook others back in order and drops duplicates. A lost
// acknowledgment is repaired by the next one (they carry the next expected
// sequence number and the window's right edge as absolute values), and a
// sender waiting for window probes. The stream fails only when its member
// stays unreachable, or data stays unacknowledged, for longer than
// Mesh.GiveUp (30 s).
//
// Streams of members that do not know them (an older release during a
// rolling update) are opened as before (tOpen): the dialer tries tOpen2
// first and falls back when no answer comes within legacyProbe.

const (
	tOpen2  = 7
	tData2  = 8
	tAck2   = 9 // seq: the next expected message; payload: the window's right edge (bytes, uint64)
	tClose2 = 10
	tProbe2 = 11 // a sender waiting for window: answer with tAck2

	// rtoMin/rtoMax bound the retransmission timeout.
	rtoMin = 100 * time.Millisecond
	rtoMax = 2 * time.Second
	// legacyProbe: no answer to tOpen2 within this time: the member is
	// asked with tOpen (and remembered for legacyTTL).
	legacyProbe = time.Second
	legacyTTL   = time.Minute
	// maxOutOfOrder bounds the messages a receiver keeps out of order.
	maxOutOfOrder = 4096
	// retransmitTick is how often the timers are checked.
	retransmitTick = 50 * time.Millisecond
)

// DefaultGiveUp is how long a reliable stream waits for its member to be
// reachable again, or for an acknowledgment (Mesh.GiveUp overrides it).
const DefaultGiveUp = 30 * time.Second

func (m *Mesh) giveUp() time.Duration {
	if m.GiveUp > 0 {
		return m.GiveUp
	}
	return DefaultGiveUp
}

// pendingMsg is a sent message not acknowledged yet.
type pendingMsg struct {
	seq     uint32
	typ     byte // tData2 or tClose2
	payload []byte
	first   time.Time // first sent
}

// relState is a reliable stream's state (guarded by Stream.mu).
type relState struct {
	// Sending.
	pend     []pendingMsg // in sequence order
	edge     uint64       // bytes the receiver accepts in total
	sent     uint64       // bytes sent in total
	lastAck  uint32
	dups     int
	rto      time.Duration
	lastTx   time.Time // last (re)transmission of pend
	probeAt  time.Time
	unreachT time.Time // when the member became unreachable (zero: reachable)
	// Receiving.
	ooo      map[uint32]oooMsg
	consumed uint64 // bytes read by the user in total
	advEdge  uint64 // the right edge last advertised
}

type oooMsg struct {
	typ     byte
	payload []byte
}

func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// ackLocked sends the stream's acknowledgment (m.mu and s.mu held).
func (m *Mesh) ackLocked(s *Stream) {
	r := &s.rel
	r.advEdge = r.consumed + Window
	m.sendLocked(&msg{typ: tAck2, hops: maxHops, src: byte(m.Self), dst: byte(s.key.member), stream: s.key.id,
		seq: s.recvSeq, payload: be64(r.advEdge)})
}

// deliverReliableLocked handles the reliable stream messages (m.mu held).
func (m *Mesh) deliverReliableLocked(x *msg, s *Stream) {
	switch x.typ {
	case tData2, tClose2:
		s.mu.Lock()
		r := &s.rel
		switch d := x.seq - s.recvSeq; {
		case x.seq == s.recvSeq:
			s.takeLocked(x.typ, x.payload)
			for {
				o, ok := r.ooo[s.recvSeq]
				if !ok {
					break
				}
				delete(r.ooo, s.recvSeq)
				s.takeLocked(o.typ, o.payload)
			}
		case d < 1<<31 && len(r.ooo) < maxOutOfOrder: // ahead: keep for later
			if r.ooo == nil {
				r.ooo = map[uint32]oooMsg{}
			}
			r.ooo[x.seq] = oooMsg{x.typ, x.payload}
		}
		// In order, a duplicate (behind) or a gap: the acknowledgment says
		// what is expected next.
		m.ackLocked(s)
		s.cond.Broadcast()
		done := s.eof && s.closed && len(r.pend) == 0
		s.mu.Unlock()
		if done {
			delete(m.streams, s.key)
		}
	case tAck2:
		if len(x.payload) < 8 {
			return
		}
		edge := binary.BigEndian.Uint64(x.payload)
		s.mu.Lock()
		r := &s.rel
		if edge > r.edge {
			r.edge = edge
		}
		acked := 0
		for acked < len(r.pend) && int32(r.pend[acked].seq-x.seq) < 0 {
			acked++
		}
		resend := false
		if acked > 0 {
			r.pend = r.pend[acked:]
			r.dups, r.rto = 0, rtoMin
		} else if x.seq == r.lastAck && len(r.pend) > 0 {
			// The receiver still waits for the same message: it was lost
			// (fast retransmission after two duplicates).
			r.dups++
			resend = r.dups == 2
		}
		r.lastAck = x.seq
		var again []pendingMsg
		if resend {
			again = append(again, r.pend[0])
		}
		done := s.closed && s.eof && len(r.pend) == 0
		s.cond.Broadcast()
		s.mu.Unlock()
		for _, p := range again {
			m.sendLocked(&msg{typ: p.typ, hops: maxHops, src: byte(m.Self), dst: byte(s.key.member), stream: s.key.id, seq: p.seq, payload: p.payload})
		}
		if done {
			delete(m.streams, s.key)
		}
	case tProbe2:
		s.mu.Lock()
		m.ackLocked(s)
		s.mu.Unlock()
	}
}

// takeLocked delivers one in-order message to the reader (s.mu held).
func (s *Stream) takeLocked(typ byte, payload []byte) {
	s.recvSeq++
	if typ == tClose2 {
		s.eof = true
		return
	}
	s.rbuf = append(s.rbuf, payload...)
}

// retransmit runs the timers of the reliable streams: messages not
// acknowledged within the timeout go again, a waiting sender probes, and a
// stream gives up after GiveUp.
func (m *Mesh) retransmit(now time.Time) (more bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	defer func() {
		if !more {
			m.timerOn = false
		}
	}()
	for k, s := range m.streams {
		if !s.reliable {
			continue
		}
		more = true
		_, reach := m.routes[k.member]
		s.mu.Lock()
		r := &s.rel
		switch {
		case reach:
			r.unreachT = time.Time{}
		case r.unreachT.IsZero():
			r.unreachT = now
		}
		var fail error
		switch {
		case !r.unreachT.IsZero() && now.Sub(r.unreachT) > m.giveUp():
			fail = fmt.Errorf("member %d is no longer reachable", k.member)
		case len(r.pend) > 0 && now.Sub(r.pend[0].first) > m.giveUp():
			fail = fmt.Errorf("member %d did not acknowledge for %s", k.member, m.giveUp())
		}
		if fail != nil {
			s.mu.Unlock()
			delete(m.streams, k)
			m.sendLocked(&msg{typ: tReset, hops: maxHops, src: byte(m.Self), dst: byte(k.member), stream: k.id})
			s.fail(fail)
			continue
		}
		var resend []pendingMsg
		if len(r.pend) > 0 && reach && now.Sub(r.lastTx) >= r.rto {
			resend = append(resend, r.pend...)
			r.lastTx = now
			r.rto = min(2*r.rto, rtoMax)
		}
		probe := len(r.pend) == 0 && r.sent >= r.edge && !s.closed && reach && now.Sub(r.probeAt) >= 4*rtoMin
		if probe {
			r.probeAt = now
		}
		s.mu.Unlock()
		for _, p := range resend {
			m.sendLocked(&msg{typ: p.typ, hops: maxHops, src: byte(m.Self), dst: byte(k.member), stream: k.id, seq: p.seq, payload: p.payload})
		}
		if probe {
			m.sendLocked(&msg{typ: tProbe2, hops: maxHops, src: byte(m.Self), dst: byte(k.member), stream: k.id})
		}
	}
	return more
}

// writeReliable sends p on a reliable stream.
func (s *Stream) writeReliable(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return written, errClosed
		}
		r := &s.rel
		if err := s.wait(func() bool { return r.sent < r.edge }, s.wdl); err != nil {
			s.mu.Unlock()
			return written, err
		}
		n := min(len(p)-written, int(r.edge-r.sent), maxPayload)
		seq := s.sendSeq
		s.sendSeq++
		r.sent += uint64(n)
		payload := append([]byte(nil), p[written:written+n]...)
		now := time.Now()
		if len(r.pend) == 0 {
			r.lastTx = now
		}
		r.pend = append(r.pend, pendingMsg{seq: seq, typ: tData2, payload: payload, first: now})
		s.mu.Unlock()
		s.m.mu.Lock()
		// No route right now: the message waits for the retransmission.
		s.m.sendLocked(&msg{typ: tData2, hops: maxHops, src: byte(s.m.Self), dst: byte(s.key.member), stream: s.key.id, seq: seq, payload: payload})
		s.m.mu.Unlock()
		written += n
	}
	return written, nil
}

// readGrantReliable advertises a larger window after a read (s.mu not
// held).
func (s *Stream) readGrantReliable() {
	s.m.mu.Lock()
	s.mu.Lock()
	if s.rel.consumed+Window-s.rel.advEdge >= Window/4 {
		s.m.ackLocked(s)
	}
	s.mu.Unlock()
	s.m.mu.Unlock()
}

// closeReliable sends the end of the stream in sequence.
func (s *Stream) closeReliable() {
	s.mu.Lock()
	seq := s.sendSeq
	s.sendSeq++
	now := time.Now()
	if len(s.rel.pend) == 0 {
		s.rel.lastTx = now
	}
	s.rel.pend = append(s.rel.pend, pendingMsg{seq: seq, typ: tClose2, first: now})
	s.mu.Unlock()
	s.m.mu.Lock()
	s.m.sendLocked(&msg{typ: tClose2, hops: maxHops, src: byte(s.m.Self), dst: byte(s.key.member), stream: s.key.id, seq: seq})
	// Forget it after a while even if the other side went away.
	key := s.key
	time.AfterFunc(s.m.giveUp()+5*time.Second, func() { s.m.drop(key) })
	s.m.mu.Unlock()
}

// timerLocked starts the retransmission timer; it ends when no reliable
// stream is left (m.mu held).
func (m *Mesh) timerLocked() {
	if m.timerOn {
		return
	}
	m.timerOn = true
	go func() {
		t := time.NewTicker(retransmitTick)
		defer t.Stop()
		for now := range t.C {
			if !m.retransmit(now) {
				return
			}
		}
	}()
}
