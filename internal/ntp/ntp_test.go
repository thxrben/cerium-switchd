package ntp

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu    sync.Mutex
	steps []time.Duration
	slews []time.Duration
}

func (f *fakeClock) Step(d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, d)
	return nil
}

func (f *fakeClock) Slew(d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slews = append(f.slews, d)
	return nil
}

func (f *fakeClock) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.steps), len(f.slews)
}

// server answers requests on 127.0.0.1 with its clock ahead by ahead;
// mangle may change the reply.
func server(t *testing.T, ahead time.Duration, stratum byte, mangle func([]byte)) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 48 {
				continue
			}
			now := time.Now().Add(ahead)
			r := make([]byte, 48)
			r[0] = 0<<6 | 4<<3 | 4
			r[1] = stratum
			copy(r[24:32], buf[40:48]) // origin = the client's transmit time
			put64(r[32:], toNTP(now))
			put64(r[40:], toNTP(now))
			if mangle != nil {
				mangle(r)
			}
			pc.WriteTo(r, addr)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func waitStatus(t *testing.T, c *Client, what string, cond func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := c.Status()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", what, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTimestampConversion(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 34, 56, 789_000_000, time.UTC)
	if d := fromNTP(toNTP(now)).Sub(now); d > time.Microsecond || d < -time.Microsecond {
		t.Errorf("round trip off by %v", d)
	}
}

func TestStepAndSlew(t *testing.T) {
	clk := &fakeClock{}
	port := server(t, 5*time.Second, 2, nil)
	c := &Client{Clock: clk, Port: port}
	c.Configure([]Server{{Host: "127.0.0.1"}})
	defer c.Stop()
	st := waitStatus(t, c, "synchronised", func(s Status) bool { return s.Synced })
	steps, slews := clk.counts()
	if steps != 1 || slews != 0 {
		t.Fatalf("steps %d slews %d", steps, slews)
	}
	if d := clk.steps[0] - 5*time.Second; d > 100*time.Millisecond || d < -100*time.Millisecond {
		t.Errorf("stepped by %v, want about 5s", clk.steps[0])
	}
	if !st.LastStep || st.Servers[0].Stratum != 2 || !st.Servers[0].Selected || !st.Servers[0].Reach {
		t.Errorf("status: %+v", st)
	}

	// A small offset is slewed.
	clk2 := &fakeClock{}
	port2 := server(t, 40*time.Millisecond, 3, nil)
	c2 := &Client{Clock: clk2, Port: port2}
	c2.Configure([]Server{{Host: "127.0.0.1"}})
	defer c2.Stop()
	waitStatus(t, c2, "synchronised", func(s Status) bool { return s.Synced })
	if steps, slews := clk2.counts(); steps != 0 || slews != 1 {
		t.Errorf("steps %d slews %d", steps, slews)
	}
}

func TestPreferredServer(t *testing.T) {
	clk := &fakeClock{}
	// Two servers on different ports: the client has one port, so they are
	// told apart by address (127.0.0.1 and 127.0.0.2 on the same port).
	pc1, _ := net.ListenPacket("udp", "127.0.0.1:0")
	port := pc1.LocalAddr().(*net.UDPAddr).Port
	pc1.Close()
	answer := func(ip string, ahead time.Duration) {
		pc, err := net.ListenPacket("udp", net.JoinHostPort(ip, itoa(port)))
		if err != nil {
			t.Skipf("cannot listen on %s: %v", ip, err)
		}
		t.Cleanup(func() { pc.Close() })
		go func() {
			buf := make([]byte, 512)
			for {
				n, addr, err := pc.ReadFrom(buf)
				if err != nil || n < 48 {
					return
				}
				r := make([]byte, 48)
				r[0], r[1] = 4<<3|4, 2
				copy(r[24:32], buf[40:48])
				now := time.Now().Add(ahead)
				put64(r[32:], toNTP(now))
				put64(r[40:], toNTP(now))
				pc.WriteTo(r, addr)
			}
		}()
	}
	answer("127.0.0.1", 10*time.Second)
	answer("127.0.0.2", 20*time.Second)
	c := &Client{Clock: clk, Port: port}
	c.Configure([]Server{{Host: "127.0.0.1"}, {Host: "127.0.0.2", Prefer: true}})
	defer c.Stop()
	st := waitStatus(t, c, "synchronised", func(s Status) bool { return s.Synced })
	if !st.Servers[1].Selected || st.Servers[0].Selected {
		t.Errorf("the preferred server was not used: %+v", st.Servers)
	}
	if d := clk.steps[0] - 20*time.Second; d > time.Second || d < -time.Second {
		t.Errorf("stepped by %v, want about 20s", clk.steps[0])
	}
}

func itoa(n int) string { return fmtInt(n) }

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestBadReplies(t *testing.T) {
	for name, mangle := range map[string]func([]byte){
		"kiss-o'-death":   func(r []byte) { r[1] = 0; copy(r[12:16], "RATE") },
		"unsynchronised":  func(r []byte) { r[0] |= 3 << 6 },
		"stratum 16":      func(r []byte) { r[1] = 16 },
		"forged origin":   func(r []byte) { r[24] ^= 0xff },
		"no timestamps":   func(r []byte) { copy(r[32:48], make([]byte, 16)) },
		"not server mode": func(r []byte) { r[0] = r[0]&^7 | 3 },
	} {
		clk := &fakeClock{}
		port := server(t, time.Hour, 2, mangle)
		c := &Client{Clock: clk, Port: port}
		c.Configure([]Server{{Host: "127.0.0.1"}})
		st := waitStatus(t, c, name, func(s Status) bool { return !s.Servers[0].LastPoll.IsZero() })
		c.Stop()
		if steps, slews := clk.counts(); steps+slews != 0 || st.Synced || st.Servers[0].Err == "" {
			t.Errorf("%s: clock adjusted or no error: steps %d slews %d %+v", name, steps, slews, st)
		}
	}
}

func TestNoAnswerAndReconfigure(t *testing.T) {
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	port := pc.LocalAddr().(*net.UDPAddr).Port
	defer pc.Close() // listens, never answers
	clk := &fakeClock{}
	c := &Client{Clock: clk, Port: port}
	c.Configure([]Server{{Host: "127.0.0.1"}})
	st := waitStatus(t, c, "no answer", func(s Status) bool { return !s.Servers[0].LastPoll.IsZero() })
	if st.Synced || st.Servers[0].Reach || st.Servers[0].Err != "no answer" {
		t.Errorf("status: %+v", st)
	}
	// An empty list stops the client and clears the status.
	c.Configure(nil)
	if st := c.Status(); len(st.Servers) != 0 || st.Synced {
		t.Errorf("status after stop: %+v", st)
	}
}

func TestOtherService(t *testing.T) {
	dir := t.TempDir()
	mk := func(pid, comm string) {
		os.MkdirAll(filepath.Join(dir, pid), 0o755)
		os.WriteFile(filepath.Join(dir, pid, "comm"), []byte(comm+"\n"), 0o644)
	}
	mk("1", "systemd")
	mk("22", "sshd")
	if got := OtherService(dir); got != "" {
		t.Errorf("found %q", got)
	}
	mk("333", "chronyd")
	if got := OtherService(dir); got != "chronyd" {
		t.Errorf("found %q", got)
	}
}
