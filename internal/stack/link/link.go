package link

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"
)

// FrameIO sends and receives the payload of stacking frames on one cable
// (the Ethernet header and EtherType are handled below it).
type FrameIO interface {
	Send(payload []byte) error
	// Frames delivers received payloads. It is shared by successive Link
	// instances on the same cable.
	Frames() <-chan []byte
	// MTU is the largest payload per frame (the port's MTU, e.g. 1500).
	MTU() int
}

// Options tune a link; zero values select the defaults.
type Options struct {
	RecvWindow    int           // bytes buffered for Read; default 256 KiB
	SendBuffer    int           // bytes buffered for Write; default 1 MiB
	HelloInterval time.Duration // default 100 ms
	// Liveness (BFD-style, reference 5.2 virtual-chassis bfd): a frame at
	// least every Interval, the link ends after Interval x Multiplier
	// without frames. Both sides use the larger values. Defaults 100 ms, 3.
	Interval   time.Duration
	Multiplier int
	Name       string // for Addr, e.g. "0/2"
}

// Errors that end a link.
var (
	ErrPeerRestarted = errors.New("stacking peer restarted")
	ErrPeerReset     = errors.New("stacking peer closed the link")
	ErrDead          = errors.New("stacking link lost (no frames from the peer)")
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "stacking link: i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

const (
	tick       = 5 * time.Millisecond
	ackDelay   = 10 * time.Millisecond
	minRTO     = 10 * time.Millisecond
	initialRTO = 50 * time.Millisecond
	maxRTO     = time.Second
)

// Link is one stacking cable's reliable stream. It implements net.Conn.
type Link struct {
	io FrameIO
	o  Options

	mu     sync.Mutex
	cond   *sync.Cond
	epoch  uint32
	peer   uint32 // peer epoch (0 = not seen yet)
	up     bool
	closed bool
	err    error

	// Send side: sbuf holds the bytes from sndUna on (sent or not).
	sbuf           []byte
	sndUna, sndNxt uint32
	sndMax         uint32
	peerWin        uint32
	rto            time.Duration
	srtt, rttvar   time.Duration
	rtoAt          time.Time // retransmission deadline (zero: nothing in flight)
	timedSeq       uint32    // RTT sample: sequence end being timed
	timedAt        time.Time // zero: none (Karn: not after a retransmission)
	dupAcks        int
	lastSent       time.Time
	lastHello      time.Time
	interval       time.Duration // effective liveness interval
	multiplier     int

	// Receive side.
	rcvNxt     uint32
	rbuf       []byte
	ackPending int
	ackAt      time.Time
	lastRecv   time.Time

	readDL, writeDL time.Time
	upCh            chan struct{}
	done            chan struct{}
}

// New starts a link on io. The link is usable (Write buffers, Read waits)
// immediately; Up reports when the handshake completed.
func New(io FrameIO, o Options) *Link {
	if o.RecvWindow == 0 {
		o.RecvWindow = 256 << 10
	}
	if o.SendBuffer == 0 {
		o.SendBuffer = 1 << 20
	}
	if o.HelloInterval == 0 {
		o.HelloInterval = 100 * time.Millisecond
	}
	if o.Interval == 0 {
		o.Interval = 100 * time.Millisecond
	}
	if o.Multiplier == 0 {
		o.Multiplier = 3
	}
	l := &Link{io: io, o: o, rto: initialRTO, upCh: make(chan struct{}), done: make(chan struct{}), peerWin: 64 << 10,
		interval: o.Interval, multiplier: o.Multiplier}
	l.cond = sync.NewCond(&l.mu)
	var b [4]byte
	for l.epoch == 0 {
		_, _ = rand.Read(b[:])
		l.epoch = binary.BigEndian.Uint32(b[:])
	}
	go l.loop()
	return l
}

// Up is closed when the handshake with the peer completed.
func (l *Link) Up() <-chan struct{} { return l.upCh }

// Done is closed when the link ended; Err tells why.
func (l *Link) Done() <-chan struct{} { return l.done }

// Err returns why the link ended (nil while it runs).
func (l *Link) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *Link) loop() {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case b, ok := <-l.io.Frames():
			if !ok {
				l.fail(ErrDead)
				return
			}
			l.receive(b)
		case <-t.C:
			l.timers()
		}
	}
}

// fail ends the link. Caller must not hold l.mu.
func (l *Link) fail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failLocked(err)
}

func (l *Link) failLocked(err error) {
	if l.closed {
		return
	}
	l.closed, l.err = true, err
	close(l.done)
	l.cond.Broadcast()
}

// send transmits a frame; caller holds l.mu (sending on a cable never
// blocks for long).
func (l *Link) send(typ byte, seq uint32, payload []byte) {
	if typ == tHello {
		payload = make([]byte, 4)
		binary.BigEndian.PutUint16(payload, uint16(min(l.o.Interval.Milliseconds(), 0xffff)))
		binary.BigEndian.PutUint16(payload[2:], uint16(min(l.o.Multiplier, 0xffff)))
	}
	f := frame{Type: typ, Epoch: l.epoch, PeerEpoch: l.peer, Seq: seq, Ack: l.rcvNxt, Payload: payload}
	if w := l.o.RecvWindow - len(l.rbuf); w > 0 {
		f.Window = uint32(w)
	}
	if typ != tHello {
		l.ackPending = 0
	}
	l.lastSent = time.Now()
	_ = l.io.Send(f.encode()) // a lost frame is recovered by retransmission
}

func (l *Link) receive(b []byte) {
	f, err := decode(b)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	now := time.Now()
	switch {
	case l.peer != 0 && f.Epoch != l.peer:
		l.failLocked(ErrPeerRestarted)
		return
	case f.PeerEpoch != 0 && f.PeerEpoch != l.epoch:
		// The peer still talks to our previous instance: show it the new
		// epoch, it restarts its side.
		l.send(tHello, 0, nil)
		return
	}
	l.peer = f.Epoch
	l.lastRecv = now
	if f.Type == tHello && len(f.Payload) >= 4 {
		pi := time.Duration(binary.BigEndian.Uint16(f.Payload)) * time.Millisecond
		pm := int(binary.BigEndian.Uint16(f.Payload[2:]))
		l.interval, l.multiplier = max(l.o.Interval, pi), max(l.o.Multiplier, pm)
	}
	if f.Type == tReset {
		l.failLocked(ErrPeerReset)
		return
	}
	if !l.up {
		if f.PeerEpoch != l.epoch {
			l.send(tHello, 0, nil) // it does not know us yet
			return
		}
		l.up = true
		close(l.upCh)
		l.cond.Broadcast()
	}
	l.peerWin = f.Window
	l.handleAck(f, now)
	if f.Type == tData && len(f.Payload) > 0 {
		room := l.o.RecvWindow - len(l.rbuf)
		if f.Seq == l.rcvNxt && len(f.Payload) <= room {
			l.rbuf = append(l.rbuf, f.Payload...)
			l.rcvNxt += uint32(len(f.Payload))
			l.cond.Broadcast()
			l.ackPending++
			if l.ackPending >= 2 {
				l.send(tAck, l.sndNxt, nil)
			} else {
				l.ackAt = now.Add(ackDelay)
			}
		} else {
			// Out of order, duplicate or no room: tell the sender where we are.
			l.send(tAck, l.sndNxt, nil)
		}
	}
	l.transmit(now)
}

func (l *Link) handleAck(f *frame, now time.Time) {
	switch {
	case seqLess(l.sndUna, f.Ack) && !seqLess(l.sndMax, f.Ack):
		n := f.Ack - l.sndUna
		l.sbuf = l.sbuf[n:]
		l.sndUna = f.Ack
		if seqLess(l.sndNxt, l.sndUna) {
			l.sndNxt = l.sndUna
		}
		if !l.timedAt.IsZero() && !seqLess(f.Ack, l.timedSeq) {
			l.rttSample(now.Sub(l.timedAt))
			l.timedAt = time.Time{}
		}
		l.dupAcks = 0
		if l.sndUna == l.sndMax {
			l.rtoAt = time.Time{}
		} else {
			l.rtoAt = now.Add(l.rto)
		}
		l.cond.Broadcast() // room for writers
	case f.Ack == l.sndUna && l.sndUna != l.sndMax && f.Type == tAck:
		l.dupAcks++
		if l.dupAcks == 3 {
			l.sndNxt = l.sndUna // fast retransmit
			l.timedAt = time.Time{}
		}
	}
}

// rttSample updates the retransmission timeout (RFC 6298).
func (l *Link) rttSample(r time.Duration) {
	if l.srtt == 0 {
		l.srtt, l.rttvar = r, r/2
	} else {
		d := l.srtt - r
		if d < 0 {
			d = -d
		}
		l.rttvar = (3*l.rttvar + d) / 4
		l.srtt = (7*l.srtt + r) / 8
	}
	l.rto = min(max(l.srtt+4*l.rttvar, minRTO), maxRTO)
}

// transmit sends new data within the peer's window. Caller holds l.mu.
func (l *Link) transmit(now time.Time) {
	if !l.up {
		return
	}
	chunk := l.io.MTU() - HeaderLen
	for {
		off := int(l.sndNxt - l.sndUna)
		inFlight := l.sndNxt - l.sndUna
		if off >= len(l.sbuf) || inFlight >= l.peerWin {
			return
		}
		n := min(chunk, len(l.sbuf)-off, int(l.peerWin-inFlight))
		if n <= 0 {
			return
		}
		l.send(tData, l.sndNxt, l.sbuf[off:off+n])
		if l.timedAt.IsZero() && l.sndNxt == l.sndMax {
			l.timedSeq, l.timedAt = l.sndNxt+uint32(n), now
		}
		l.sndNxt += uint32(n)
		if seqLess(l.sndMax, l.sndNxt) {
			l.sndMax = l.sndNxt
		}
		if l.rtoAt.IsZero() {
			l.rtoAt = now.Add(l.rto)
		}
	}
}

func (l *Link) timers() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	now := time.Now()
	if !l.up {
		if now.Sub(l.lastHello) >= l.o.HelloInterval {
			l.lastHello = now
			l.send(tHello, 0, nil)
		}
		return
	}
	if now.Sub(l.lastRecv) > l.interval*time.Duration(l.multiplier) {
		l.failLocked(ErrDead)
		return
	}
	if !l.rtoAt.IsZero() && now.After(l.rtoAt) {
		l.sndNxt = l.sndUna // go back and resend
		l.rto = min(2*l.rto, maxRTO)
		l.timedAt = time.Time{}
		l.rtoAt = now.Add(l.rto)
	}
	l.transmit(now)
	switch {
	case l.ackPending > 0 && now.After(l.ackAt):
		l.send(tAck, l.sndNxt, nil)
	case now.Sub(l.lastSent) >= l.interval-tick:
		l.send(tAck, l.sndNxt, nil) // liveness, also re-advertises the window
	}
}

// wait blocks until ready() or the link ends or the deadline passes.
// Caller holds l.mu.
func (l *Link) wait(ready func() bool, dl time.Time) error {
	for !ready() {
		if l.closed {
			return l.err
		}
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return timeoutError{}
			}
			t := time.AfterFunc(d, func() {
				l.mu.Lock()
				l.cond.Broadcast()
				l.mu.Unlock()
			})
			l.cond.Wait()
			t.Stop()
			continue
		}
		l.cond.Wait()
	}
	return nil
}

// Read reads stream bytes.
func (l *Link) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.wait(func() bool { return len(l.rbuf) > 0 }, l.readDL); err != nil {
		return 0, err
	}
	n := copy(p, l.rbuf)
	wasFull := l.o.RecvWindow-len(l.rbuf) < l.io.MTU()
	l.rbuf = l.rbuf[n:]
	if len(l.rbuf) == 0 {
		l.rbuf = nil
	}
	if wasFull && !l.closed {
		l.send(tAck, l.sndNxt, nil) // the window opened again
	}
	return n, nil
}

// Write buffers p for transmission; it blocks while the send buffer is full.
func (l *Link) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	written := 0
	if l.closed {
		return 0, l.err
	}
	for written < len(p) {
		if err := l.wait(func() bool { return len(l.sbuf) < l.o.SendBuffer }, l.writeDL); err != nil {
			return written, err
		}
		n := min(len(p)-written, l.o.SendBuffer-len(l.sbuf))
		l.sbuf = append(l.sbuf, p[written:written+n]...)
		written += n
		l.transmit(time.Now())
	}
	return written, nil
}

// Close ends the link and tells the peer.
func (l *Link) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	if l.up {
		l.send(tReset, l.sndNxt, nil)
	}
	l.failLocked(net.ErrClosed)
	return nil
}

type addr string

func (a addr) Network() string { return "stack" }
func (a addr) String() string  { return string(a) }

func (l *Link) LocalAddr() net.Addr  { return addr(l.o.Name) }
func (l *Link) RemoteAddr() net.Addr { return addr(l.o.Name + " peer") }

func (l *Link) SetDeadline(t time.Time) error {
	l.mu.Lock()
	l.readDL, l.writeDL = t, t
	l.cond.Broadcast()
	l.mu.Unlock()
	return nil
}

func (l *Link) SetReadDeadline(t time.Time) error {
	l.mu.Lock()
	l.readDL = t
	l.cond.Broadcast()
	l.mu.Unlock()
	return nil
}

func (l *Link) SetWriteDeadline(t time.Time) error {
	l.mu.Lock()
	l.writeDL = t
	l.cond.Broadcast()
	l.mu.Unlock()
	return nil
}
