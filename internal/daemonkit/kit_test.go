package daemonkit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestKitWithSwitchd(t *testing.T) {
	dir, err := os.MkdirTemp("", "kit")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A fake switchd: config and role topics, the stack relay (to "member
	// 2", which is this same daemon here) and notices.
	sw := ipc.NewEndpoint(svc.Switchd, "test", quiet)
	sw.Publish(svc.TopicConfig, "cer-test", map[string]int{"interval": 30})
	sw.Publish(svc.TopicConfig, "cer-other", map[string]int{"x": 1})
	sw.Publish(svc.TopicRole, "", svc.Role{Member: 1, Master: true})
	notes := make(chan string, 1)
	sw.Handle(svc.MethodNote, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var n svc.Notice
		json.Unmarshal(raw, &n)
		notes <- n.Text
		return nil, nil
	})
	sw.Handle(svc.MethodStack, func(ctx context.Context, c *ipc.Conn, raw json.RawMessage) (any, error) {
		var sc svc.StackCall
		json.Unmarshal(raw, &sc)
		var out json.RawMessage
		err := c.Call(ctx, svc.StackPrefix+sc.Method, svc.StackIn{From: 1, Data: sc.Data}, &out)
		return out, err
	})
	connected := make(chan *ipc.Conn, 1)
	sw.OnConnect = func(c *ipc.Conn) { connected <- c }
	l, err := ipc.Listen(svc.Socket(dir, svc.Switchd))
	if err != nil {
		t.Fatal(err)
	}
	go sw.Serve(ctx, l)

	k := New(ctx, Options{Name: "cer-test", SocketDir: dir, Log: quiet})
	configs := make(chan string, 2)
	k.OnConfig(func(raw json.RawMessage) { configs <- string(raw) })
	k.HandleStack("echo", func(_ context.Context, from int, req json.RawMessage) (any, error) {
		var s string
		json.Unmarshal(req, &s)
		return map[string]any{"from": from, "said": s}, nil
	})
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	conn := <-connected
	var meta svc.Meta
	json.Unmarshal(conn.Peer().Meta, &meta)
	if !slices.Equal(meta.Stack, []string{"echo"}) {
		t.Fatalf("meta %+v", meta)
	}
	select {
	case c := <-configs:
		if c != `{"interval":30}` {
			t.Fatalf("config %s", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no config")
	}
	sw.Publish(svc.TopicConfig, "cer-other", map[string]int{"x": 2}) // not ours
	sw.Publish(svc.TopicConfig, "cer-test", map[string]int{"interval": 10})
	if c := <-configs; c != `{"interval":10}` {
		t.Fatalf("config change %s", c)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r, ok := k.Role(); ok && r.Master {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no role")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
	defer ccancel()
	var got struct {
		From int    `json:"from"`
		Said string `json:"said"`
	}
	if err := k.StackCall(cctx, 2, "echo", "hello", &got); err != nil || got.From != 1 || got.Said != "hello" {
		t.Fatalf("stack call: %+v %v", got, err)
	}
	k.Notify("cer-test is fine")
	if n := <-notes; n != "cer-test is fine" {
		t.Fatalf("notice %q", n)
	}
}

// The shutdown work runs while the daemon still works (Ctx alive), in
// parallel, within the budget; a hook that hangs does not keep it.
func TestShutdownHooks(t *testing.T) {
	k := New(context.Background(), Options{Name: "cer-test", Log: quiet, StopTimeout: 200 * time.Millisecond})
	var mu sync.Mutex
	var done []string
	k.OnShutdown(func(ctx context.Context) {
		if k.Ctx.Err() != nil {
			t.Error("the daemon's context ended before its shutdown work")
		}
		mu.Lock()
		done = append(done, "quick")
		mu.Unlock()
	})
	k.OnShutdown(func(ctx context.Context) {
		<-ctx.Done() // hangs until the budget is spent
		mu.Lock()
		done = append(done, "slow")
		mu.Unlock()
	})
	start := time.Now()
	k.Shutdown()
	if d := time.Since(start); d < 150*time.Millisecond || d > time.Second {
		t.Errorf("shutdown took %v with a 200 ms budget", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(done, "quick") {
		t.Errorf("hooks run: %v", done)
	}
}
