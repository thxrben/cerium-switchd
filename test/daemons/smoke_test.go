// Package daemons runs every cer- program against a fake switchd (a
// service socket in a temporary directory): it must connect, answer its
// status call and stop on SIGTERM. Nothing on the machine changes: the
// daemons get an empty configuration.
package daemons

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/lib/platform/ipc"
	"github.com/thxrben/cerium-switchd/lib/platform/svc"
)

func programs(t *testing.T) []string {
	entries, err := os.ReadDir("../../apps")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "cer-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestDaemonsStartAndStop(t *testing.T) {
	progs := programs(t)
	if len(progs) == 0 {
		t.Skip("no cer- programs yet")
	}
	bin := t.TempDir()
	for _, p := range progs {
		out, err := exec.Command("go", "build", "-o", filepath.Join(bin, p), "../../apps/"+p).CombinedOutput()
		if err != nil {
			t.Fatalf("build %s: %v\n%s", p, err, out)
		}
	}
	for _, p := range progs {
		t.Run(p, func(t *testing.T) { smoke(t, filepath.Join(bin, p), p) })
	}
}

func smoke(t *testing.T, path, name string) {
	dir, err := os.MkdirTemp("", "smoke") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := ipc.NewEndpoint(svc.Switchd, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	sw.Publish(svc.TopicRole, "", svc.Role{Member: 1, Master: true, HostName: "sw1"})
	sw.Publish(svc.TopicConfig, name, map[string]any{})
	connected := make(chan *ipc.Conn, 1)
	sw.OnConnect = func(c *ipc.Conn) { connected <- c }
	l, err := ipc.Listen(svc.Socket(dir, svc.Switchd))
	if err != nil {
		t.Fatal(err)
	}
	go sw.Serve(ctx, l)

	cmd := exec.Command(path, "-socket-dir", dir, "-state-dir", t.TempDir(), "-member", "1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "NOTIFY_SOCKET=") // not under systemd
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
		}
	}()
	var c *ipc.Conn
	select {
	case c = <-connected:
	case err := <-exited:
		t.Fatalf("%s ended before connecting: %v\n%s", name, err, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not connect\n%s", name, stderr.String())
	}
	if c.Peer().Name != name {
		t.Fatalf("hello from %q", c.Peer().Name)
	}
	cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
	defer ccancel()
	var st json.RawMessage
	if err := c.Call(cctx, svc.MethodStatus, nil, &st); err != nil {
		t.Fatalf("status: %v\n%s", err, stderr.String())
	}
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v\n%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not stop on SIGTERM", name)
	}
}
