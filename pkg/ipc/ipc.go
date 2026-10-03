// Package ipc connects the programs of a switch (switchd and the cer-
// daemons) over unix sockets.
//
// An Endpoint is one program's side: it serves calls (Handle) and state
// topics (Publish), on every connection, accepted (Serve) or dialled
// (Client). Both ends of a connection can call the other.
//
// A state topic is a set of keys with JSON values. A subscriber receives
// every key, then a Sync event, then each change. After a reconnect it
// receives the full state again; keys that are gone meanwhile are reported
// deleted with the Sync event, so subscribers always see a consistent
// state without handling reconnects themselves.
//
// On the wire: frames of a 4-byte big-endian length and a JSON object. Each
// side starts with a hello naming the protocol version, the program and its
// version; a peer with another protocol version is refused.
package ipc

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"
)

// Version is the protocol version.
const Version = 1

// MaxFrame bounds one frame (a full routing table goes in pieces).
const MaxFrame = 64 << 20

// queueLen bounds the frames waiting to be written to one peer; a peer
// that does not read is disconnected (it resynchronises when it is back).
const queueLen = 4096

type frame struct {
	ID   uint64          `json:"id,omitempty"`
	K    string          `json:"k"`           // hello, call, reply, sub, ev, bye
	M    string          `json:"m,omitempty"` // method or topic
	Key  string          `json:"key,omitempty"`
	D    json.RawMessage `json:"d,omitempty"`
	E    string          `json:"e,omitempty"`
	Del  bool            `json:"del,omitempty"`
	Sync bool            `json:"sync,omitempty"`
	// S is the subscription an event belongs to (chosen by the
	// subscriber).
	S uint64 `json:"s,omitempty"`
}

// Hello is what each side announces.
type Hello struct {
	Proto   int    `json:"proto"`
	Name    string `json:"name"`
	Version string `json:"version"`
	// Meta is program-specific (e.g. the stacking-protocol methods a
	// daemon serves).
	Meta json.RawMessage `json:"meta,omitempty"`
}

// HandlerFunc serves a call. The context ends when the connection does.
type HandlerFunc func(ctx context.Context, c *Conn, req json.RawMessage) (any, error)

// Event is a change of a subscribed topic.
type Event struct {
	Topic   string
	Key     string
	Value   json.RawMessage
	Deleted bool
	// Sync: the full state has been delivered (after every connect).
	Sync bool
}

// RemoteError is an error returned by the peer's handler.
type RemoteError struct{ Msg string }

func (e *RemoteError) Error() string { return e.Msg }

// ErrClosed is returned by calls on a closed connection.
var ErrClosed = errors.New("ipc: connection closed")

// Endpoint is one program's side of its connections.
type Endpoint struct {
	Name, Version string
	Log           *slog.Logger
	// Meta is sent in every hello.
	Meta json.RawMessage
	// OnConnect runs after the hellos of a new connection (accepted or
	// dialled), OnDisconnect when it ends.
	OnConnect    func(*Conn)
	OnDisconnect func(*Conn)
	// Retry is the delay between connection attempts of its clients
	// (0: DefaultRetry).
	Retry time.Duration

	mu       sync.Mutex
	handlers map[string]HandlerFunc
	topics   map[string]map[string]json.RawMessage
	subs     map[string]map[*Conn]map[uint64]string // topic -> conn -> subscription id -> key ("" all)
	conns    map[*Conn]bool
}

// NewEndpoint returns an endpoint.
func NewEndpoint(name, version string, log *slog.Logger) *Endpoint {
	if log == nil {
		log = slog.Default()
	}
	return &Endpoint{Name: name, Version: version, Log: log, handlers: map[string]HandlerFunc{},
		topics: map[string]map[string]json.RawMessage{}, subs: map[string]map[*Conn]map[uint64]string{}, conns: map[*Conn]bool{}}
}

func (e *Endpoint) retry() time.Duration {
	if e.Retry > 0 {
		return e.Retry
	}
	return DefaultRetry
}

// Handle registers a call handler.
func (e *Endpoint) Handle(method string, f HandlerFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers[method] = f
}

// Publish sets a key of a topic (v nil: deletes it). Subscribers are told
// only when the value changes.
func (e *Endpoint) Publish(topic, key string, v any) error {
	var raw json.RawMessage
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		raw = b
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setLocked(topic, key, raw)
	return nil
}

// Replace sets a whole topic: keys not in all are deleted.
func (e *Endpoint) Replace(topic string, all map[string]any) error {
	enc := map[string]json.RawMessage{}
	for k, v := range all {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		enc[k] = b
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, k := range slices.Sorted(maps.Keys(e.topics[topic])) {
		if _, ok := enc[k]; !ok {
			e.setLocked(topic, k, nil)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(enc)) {
		e.setLocked(topic, k, enc[k])
	}
	return nil
}

// Get returns a topic's current value of a key (nil: none).
func (e *Endpoint) Get(topic, key string) json.RawMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.topics[topic][key]
}

func (e *Endpoint) setLocked(topic, key string, raw json.RawMessage) {
	t := e.topics[topic]
	old, had := t[key]
	switch {
	case raw == nil && !had:
		return
	case raw != nil && had && string(old) == string(raw):
		return
	}
	if raw == nil {
		delete(t, key)
	} else {
		if t == nil {
			t = map[string]json.RawMessage{}
			e.topics[topic] = t
		}
		t[key] = raw
	}
	for c, subs := range e.subs[topic] {
		for id, k := range subs {
			if k == "" || k == key {
				c.queue(frame{K: "ev", M: topic, Key: key, D: raw, Del: raw == nil, S: id})
			}
		}
	}
}

// Conns returns the current connections.
func (e *Endpoint) Conns() []*Conn {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Collect(maps.Keys(e.conns))
}

// ConnTo returns a current connection to the named program (nil: none).
func (e *Endpoint) ConnTo(name string) *Conn {
	e.mu.Lock()
	defer e.mu.Unlock()
	for c := range e.conns {
		if c.peer.Name == name {
			return c
		}
	}
	return nil
}

// Listen creates the socket at path (replacing a stale one), readable and
// writable by its owner only.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// A listener that closes must not remove the path: a new instance of
	// the program (or a test's next one) may already listen there. A stale
	// socket file is replaced by the next Listen.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Serve accepts connections until ctx ends.
func (e *Endpoint) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		nc, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !trusted(nc) {
			e.Log.Warn("ipc: connection from another user refused")
			nc.Close()
			continue
		}
		go func() {
			c, err := e.start(ctx, nc)
			if err != nil {
				e.Log.Debug("ipc: handshake", "err", err)
				return
			}
			<-c.done
		}()
	}
}

// trusted: the peer runs as root or as this program's user (tests).
func trusted(nc net.Conn) bool {
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		return true
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || cerr != nil {
		return false
	}
	return cred.Uid == 0 || int(cred.Uid) == os.Getuid()
}

// Conn is one connection.
type Conn struct {
	e    *Endpoint
	nc   net.Conn
	peer Hello
	ctx  context.Context
	stop context.CancelFunc
	out  chan frame
	done chan struct{}

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan frame
	// Subscriptions this side made on the peer (client side).
	events func(frame)
	subs   []*subscription // Conn.Subscribe
	closed bool
}

// Peer returns the peer's hello.
func (c *Conn) Peer() Hello { return c.peer }

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Close ends the connection.
func (c *Conn) Close() { c.nc.Close() }

func (e *Endpoint) start(ctx context.Context, nc net.Conn) (*Conn, error) {
	cctx, stop := context.WithCancel(ctx)
	c := &Conn{e: e, nc: nc, ctx: cctx, stop: stop, out: make(chan frame, queueLen), done: make(chan struct{}),
		pending: map[uint64]chan frame{}}
	hello, _ := json.Marshal(Hello{Proto: Version, Name: e.Name, Version: e.Version, Meta: e.Meta})
	nc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writeFrame(nc, frame{K: "hello", D: hello}); err != nil {
		nc.Close()
		stop()
		return nil, err
	}
	r := bufio.NewReaderSize(nc, 64<<10)
	f, err := readFrame(r)
	if err != nil {
		nc.Close()
		stop()
		return nil, err
	}
	if f.K != "hello" || json.Unmarshal(f.D, &c.peer) != nil {
		nc.Close()
		stop()
		return nil, errors.New("ipc: no hello")
	}
	if c.peer.Proto != Version {
		writeFrame(nc, frame{K: "bye", E: fmt.Sprintf("protocol version %d, %s speaks %d", c.peer.Proto, e.Name, Version)})
		nc.Close()
		stop()
		return nil, fmt.Errorf("ipc: %s speaks protocol version %d, this is %d", c.peer.Name, c.peer.Proto, Version)
	}
	nc.SetDeadline(time.Time{})
	e.mu.Lock()
	e.conns[c] = true
	e.mu.Unlock()
	go c.writer()
	go c.reader(r)
	if e.OnConnect != nil {
		e.OnConnect(c)
	}
	return c, nil
}

func (c *Conn) queue(f frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.out <- f:
	default:
		c.e.Log.Warn("ipc: peer does not read; disconnected", "peer", c.peer.Name)
		c.closed = true
		c.nc.Close()
	}
}

func (c *Conn) writer() {
	w := bufio.NewWriterSize(c.nc, 64<<10)
	for {
		select {
		case f := <-c.out:
			if err := writeFrame(w, f); err != nil {
				c.nc.Close()
				return
			}
			// Write out once nothing else is waiting.
			if len(c.out) == 0 {
				if err := w.Flush(); err != nil {
					c.nc.Close()
					return
				}
			}
		case <-c.done:
			return
		}
	}
}

func (c *Conn) reader(r *bufio.Reader) {
	// Events are handed over in order on their own goroutine, so a
	// subscriber may call the peer from its callback.
	evq := make(chan frame, queueLen)
	evDone := make(chan struct{})
	go func() {
		defer close(evDone)
		for f := range evq {
			c.mu.Lock()
			h := c.events
			c.mu.Unlock()
			if h != nil {
				h(f)
			}
		}
	}()
	defer func() {
		close(evq)
		<-evDone
		c.shutdown()
	}()
	for {
		f, err := readFrame(r)
		if err != nil {
			return
		}
		switch f.K {
		case "call":
			go c.serve(f)
		case "reply":
			c.mu.Lock()
			ch := c.pending[f.ID]
			delete(c.pending, f.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		case "sub":
			c.e.subscribe(c, f.S, f.M, f.Key)
		case "ev":
			select {
			case evq <- f:
			default:
				c.e.Log.Warn("ipc: subscriber too slow; reconnecting", "peer", c.peer.Name)
				return
			}
		case "bye":
			c.e.Log.Warn("ipc: refused by peer", "peer", c.peer.Name, "reason", f.E)
			return
		}
	}
}

func (c *Conn) shutdown() {
	c.mu.Lock()
	c.closed = true
	pending := c.pending
	c.pending = map[uint64]chan frame{}
	c.mu.Unlock()
	c.nc.Close()
	c.stop()
	for _, ch := range pending {
		ch <- frame{K: "reply", E: ErrClosed.Error()}
	}
	e := c.e
	e.mu.Lock()
	delete(e.conns, c)
	for _, s := range e.subs {
		delete(s, c)
	}
	e.mu.Unlock()
	close(c.done)
	if e.OnDisconnect != nil {
		e.OnDisconnect(c)
	}
}

func (c *Conn) serve(f frame) {
	c.e.mu.Lock()
	h := c.e.handlers[f.M]
	c.e.mu.Unlock()
	rep := frame{ID: f.ID, K: "reply"}
	var v any
	var err error
	if h == nil {
		err = fmt.Errorf("%s: no method %q", c.e.Name, f.M)
	} else {
		v, err = func() (v any, err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("%s: %s failed: %v", c.e.Name, f.M, p)
					c.e.Log.Error("ipc: handler panicked", "method", f.M, "panic", p)
				}
			}()
			return h(c.ctx, c, f.D)
		}()
	}
	if err != nil {
		rep.E = err.Error()
		if rep.E == "" {
			rep.E = "error"
		}
	} else if v != nil {
		if raw, ok := v.(json.RawMessage); ok {
			rep.D = raw
		} else if rep.D, err = json.Marshal(v); err != nil {
			rep.E = err.Error()
		}
	}
	c.queue(rep)
}

// Call calls a method of the peer; resp (if not nil) receives the result.
func (c *Conn) Call(ctx context.Context, method string, req, resp any) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ch := make(chan frame, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	c.queue(frame{ID: id, K: "call", M: method, D: raw})
	select {
	case f := <-ch:
		if f.E != "" {
			if f.E == ErrClosed.Error() {
				return ErrClosed
			}
			return &RemoteError{f.E}
		}
		if resp != nil && len(f.D) > 0 {
			return json.Unmarshal(f.D, resp)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (e *Endpoint) subscribe(c *Conn, id uint64, topic, key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.subs[topic]
	if s == nil {
		s = map[*Conn]map[uint64]string{}
		e.subs[topic] = s
	}
	if s[c] == nil {
		s[c] = map[uint64]string{}
	}
	s[c][id] = key
	t := e.topics[topic]
	for _, k := range slices.Sorted(maps.Keys(t)) {
		if key == "" || k == key {
			c.queue(frame{K: "ev", M: topic, Key: k, D: t[k], S: id})
		}
	}
	c.queue(frame{K: "ev", M: topic, Key: key, Sync: true, S: id})
}

func writeFrame(w io.Writer, f frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return fmt.Errorf("ipc: frame of %d bytes", len(b))
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func readFrame(r io.Reader) (frame, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return frame{}, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > MaxFrame {
		return frame{}, fmt.Errorf("ipc: frame of %d bytes", size)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return frame{}, err
	}
	var f frame
	if err := json.Unmarshal(b, &f); err != nil {
		return frame{}, err
	}
	return f, nil
}
