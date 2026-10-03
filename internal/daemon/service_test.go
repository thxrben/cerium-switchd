package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

// A daemon built with the kit gets its configuration and role from
// switchd's service, its notices reach the CLI sessions, and switchd can
// call it.
func TestServiceWithDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("", "svc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	notes := make(chan string, 1)
	s := newService(1, nil, func(text string) { notes <- text }, func() svc.Role { return svc.Role{Member: 1, Master: true, HostName: "sw1"} }, log)
	if err := s.start(ctx, dir); err != nil {
		t.Fatal(err)
	}
	s.setConfig("cer-test", map[string]string{"mode": "on"})

	k := daemonkit.New(ctx, daemonkit.Options{Name: "cer-test", SocketDir: dir, Log: log})
	configs := make(chan string, 1)
	k.OnConfig(func(raw json.RawMessage) { configs <- string(raw) })
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) { return "fine", nil })
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-configs:
		if c != `{"mode":"on"}` {
			t.Fatalf("config %s", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no configuration")
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if r, ok := k.Role(); ok && r.HostName == "sw1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no role")
		}
		time.Sleep(10 * time.Millisecond)
	}
	k.Notify("cer-test: hello")
	if n := <-notes; n != "cer-test: hello" {
		t.Fatalf("notice %q", n)
	}
	cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
	defer ccancel()
	var st string
	if err := s.call(cctx, "cer-test", svc.MethodStatus, nil, &st); err != nil || st != "fine" {
		t.Fatalf("status: %q %v", st, err)
	}
	if err := s.call(cctx, "cer-none", svc.MethodStatus, nil, nil); err == nil {
		t.Fatal("call to a daemon that is not running")
	}
	// Standalone: no stack to relay to.
	if err := k.StackCall(cctx, 2, "x", nil, nil); err == nil {
		t.Fatal("stack call without a stack")
	}
}

// switchd's copy of a daemon's topic follows the daemon over a restart:
// keys gone meanwhile disappear, nothing stale stays.
func TestMirrorOverDaemonRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "svc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := newService(1, nil, func(string) {}, func() svc.Role { return svc.Role{Member: 1, Master: true} }, log)
	changes := make(chan map[string]json.RawMessage, 10)
	m := s.follow("cer-dhcpcd", svc.TopicLeases, func(st map[string]json.RawMessage) { changes <- st })
	if err := s.start(ctx, dir); err != nil {
		t.Fatal(err)
	}
	run := func(leases map[string]any) context.CancelFunc {
		kctx, kcancel := context.WithCancel(ctx)
		k := daemonkit.New(kctx, daemonkit.Options{Name: "cer-dhcpcd", SocketDir: dir, Log: log})
		k.Endpoint.Replace(svc.TopicLeases, leases)
		if err := k.Start(); err != nil {
			t.Fatal(err)
		}
		return kcancel
	}
	wait := func(want int) map[string]json.RawMessage {
		for {
			select {
			case st := <-changes:
				if len(st) == want {
					return st
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no state with %d leases", want)
			}
		}
	}
	stop := run(map[string]any{"irb.10": svc.Lease{Addr: "10.1.2.50/24", Router: "10.1.2.1"}, "sw-0-6": svc.Lease{Addr: "10.9.0.7/30"}})
	<-m.Ready()
	st := wait(2)
	got := dhcpLeasesOf(st)
	if got["irb.10"].Addr.String() != "10.1.2.50/24" || got["irb.10"].Router.String() != "10.1.2.1" || got["sw-0-6"].Router.IsValid() {
		t.Fatalf("leases %+v", got)
	}
	stop()
	// The daemon comes back holding one lease only.
	stop = run(map[string]any{"irb.10": svc.Lease{Addr: "10.1.2.50/24", Router: "10.1.2.1"}})
	defer stop()
	if st := wait(1); st["irb.10"] == nil {
		t.Fatalf("after the restart %v", st)
	}
}
