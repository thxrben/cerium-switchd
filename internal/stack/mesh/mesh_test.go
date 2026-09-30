package mesh

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// link connects two meshes over an in-memory session; the returned function
// cuts it.
func link(a, b *Mesh) func() {
	ca, cb := net.Pipe()
	go a.AddPeer(b.Self, ca)
	go b.AddPeer(a.Self, cb)
	return func() { ca.Close(); cb.Close() }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func meshes(n int) []*Mesh {
	out := make([]*Mesh, n+1)
	for i := 1; i <= n; i++ {
		out[i] = New(i, nil)
	}
	return out
}

func reaches(m *Mesh, want ...int) func() bool {
	return func() bool { return slices.Equal(m.Reachable(), want) }
}

func TestChainRoutes(t *testing.T) {
	m := meshes(4)
	link(m[1], m[2])
	link(m[2], m[3])
	link(m[3], m[4])
	waitFor(t, "1 reaches all", reaches(m[1], 2, 3, 4))
	waitFor(t, "4 reaches all", reaches(m[4], 1, 2, 3))
	for dst, hop := range map[int]int{2: 2, 3: 2, 4: 2} {
		if got := m[1].NextHop(dst); got != hop {
			t.Errorf("1 -> %d via %d, want %d", dst, got, hop)
		}
	}
	if got := m[3].NextHop(1); got != 2 {
		t.Errorf("3 -> 1 via %d", got)
	}
}

func echo(t *testing.T, m *Mesh, service string) {
	l := m.Listen(service)
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(c, c)
				c.Close()
			}()
		}
	}()
}

func TestStreamAcrossHops(t *testing.T) {
	m := meshes(3)
	link(m[1], m[2])
	link(m[2], m[3])
	waitFor(t, "1 reaches 3", reaches(m[1], 2, 3))
	echo(t, m[3], "echo")

	s, err := m[1].Dial(3, "echo", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.RemoteAddr().String() != "member 3" {
		t.Errorf("remote %s", s.RemoteAddr())
	}
	// Several windows' worth, so flow control has to work.
	data := make([]byte, 5*Window+123)
	rand.Read(data)
	go func() {
		if _, err := s.Write(data); err != nil {
			t.Error(err)
		}
	}()
	got := make([]byte, len(data))
	s.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(s, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("data corrupted")
	}
	s.Close()
	if _, err := s.Write([]byte("x")); err == nil {
		t.Error("write after close succeeded")
	}
	waitFor(t, "streams forgotten", func() bool {
		for _, x := range m[1:] {
			x.mu.Lock()
			n := len(x.streams)
			x.mu.Unlock()
			if n != 0 {
				return false
			}
		}
		return true
	})
}

func TestUnknownService(t *testing.T) {
	m := meshes(2)
	link(m[1], m[2])
	waitFor(t, "1 reaches 2", reaches(m[1], 2))
	if _, err := m[1].Dial(2, "nothing", time.Second); err == nil || !strings.Contains(err.Error(), "reset") {
		t.Fatalf("dial unknown service: %v", err)
	}
	if _, err := m[1].Dial(7, "echo", time.Second); err == nil {
		t.Fatal("dial unknown member succeeded")
	}
}

func TestReadDeadline(t *testing.T) {
	m := meshes(2)
	link(m[1], m[2])
	waitFor(t, "1 reaches 2", reaches(m[1], 2))
	echo(t, m[2], "echo")
	s, err := m[1].Dial(2, "echo", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	var ne net.Error
	if _, err := s.Read(make([]byte, 1)); !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read: %v", err)
	}
}

// A ring survives one cut: routes go the other way round. Streams to
// members that are still reachable survive if nothing was lost on the way;
// streams to members that became unreachable fail.
func TestRingCut(t *testing.T) {
	m := meshes(4)
	cut12 := link(m[1], m[2])
	link(m[2], m[3])
	link(m[3], m[4])
	cut41 := link(m[4], m[1])
	waitFor(t, "ring", reaches(m[1], 2, 3, 4))
	if hop := m[1].NextHop(2); hop != 2 {
		t.Fatalf("1 -> 2 via %d", hop)
	}
	echo(t, m[2], "echo")
	s, err := m[1].Dial(2, "echo", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch := m[1].Changed()
	cut12()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no change notification")
	}
	waitFor(t, "1 -> 2 via 4", func() bool { return m[1].NextHop(2) == 4 })
	waitFor(t, "2 -> 1 via 3", func() bool { return m[2].NextHop(1) == 3 })
	// The path changed: the stream is reset (data may have been lost), a
	// new one works over the new path.
	buf := make([]byte, 5)
	s.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := s.Read(buf); err == nil || !strings.Contains(err.Error(), "path to member 2 changed") {
		t.Fatalf("stream after reroute: %v", err)
	}
	s, err = m[1].Dial(2, "echo", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(s, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("after reroute: %q %v", buf, err)
	}

	// Cut the other side too: 1 is alone, the stream fails.
	defer s.Close()
	cut41()
	waitFor(t, "1 isolated", reaches(m[1]))
	waitFor(t, "3 lost 1", reaches(m[3], 2, 4))
	if _, err := s.Read(buf); err == nil || !strings.Contains(err.Error(), "reachable") {
		t.Fatalf("read on isolated member: %v", err)
	}
}

// A member that restarts (new mesh, low announcement history) is learned
// again by the others.
func TestRestartedMember(t *testing.T) {
	m := meshes(3)
	link(m[1], m[2])
	cut := link(m[2], m[3])
	waitFor(t, "1 reaches 3", reaches(m[1], 2, 3))
	cut()
	waitFor(t, "1 lost 3", reaches(m[1], 2))
	m[3] = New(3, nil)
	m[3].ownSeq = 1 // as if its clock were far behind
	link(m[2], m[3])
	waitFor(t, "1 reaches restarted 3", reaches(m[1], 2, 3))
	waitFor(t, "restarted 3 reaches 1", reaches(m[3], 1, 2))
}

func TestParallelLinks(t *testing.T) {
	m := meshes(2)
	cutA := link(m[1], m[2])
	link(m[1], m[2])
	waitFor(t, "1 reaches 2", reaches(m[1], 2))
	echo(t, m[2], "echo")
	cutA()
	waitFor(t, "one link left", func() bool {
		m[1].mu.Lock()
		defer m[1].mu.Unlock()
		return len(m[1].peers[2]) == 1
	})
	if !slices.Equal(m[1].Reachable(), []int{2}) {
		t.Fatal("lost member 2 with one link left")
	}
	s, err := m[1].Dial(2, "echo", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestMalformed(t *testing.T) {
	for _, b := range [][]byte{{0, 0, 0, 1}, {0xff, 0xff, 0xff, 0xff}} {
		if _, err := readMsg(bytes.NewReader(b)); err == nil {
			t.Errorf("%x accepted", b)
		}
	}
	x := &msg{typ: tData, hops: 3, src: 1, dst: 2, stream: 7, seq: 9, payload: []byte("abc")}
	y, err := readMsg(bytes.NewReader(x.encode()))
	if err != nil || y.typ != x.typ || y.hops != 3 || y.src != 1 || y.dst != 2 || y.stream != 7 || y.seq != 9 || string(y.payload) != "abc" {
		t.Fatalf("round trip: %+v %v", y, err)
	}
}

// In a ring of four, the opposite member is reached both ways around; the
// others only directly.
func TestFirstHops(t *testing.T) {
	m := meshes(4)
	link(m[1], m[2])
	link(m[2], m[3])
	link(m[3], m[4])
	cut := link(m[4], m[1])
	waitFor(t, "ring", reaches(m[1], 2, 3, 4))
	want := map[int][]int{2: {2}, 3: {2, 4}, 4: {4}}
	waitFor(t, "equal-cost hops", func() bool {
		got := m[1].FirstHops()
		for d, h := range want {
			if !slices.Equal(got[d], h) {
				return false
			}
		}
		return len(got) == len(want)
	})
	cut()
	want = map[int][]int{2: {2}, 3: {2}, 4: {2}}
	waitFor(t, "hops after the cut", func() bool {
		got := m[1].FirstHops()
		for d, h := range want {
			if !slices.Equal(got[d], h) {
				return false
			}
		}
		return len(got) == len(want)
	})
}
