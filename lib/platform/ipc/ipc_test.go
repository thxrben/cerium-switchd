package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func sockPath(t *testing.T) string {
	dir, err := os.MkdirTemp("", "ipc") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func serve(t *testing.T, ctx context.Context, e *Endpoint, path string) (stop func()) {
	t.Helper()
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { e.Serve(sctx, l); close(done) }()
	return func() {
		cancel()
		<-done
		for _, c := range e.Conns() {
			c.Close()
			<-c.Done()
		}
	}
}

// recorder collects events.
type recorder struct {
	mu  sync.Mutex
	evs []Event
	ch  chan Event
}

func newRecorder() *recorder { return &recorder{ch: make(chan Event, 100)} }

func (r *recorder) add(e Event) {
	r.mu.Lock()
	r.evs = append(r.evs, e)
	r.mu.Unlock()
	r.ch <- e
}

func (r *recorder) next(t *testing.T) Event {
	t.Helper()
	select {
	case e := <-r.ch:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

func (r *recorder) none(t *testing.T) {
	t.Helper()
	select {
	case e := <-r.ch:
		t.Fatalf("unexpected event %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCallsBothWays(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := sockPath(t)
	srv := NewEndpoint("switchd", "1", quiet)
	srv.Handle("add", func(_ context.Context, _ *Conn, req json.RawMessage) (any, error) {
		var v [2]int
		json.Unmarshal(req, &v)
		return v[0] + v[1], nil
	})
	srv.Handle("fail", func(context.Context, *Conn, json.RawMessage) (any, error) { return nil, errors.New("no such bundle") })
	srv.Handle("panic", func(context.Context, *Conn, json.RawMessage) (any, error) { panic("boom") })
	// The server calls back the client on the same connection.
	srv.Handle("ask-back", func(ctx context.Context, c *Conn, _ json.RawMessage) (any, error) {
		var s string
		err := c.Call(ctx, "name", nil, &s)
		return "peer says " + s, err
	})
	defer serve(t, ctx, srv, path)()

	cli := NewEndpoint("cer-lldpd", "1", quiet)
	cli.Handle("name", func(context.Context, *Conn, json.RawMessage) (any, error) { return "cer-lldpd", nil })
	c := cli.Dial(ctx, path)
	cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
	defer ccancel()
	var sum int
	if err := c.Call(cctx, "add", [2]int{2, 3}, &sum); err != nil || sum != 5 {
		t.Fatalf("add: %d %v", sum, err)
	}
	var re *RemoteError
	if err := c.Call(cctx, "fail", nil, nil); !errors.As(err, &re) || re.Msg != "no such bundle" {
		t.Fatalf("fail: %v", err)
	}
	if err := c.Call(cctx, "panic", nil, nil); !errors.As(err, &re) {
		t.Fatalf("panic: %v", err)
	}
	if err := c.Call(cctx, "nope", nil, nil); !errors.As(err, &re) {
		t.Fatalf("missing method: %v", err)
	}
	var s string
	if err := c.Call(cctx, "ask-back", nil, &s); err != nil || s != "peer says cer-lldpd" {
		t.Fatalf("ask-back: %q %v", s, err)
	}
	if conn := srv.ConnTo("cer-lldpd"); conn == nil || conn.Peer().Version != "1" {
		t.Fatal("server does not know the peer")
	}
}

func TestTopics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := sockPath(t)
	srv := NewEndpoint("cer-lacpd", "1", quiet)
	srv.Publish("legs", "ae1", true)
	srv.Publish("legs", "ae2", false)
	defer serve(t, ctx, srv, path)()

	all, one := newRecorder(), newRecorder()
	c := NewEndpoint("cer-mclagd", "1", quiet).Dial(ctx, path)
	c.Subscribe("legs", "", all.add)
	c.Subscribe("legs", "ae2", one.add)
	for _, want := range []Event{{Key: "ae1", Value: json.RawMessage("true")}, {Key: "ae2", Value: json.RawMessage("false")}, {Sync: true}} {
		got := all.next(t)
		if got.Key != want.Key || string(got.Value) != string(want.Value) || got.Sync != want.Sync {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
	if e := one.next(t); e.Key != "ae2" {
		t.Fatalf("filtered: %+v", e)
	}
	if e := one.next(t); !e.Sync {
		t.Fatalf("filtered sync: %+v", e)
	}
	srv.Publish("legs", "ae1", true) // unchanged: no event
	all.none(t)
	srv.Publish("legs", "ae1", false)
	if e := all.next(t); e.Key != "ae1" || string(e.Value) != "false" {
		t.Fatalf("change: %+v", e)
	}
	one.none(t)
	srv.Replace("legs", map[string]any{"ae2": true, "ae3": true})
	got := map[string]string{}
	for range 3 {
		e := all.next(t)
		if e.Deleted {
			got[e.Key] = "deleted"
		} else {
			got[e.Key] = string(e.Value)
		}
	}
	if got["ae1"] != "deleted" || got["ae2"] != "true" || got["ae3"] != "true" {
		t.Fatalf("replace: %v", got)
	}
}

// After the peer restarts with another state, the subscriber sees exactly
// the differences and calls work again.
func TestReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := sockPath(t)
	srv := NewEndpoint("cer-lacpd", "1", quiet)
	srv.Publish("legs", "ae1", true)
	srv.Publish("legs", "ae2", true)
	stop := serve(t, ctx, srv, path)

	rec := newRecorder()
	cli := NewEndpoint("cer-mclagd", "1", quiet)
	cli.Retry = 20 * time.Millisecond
	connects := make(chan struct{}, 10)
	c := cli.Dial(ctx, path)
	c.OnConnect(func(*Conn) { connects <- struct{}{} })
	c.Subscribe("legs", "", rec.add)
	rec.next(t)
	rec.next(t)
	if !rec.next(t).Sync {
		t.Fatal("no sync")
	}
	<-connects
	stop()

	srv2 := NewEndpoint("cer-lacpd", "2", quiet)
	srv2.Publish("legs", "ae2", false)
	srv2.Publish("legs", "ae3", true)
	srv2.Handle("ping", func(context.Context, *Conn, json.RawMessage) (any, error) { return "pong", nil })
	defer serve(t, ctx, srv2, path)()
	<-connects
	got := map[string]string{}
	for {
		e := rec.next(t)
		if e.Sync {
			break
		}
		if e.Deleted {
			got[e.Key] = "deleted"
		} else {
			got[e.Key] = string(e.Value)
		}
	}
	if len(got) != 3 || got["ae1"] != "deleted" || got["ae2"] != "false" || got["ae3"] != "true" {
		t.Fatalf("after reconnect: %v", got)
	}
	cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
	defer ccancel()
	var s string
	if err := c.Call(cctx, "ping", nil, &s); err != nil || s != "pong" {
		t.Fatalf("call after reconnect: %q %v", s, err)
	}
}

// The accepting side subscribes to topics of the program that connected.
func TestSubscribeOnAcceptedConn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := sockPath(t)
	rec := newRecorder()
	srv := NewEndpoint("switchd", "1", quiet)
	srv.OnConnect = func(c *Conn) {
		if c.Peer().Name == "cer-lacpd" {
			c.Subscribe("legs", "", rec.add)
		}
	}
	defer serve(t, ctx, srv, path)()
	d := NewEndpoint("cer-lacpd", "1", quiet)
	d.Publish("legs", "ae1", true)
	d.Dial(ctx, path)
	if e := rec.next(t); e.Key != "ae1" {
		t.Fatalf("%+v", e)
	}
	if !rec.next(t).Sync {
		t.Fatal("no sync")
	}
	d.Publish("legs", "ae1", false)
	if e := rec.next(t); string(e.Value) != "false" {
		t.Fatalf("%+v", e)
	}
}

func TestCallWithoutPeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewEndpoint("x", "1", quiet).Dial(ctx, sockPath(t))
	cctx, ccancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer ccancel()
	if err := c.Call(cctx, "m", nil, nil); err == nil {
		t.Fatal("call without a peer succeeded")
	}
}

func TestProtocolMismatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := sockPath(t)
	srv := NewEndpoint("switchd", "1", quiet)
	defer serve(t, ctx, srv, path)()
	nc, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	hello, _ := json.Marshal(Hello{Proto: Version + 1, Name: "future"})
	if err := writeFrame(nc, frame{K: "hello", D: hello}); err != nil {
		t.Fatal(err)
	}
	if f, err := readFrame(nc); err != nil || f.K != "hello" {
		t.Fatalf("first frame: %+v %v", f, err)
	}
	if f, err := readFrame(nc); err != nil || f.K != "bye" || f.E == "" {
		t.Fatalf("refusal: %+v %v", f, err)
	}
	if len(srv.Conns()) != 0 {
		t.Fatal("refused connection kept")
	}
}
