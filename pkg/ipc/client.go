package ipc

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Client keeps a connection to another program's socket: it connects,
// reconnects after the peer went away (e.g. it restarted) and subscribes
// again. Calls wait for a connection (bounded by their context).
type Client struct {
	e    *Endpoint
	path string

	mu    sync.Mutex
	conn  *Conn
	ready chan struct{} // closed while connected
	subs  []*subscription
	// OnConnect runs after each (re)connect, after the subscriptions are
	// sent.
	onConnect []func(*Conn)
}

// subIDs numbers subscriptions (unique in the process).
var subIDs atomic.Uint64

type subscription struct {
	id         uint64
	topic, key string
	f          func(Event)
	// known: keys delivered since the last Sync; seen: keys delivered
	// since the current connect (before its Sync).
	known, seen map[string]bool
	syncing     bool
}

// DefaultRetry is the delay between connection attempts.
const DefaultRetry = 200 * time.Millisecond

// Dial returns a client of the socket at path; it connects in the
// background until ctx ends.
func (e *Endpoint) Dial(ctx context.Context, path string) *Client {
	c := &Client{e: e, path: path, ready: make(chan struct{})}
	go c.run(ctx)
	return c
}

// OnConnect registers a function run after each (re)connect.
func (c *Client) OnConnect(f func(*Conn)) {
	c.mu.Lock()
	c.onConnect = append(c.onConnect, f)
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		f(conn)
	}
}

// Subscribe follows a topic of the peer (key "": every key). f is called
// in order, never concurrently; it may call the peer.
func (c *Client) Subscribe(topic, key string, f func(Event)) {
	s := &subscription{id: subIDs.Add(1), topic: topic, key: key, f: f, known: map[string]bool{}}
	c.mu.Lock()
	c.subs = append(c.subs, s)
	conn := c.conn
	if conn != nil {
		s.start()
	}
	c.mu.Unlock()
	if conn != nil {
		conn.queue(frame{K: "sub", M: topic, Key: key, S: s.id})
	}
}

func (s *subscription) start() {
	s.seen, s.syncing = map[string]bool{}, true
}

// deliver hands an event of the subscribed topic to the subscriber.
func (s *subscription) deliver(f frame) {
	if f.Sync {
		// Keys known before the reconnect that did not come again are gone.
		for k := range s.known {
			if !s.seen[k] {
				s.f(Event{Topic: s.topic, Key: k, Deleted: true})
			}
		}
		s.known, s.seen, s.syncing = s.seen, nil, false
		s.f(Event{Topic: s.topic, Sync: true})
		return
	}
	if s.syncing {
		if f.Del {
			delete(s.seen, f.Key)
		} else {
			s.seen[f.Key] = true
		}
	} else if f.Del {
		delete(s.known, f.Key)
	} else {
		s.known[f.Key] = true
	}
	s.f(Event{Topic: s.topic, Key: f.Key, Value: f.D, Deleted: f.Del})
}

func (c *Client) run(ctx context.Context) {
	for ctx.Err() == nil {
		var d net.Dialer
		nc, err := d.DialContext(ctx, "unix", c.path)
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(c.e.retry()):
			}
			continue
		}
		conn, err := c.e.start(ctx, nc)
		if err != nil {
			c.e.Log.Debug("ipc: connect", "path", c.path, "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(c.e.retry()):
			}
			continue
		}
		c.mu.Lock()
		subs := c.subs
		conn.mu.Lock()
		conn.events = func(f frame) {
			c.mu.Lock()
			subs := c.subs
			c.mu.Unlock()
			for _, s := range subs {
				if s.id == f.S {
					s.deliver(f)
				}
			}
		}
		conn.mu.Unlock()
		for _, s := range subs {
			s.start()
			conn.queue(frame{K: "sub", M: s.topic, Key: s.key, S: s.id})
		}
		c.conn = conn
		close(c.ready)
		hooks := c.onConnect
		c.mu.Unlock()
		for _, f := range hooks {
			f(conn)
		}
		<-conn.done
		c.mu.Lock()
		c.conn = nil
		c.ready = make(chan struct{})
		c.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(c.e.retry()):
		}
	}
}

// Conn returns the current connection (nil: not connected).
func (c *Client) Conn() *Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

// Wait waits until connected.
func (c *Client) Wait(ctx context.Context) (*Conn, error) {
	for {
		c.mu.Lock()
		conn, ready := c.conn, c.ready
		c.mu.Unlock()
		if conn != nil {
			return conn, nil
		}
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Call calls a method of the peer, waiting for a connection; a call cut
// off by a lost connection is not repeated (it may have run).
func (c *Client) Call(ctx context.Context, method string, req, resp any) error {
	conn, err := c.Wait(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("ipc: " + c.path + " not reachable")
		}
		return err
	}
	return conn.Call(ctx, method, req, resp)
}

// Subscribe follows a topic of the peer on this connection only (an
// accepted connection: the peer reconnects with a new one, which
// Endpoint.OnConnect can subscribe again).
func (c *Conn) Subscribe(topic, key string, f func(Event)) {
	s := &subscription{id: subIDs.Add(1), topic: topic, key: key, f: f, known: map[string]bool{}}
	s.start()
	c.mu.Lock()
	c.subs = append(c.subs, s)
	if c.events == nil {
		c.events = func(fr frame) {
			c.mu.Lock()
			subs := c.subs
			c.mu.Unlock()
			for _, s := range subs {
				if s.id == fr.S {
					s.deliver(fr)
				}
			}
		}
	}
	c.mu.Unlock()
	c.queue(frame{K: "sub", M: topic, Key: key, S: s.id})
}
