package sdnotify

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestNotify(t *testing.T) {
	dir, err := os.MkdirTemp("", "sdnotify")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "notify")
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	t.Setenv("NOTIFY_SOCKET", path)
	if err := Ready(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 100)
	l.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := l.Read(buf)
	if err != nil || string(buf[:n]) != "READY=1" {
		t.Fatalf("%q %v", buf[:n], err)
	}
}

func TestWithoutSystemd(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	t.Setenv("WATCHDOG_USEC", "")
	if err := Alive(); err != nil || WatchdogInterval() != 0 {
		t.Fatal("not started by systemd: no-ops expected")
	}
}

func TestWatchdogInterval(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "10000000")
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))
	if got := WatchdogInterval(); got != 5*time.Second {
		t.Fatalf("%v", got)
	}
	t.Setenv("WATCHDOG_PID", "1")
	if WatchdogInterval() != 0 {
		t.Fatal("watchdog for another process")
	}
}

func TestLiveness(t *testing.T) {
	now := time.Unix(100, 0)
	l := &Liveness{Max: 10 * time.Second, now: func() time.Time { return now }}
	l.Beat("applier")
	l.Beat("stack")
	now = now.Add(5 * time.Second)
	l.Beat("stack")
	if late := l.Late(); len(late) != 0 {
		t.Fatalf("late %v", late)
	}
	now = now.Add(6 * time.Second)
	if late := l.Late(); len(late) != 1 || late[0] != "applier" {
		t.Fatalf("late %v", late)
	}
	l.Forget("applier")
	if late := l.Late(); len(late) != 0 {
		t.Fatalf("late after forget %v", late)
	}
}
