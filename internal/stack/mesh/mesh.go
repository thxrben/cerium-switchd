// Package mesh connects all members of a stack over the authenticated
// sessions of the stacking links (docs/stack-protocol.md, "Mesh"): it
// learns the topology from link-state announcements, relays messages hop
// by hop along shortest paths, and offers streams between any two members
// as net.Conn (used by Raft and stack RPC).
package mesh

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"sort"
	"sync"
	"time"
)

// Message types.
const (
	tLSA    = 1
	tOpen   = 2
	tData   = 3
	tCredit = 4
	tClose  = 5
	tReset  = 6
)

const (
	headerLen   = 12
	maxHops     = 16
	maxPayload  = 32 << 10
	lsaInterval = 5 * time.Second
	lsaMaxAge   = 20 * time.Second
	queueLen    = 4096
	// Window is the per-stream receive window.
	Window = 256 << 10
)

type msg struct {
	typ      byte
	hops     byte
	src, dst byte
	stream   uint32
	seq      uint32
	payload  []byte
}

func (x *msg) encode() []byte {
	b := make([]byte, 4+headerLen+len(x.payload))
	binary.BigEndian.PutUint32(b, uint32(headerLen+len(x.payload)))
	h := b[4:]
	h[0], h[1], h[2], h[3] = x.typ, x.hops, x.src, x.dst
	binary.BigEndian.PutUint32(h[4:], x.stream)
	binary.BigEndian.PutUint32(h[8:], x.seq)
	copy(h[headerLen:], x.payload)
	return b
}

var errBadMessage = errors.New("malformed mesh message")

func readMsg(r io.Reader) (*msg, error) {
	var lb [4]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lb[:])
	if n < headerLen || n > headerLen+maxPayload+64 {
		return nil, errBadMessage
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return &msg{typ: b[0], hops: b[1], src: b[2], dst: b[3], stream: binary.BigEndian.Uint32(b[4:]),
		seq: binary.BigEndian.Uint32(b[8:]), payload: b[headerLen:]}, nil
}

// peer is one authenticated session to a direct neighbour.
type peer struct {
	member int
	conn   io.ReadWriteCloser
	queue  chan []byte
	done   chan struct{}
	once   sync.Once
}

func (p *peer) close() {
	p.once.Do(func() {
		close(p.done)
		p.conn.Close()
	})
}

type lsaEntry struct {
	seq       uint64
	neighbors []int
	at        time.Time
}

type streamKey struct {
	member int
	id     uint32
}

// Mesh is this member's view of the stack.
type Mesh struct {
	Self int
	Log  *slog.Logger

	mu       sync.Mutex
	peers    map[int][]*peer // direct neighbours (parallel cables possible)
	lsas     map[int]lsaEntry
	ownSeq   uint64
	routes   map[int]int // destination -> next hop
	services map[string]*listener
	streams  map[streamKey]*Stream
	nextID   uint32
	changed  chan struct{} // closed and replaced on topology changes
}

// New creates the mesh of member self.
func New(self int, log *slog.Logger) *Mesh {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Mesh{Self: self, Log: log, peers: map[int][]*peer{}, lsas: map[int]lsaEntry{}, routes: map[int]int{},
		services: map[string]*listener{}, streams: map[streamKey]*Stream{}, changed: make(chan struct{})}
	m.nextID = 1
	// A restarted member must not start below the sequence numbers the
	// others still hold for it.
	m.ownSeq = uint64(time.Now().UnixNano())
	return m
}

// Run sends periodic announcements and ages the topology until done.
func (m *Mesh) Run(done <-chan struct{}) {
	t := time.NewTicker(lsaInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			m.mu.Lock()
			m.announceLocked()
			m.ageLocked()
			m.mu.Unlock()
		}
	}
}

// AddPeer takes over an authenticated session to direct neighbour member.
// It returns when the session ends.
func (m *Mesh) AddPeer(member int, conn io.ReadWriteCloser) {
	p := &peer{member: member, conn: conn, queue: make(chan []byte, queueLen), done: make(chan struct{})}
	go func() {
		for {
			select {
			case <-p.done:
				return
			case b := <-p.queue:
				if _, err := conn.Write(b); err != nil {
					p.close()
					return
				}
			}
		}
	}()
	m.mu.Lock()
	m.peers[member] = append(m.peers[member], p)
	m.announceLocked()
	m.recomputeLocked()
	// Tell the new neighbour everything we know.
	for origin, e := range m.lsas {
		p.send(lsaMsg(origin, e))
	}
	m.mu.Unlock()
	for {
		x, err := readMsg(conn)
		if err != nil {
			break
		}
		m.receive(p, x)
	}
	p.close()
	m.mu.Lock()
	m.peers[member] = slices.DeleteFunc(m.peers[member], func(q *peer) bool { return q == p })
	if len(m.peers[member]) == 0 {
		delete(m.peers, member)
	}
	m.announceLocked()
	m.recomputeLocked()
	m.mu.Unlock()
}

func (p *peer) send(x *msg) {
	select {
	case p.queue <- x.encode():
	case <-p.done:
	default:
		// The neighbour is too slow: dropped. A stream notices the gap and
		// resets; LSAs are repeated.
	}
}

func lsaMsg(origin int, e lsaEntry) *msg {
	pl := make([]byte, 8+len(e.neighbors))
	binary.BigEndian.PutUint64(pl, e.seq)
	for i, n := range e.neighbors {
		pl[8+i] = byte(n)
	}
	return &msg{typ: tLSA, hops: maxHops, src: byte(origin), payload: pl}
}

// announceLocked floods this member's current neighbours.
func (m *Mesh) announceLocked() {
	m.ownSeq++
	var ns []int
	for id := range m.peers {
		ns = append(ns, id)
	}
	sort.Ints(ns)
	e := lsaEntry{seq: m.ownSeq, neighbors: ns, at: time.Now()}
	m.lsas[m.Self] = e
	x := lsaMsg(m.Self, e)
	for _, ps := range m.peers {
		for _, p := range ps {
			p.send(x)
		}
	}
}

func (m *Mesh) ageLocked() {
	changed := false
	for origin, e := range m.lsas {
		if origin != m.Self && time.Since(e.at) > lsaMaxAge {
			delete(m.lsas, origin)
			changed = true
		}
	}
	if changed {
		m.recomputeLocked()
	}
}

// recomputeLocked computes shortest paths (BFS; an edge counts only if both
// ends announce it) and resets streams to members that became unreachable.
func (m *Mesh) recomputeLocked() {
	adj := func(a int) []int {
		var out []int
		for _, b := range m.lsas[a].neighbors {
			if slices.Contains(m.lsas[b].neighbors, a) || (a == m.Self && len(m.peers[b]) > 0) {
				out = append(out, b)
			}
		}
		sort.Ints(out)
		return out
	}
	routes := map[int]int{}
	type item struct{ node, first int }
	seen := map[int]bool{m.Self: true}
	var q []item
	for _, n := range adj(m.Self) {
		if len(m.peers[n]) > 0 {
			seen[n] = true
			routes[n] = n
			q = append(q, item{n, n})
		}
	}
	for len(q) > 0 {
		it := q[0]
		q = q[1:]
		for _, n := range adj(it.node) {
			if !seen[n] {
				seen[n] = true
				routes[n] = it.first
				q = append(q, item{n, it.first})
			}
		}
	}
	if !mapsEqual(routes, m.routes) {
		old := m.routes
		m.routes = routes
		close(m.changed)
		m.changed = make(chan struct{})
		for k, s := range m.streams {
			next, ok := routes[k.member]
			switch {
			case !ok:
				s.fail(fmt.Errorf("member %d is no longer reachable", k.member))
				delete(m.streams, k)
			case old[k.member] != next && old[k.member] != 0:
				// Messages may have been lost on the old path, and a stream
				// waiting for an answer would never notice: reset it, its
				// user reconnects over the new path.
				m.sendLocked(&msg{typ: tReset, hops: maxHops, src: byte(m.Self), dst: byte(k.member), stream: k.id})
				s.fail(fmt.Errorf("path to member %d changed", k.member))
				delete(m.streams, k)
			}
		}
	}
}

func mapsEqual(a, b map[int]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Reachable lists the members this member can reach (itself excluded).
func (m *Mesh) Reachable() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []int
	for id := range m.routes {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// Topology returns every known member's announced neighbours.
func (m *Mesh) Topology() map[int][]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int][]int{}
	for id, e := range m.lsas {
		out[id] = slices.Clone(e.neighbors)
	}
	return out
}

// NextHop returns the neighbour on the path to member (0: unreachable).
func (m *Mesh) NextHop(member int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routes[member]
}

// Changed is closed on the next topology change.
func (m *Mesh) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

// sendLocked routes a message to its destination.
func (m *Mesh) sendLocked(x *msg) bool {
	next, ok := m.routes[int(x.dst)]
	if !ok || len(m.peers[next]) == 0 {
		return false
	}
	m.peers[next][0].send(x)
	return true
}

func (m *Mesh) receive(from *peer, x *msg) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if x.typ == tLSA {
		if len(x.payload) < 8 {
			return
		}
		origin := int(x.src)
		seq := binary.BigEndian.Uint64(x.payload)
		if origin == m.Self {
			if seq >= m.ownSeq {
				// Our own announcement from before a restart: go past it.
				m.ownSeq = seq
				m.announceLocked()
			}
			return
		}
		if seq <= m.lsas[origin].seq {
			return
		}
		var ns []int
		for _, b := range x.payload[8:] {
			ns = append(ns, int(b))
		}
		m.lsas[origin] = lsaEntry{seq: seq, neighbors: ns, at: time.Now()}
		for _, ps := range m.peers {
			for _, p := range ps {
				if p != from {
					p.send(x)
				}
			}
		}
		m.recomputeLocked()
		return
	}
	if int(x.dst) != m.Self {
		if x.hops <= 1 {
			return
		}
		x.hops--
		m.sendLocked(x)
		return
	}
	m.deliverLocked(x)
}

// ---- streams ----

// Stream is a connection between two members (net.Conn).
type Stream struct {
	m      *Mesh
	key    streamKey
	opened chan error // Dial waits for CREDIT or RESET

	mu        sync.Mutex
	cond      *sync.Cond
	rbuf      []byte
	recvSeq   uint32
	sendSeq   uint32
	credit    int
	unacked   int // bytes read but not yet granted back
	eof       bool
	closed    bool
	err       error
	rdl, wdl  time.Time
	service   string
	remoteEnd bool // the other side opened it
}

func newStream(m *Mesh, key streamKey) *Stream {
	s := &Stream{m: m, key: key, opened: make(chan error, 1)}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *Stream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.cond.Broadcast()
	s.mu.Unlock()
	select {
	case s.opened <- err:
	default:
	}
}

func (m *Mesh) deliverLocked(x *msg) {
	key := streamKey{int(x.src), x.stream}
	s := m.streams[key]
	switch x.typ {
	case tOpen:
		l := m.services[string(x.payload)]
		if l == nil || s != nil {
			m.sendLocked(&msg{typ: tReset, hops: maxHops, src: byte(m.Self), dst: x.src, stream: x.stream})
			return
		}
		s = newStream(m, key)
		s.service, s.remoteEnd, s.credit = string(x.payload), true, Window
		m.streams[key] = s
		m.sendLocked(&msg{typ: tCredit, hops: maxHops, src: byte(m.Self), dst: x.src, stream: x.stream, seq: Window})
		select {
		case l.accept <- s:
		default:
			delete(m.streams, key)
			m.sendLocked(&msg{typ: tReset, hops: maxHops, src: byte(m.Self), dst: x.src, stream: x.stream})
		}
	case tData:
		if s == nil {
			return
		}
		s.mu.Lock()
		switch {
		case x.seq != s.recvSeq:
			s.mu.Unlock()
			m.resetLocked(s, fmt.Errorf("stream to member %d lost data (path change)", key.member))
			return
		case len(s.rbuf)+len(x.payload) > Window:
			s.mu.Unlock()
			m.resetLocked(s, errors.New("stream window exceeded"))
			return
		}
		s.recvSeq++
		s.rbuf = append(s.rbuf, x.payload...)
		s.cond.Broadcast()
		s.mu.Unlock()
	case tCredit:
		if s == nil {
			return
		}
		s.mu.Lock()
		s.credit += int(x.seq)
		s.cond.Broadcast()
		s.mu.Unlock()
		select {
		case s.opened <- nil:
		default:
		}
	case tClose:
		if s == nil {
			return
		}
		s.mu.Lock()
		s.eof = true
		s.cond.Broadcast()
		done := s.closed
		s.mu.Unlock()
		if done {
			delete(m.streams, key)
		}
	case tReset:
		if s == nil {
			return
		}
		delete(m.streams, key)
		s.fail(fmt.Errorf("stream reset by member %d", key.member))
	}
}

func (m *Mesh) resetLocked(s *Stream, err error) {
	delete(m.streams, s.key)
	m.sendLocked(&msg{typ: tReset, hops: maxHops, src: byte(m.Self), dst: byte(s.key.member), stream: s.key.id})
	s.fail(err)
}

// Dial opens a stream to service on member.
func (m *Mesh) Dial(member int, service string, timeout time.Duration) (*Stream, error) {
	if member == m.Self {
		return nil, errors.New("cannot dial self")
	}
	m.mu.Lock()
	// Stream ids: odd from the member with the lower id, even from the other.
	id := m.nextID * 2
	if m.Self < member {
		id--
	}
	m.nextID++
	key := streamKey{member, id}
	s := newStream(m, key)
	s.service = service
	m.streams[key] = s
	ok := m.sendLocked(&msg{typ: tOpen, hops: maxHops, src: byte(m.Self), dst: byte(member), stream: id, payload: []byte(service)})
	m.mu.Unlock()
	if !ok {
		m.drop(key)
		return nil, fmt.Errorf("member %d is not reachable", member)
	}
	select {
	case err := <-s.opened:
		if err != nil {
			m.drop(key)
			return nil, err
		}
		return s, nil
	case <-time.After(timeout):
		m.drop(key)
		return nil, fmt.Errorf("member %d did not answer", member)
	}
}

func (m *Mesh) drop(key streamKey) {
	m.mu.Lock()
	delete(m.streams, key)
	m.mu.Unlock()
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "mesh stream: i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func (s *Stream) wait(ready func() bool, dl time.Time) error {
	for !ready() {
		if s.err != nil {
			return s.err
		}
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return timeoutError{}
			}
			t := time.AfterFunc(d, func() {
				s.mu.Lock()
				s.cond.Broadcast()
				s.mu.Unlock()
			})
			s.cond.Wait()
			t.Stop()
			continue
		}
		s.cond.Wait()
	}
	return nil
}

func (s *Stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	if err := s.wait(func() bool { return len(s.rbuf) > 0 || s.eof }, s.rdl); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	if len(s.rbuf) == 0 {
		s.mu.Unlock()
		return 0, io.EOF
	}
	n := copy(p, s.rbuf)
	s.rbuf = s.rbuf[n:]
	s.unacked += n
	grant := 0
	if s.unacked >= Window/4 {
		grant, s.unacked = s.unacked, 0
	}
	s.mu.Unlock()
	if grant > 0 {
		s.m.mu.Lock()
		s.m.sendLocked(&msg{typ: tCredit, hops: maxHops, src: byte(s.m.Self), dst: byte(s.key.member), stream: s.key.id, seq: uint32(grant)})
		s.m.mu.Unlock()
	}
	return n, nil
}

func (s *Stream) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return written, net.ErrClosed
		}
		if err := s.wait(func() bool { return s.credit > 0 }, s.wdl); err != nil {
			s.mu.Unlock()
			return written, err
		}
		n := min(len(p)-written, s.credit, maxPayload)
		seq := s.sendSeq
		s.sendSeq++
		s.credit -= n
		s.mu.Unlock()
		s.m.mu.Lock()
		ok := s.m.sendLocked(&msg{typ: tData, hops: maxHops, src: byte(s.m.Self), dst: byte(s.key.member), stream: s.key.id,
			seq: seq, payload: append([]byte(nil), p[written:written+n]...)})
		s.m.mu.Unlock()
		if !ok {
			err := fmt.Errorf("member %d is not reachable", s.key.member)
			s.fail(err)
			return written, err
		}
		written += n
	}
	return written, nil
}

// Close ends the stream (both directions for this side).
func (s *Stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.err == nil {
		s.err = net.ErrClosed
	}
	peerDone := s.eof
	s.cond.Broadcast()
	s.mu.Unlock()
	s.m.mu.Lock()
	s.m.sendLocked(&msg{typ: tClose, hops: maxHops, src: byte(s.m.Self), dst: byte(s.key.member), stream: s.key.id})
	if peerDone {
		delete(s.m.streams, s.key)
	} else {
		// Forget it after a while even if the CLOSE of the other side never
		// comes (it went away).
		key := s.key
		time.AfterFunc(30*time.Second, func() { s.m.drop(key) })
	}
	s.m.mu.Unlock()
	return nil
}

// Addr of a stream endpoint: "member <id>".
type Addr int

func (a Addr) Network() string { return "mesh" }
func (a Addr) String() string  { return fmt.Sprintf("member %d", int(a)) }

func (s *Stream) LocalAddr() net.Addr  { return Addr(s.m.Self) }
func (s *Stream) RemoteAddr() net.Addr { return Addr(s.key.member) }

// Member returns the member at the other end.
func (s *Stream) Member() int { return s.key.member }

func (s *Stream) SetDeadline(t time.Time) error {
	s.mu.Lock()
	s.rdl, s.wdl = t, t
	s.cond.Broadcast()
	s.mu.Unlock()
	return nil
}

func (s *Stream) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.rdl = t
	s.cond.Broadcast()
	s.mu.Unlock()
	return nil
}

func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.wdl = t
	s.cond.Broadcast()
	s.mu.Unlock()
	return nil
}

// ---- listeners ----

type listener struct {
	m       *Mesh
	service string
	accept  chan *Stream
	done    chan struct{}
	once    sync.Once
}

// Listen accepts streams for a service (net.Listener).
func (m *Mesh) Listen(service string) net.Listener {
	l := &listener{m: m, service: service, accept: make(chan *Stream, 64), done: make(chan struct{})}
	m.mu.Lock()
	m.services[service] = l
	m.mu.Unlock()
	return l
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case s := <-l.accept:
		return s, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *listener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.m.mu.Lock()
		if l.m.services[l.service] == l {
			delete(l.m.services, l.service)
		}
		l.m.mu.Unlock()
	})
	return nil
}

func (l *listener) Addr() net.Addr { return Addr(l.m.Self) }
