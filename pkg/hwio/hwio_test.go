package hwio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitStuck(t *testing.T, n int) []Call {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s := Stuck(); len(s) == n {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("stuck calls %v, want %d", Stuck(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A call that hangs returns an error at its deadline; the resource then
// refuses calls at once until the hanging call returns.
func TestHangingCall(t *testing.T) {
	release := make(chan struct{})
	changes := make(chan int, 10)
	Watch(func(c []Call) {
		select { // a watcher never blocks (it stays for the later tests)
		case changes <- len(c):
		default:
		}
	})
	start := time.Now()
	_, err := Do("/dev/sdx", "read", 50*time.Millisecond, func() (int, error) { <-release; return 1, nil })
	var e *Error
	if !errors.Is(err, ErrTimeout) || !errors.As(err, &e) || e.After != 50*time.Millisecond {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	if s := Stuck(); len(s) != 1 || s[0].Resource != "/dev/sdx" {
		t.Fatalf("stuck %v", s)
	}
	// Refused at once, without running.
	ran := false
	err = DoErr("/dev/sdx", "write", time.Second, func() error { ran = true; return nil })
	if !errors.Is(err, ErrTimeout) || ran {
		t.Fatalf("second call: %v ran=%v", err, ran)
	}
	// Other resources work.
	if v, err := Do("/dev/sdy", "read", time.Second, func() (int, error) { return 7, nil }); v != 7 || err != nil {
		t.Fatalf("other resource: %v %v", v, err)
	}
	close(release)
	waitStuck(t, 0)
	if err := DoErr("/dev/sdx", "read", time.Second, func() error { return nil }); err != nil {
		t.Fatalf("after the hang ended: %v", err)
	}
	if n := <-changes; n != 1 {
		t.Errorf("first change %d", n)
	}
}

// A cancelled caller is not a hang: the resource stays usable unless the
// call outlives its own deadline.
func TestCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := DoCtx(ctx, "netlink", "link add", time.Second, func() (int, error) { <-release; return 0, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if s := Stuck(); len(s) != 0 {
		t.Fatalf("stuck after a cancel: %v", s)
	}
	close(release)
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a")
	if err := WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFile(p); err != nil || string(b) != "x" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := ReadFile(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	for in, want := range map[string]string{"/var/lib/x": "/var", "/dev/sda2": "/dev/sda2", "/": "/", "/config": "/config"} {
		if got := Resource(in); got != want {
			t.Errorf("Resource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWatchResources(t *testing.T) {
	type ev struct {
		res    string
		raised bool
	}
	evs := make(chan ev, 10)
	WatchResources(func(c Call, raised bool) {
		if c.Resource == "/dev/sdz" {
			select {
			case evs <- ev{c.Resource, raised}:
			default:
			}
		}
	})
	release := make(chan struct{})
	Do("/dev/sdz", "read", 20*time.Millisecond, func() (int, error) { <-release; return 0, nil })
	if e := <-evs; e != (ev{"/dev/sdz", true}) {
		t.Fatalf("%+v", e)
	}
	close(release)
	if e := <-evs; e != (ev{"/dev/sdz", false}) {
		t.Fatalf("%+v", e)
	}
}
