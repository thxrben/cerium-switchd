package updated

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mclag/internal/software"
)

func setup(t *testing.T, healthy func(v string) bool) (*Daemon, *software.Installer, string) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "switchd")
	os.WriteFile(prog, []byte("v1"), 0o755)
	in := &software.Installer{Program: prog, StateFile: filepath.Join(dir, "software.json")}
	d := &Daemon{Inst: in, Socket: filepath.Join(dir, "s", "sock"), Version: "v1", Timeout: 300 * time.Millisecond}
	d.Restart = func() error {
		// The "new switchd" reports itself healthy (or not).
		raw, _ := os.ReadFile(prog)
		v := string(raw)
		if healthy(v) {
			go func() {
				time.Sleep(50 * time.Millisecond)
				Call(d.Socket, Request{Op: "healthy", Version: v})
			}()
		}
		return nil
	}
	return d, in, dir
}

func waitState(t *testing.T, d *Daemon, want string) string {
	for i := 0; i < 100; i++ {
		rep, err := Call(d.Socket, Request{Op: "status"})
		if err == nil && strings.Contains(rep.State, want) {
			return rep.State
		}
		time.Sleep(30 * time.Millisecond)
	}
	rep, _ := Call(d.Socket, Request{Op: "status"})
	t.Fatalf("state %q, want %q", rep.State, want)
	return ""
}

func TestInstallHealthy(t *testing.T) {
	d, in, dir := setup(t, func(string) bool { return true })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan struct{}, 1)
	d.Exit = func() { exited <- struct{}{} }
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	staged := filepath.Join(dir, "staged")
	os.WriteFile(staged, []byte("v2"), 0o600)
	if _, err := Call(d.Socket, Request{Op: "install", Program: staged, Version: "v2", Running: "v1"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, d, "v2 is healthy")
	if raw, _ := os.ReadFile(in.Program); string(raw) != "v2" || in.Load().Pending != nil || in.Load().Previous != "v1" {
		t.Errorf("installed %q, state %+v", raw, in.Load())
	}
	select {
	case <-exited: // the daemon continues on the new program
	case <-time.After(time.Second):
		t.Error("daemon did not restart onto the new program")
	}
}

func TestInstallUnhealthyRollsBack(t *testing.T) {
	d, in, dir := setup(t, func(v string) bool { return v == "v1" })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	staged := filepath.Join(dir, "staged")
	os.WriteFile(staged, []byte("v2"), 0o600)
	if _, err := Call(d.Socket, Request{Op: "install", Program: staged, Version: "v2", Running: "v1"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, d, "v1 is healthy")
	st := in.Load()
	if raw, _ := os.ReadFile(in.Program); string(raw) != "v1" || !strings.Contains(st.Note, "v2 was not healthy") {
		t.Errorf("program %q, state %+v", raw, st)
	}
}

func TestBothUnhealthyGivesUp(t *testing.T) {
	d, _, dir := setup(t, func(string) bool { return false })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	staged := filepath.Join(dir, "staged")
	os.WriteFile(staged, []byte("v2"), 0o600)
	Call(d.Socket, Request{Op: "install", Program: staged, Version: "v2", Running: "v1"})
	waitState(t, d, "failed: the previous version v1")
}
