package rshell

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// A shell on a pty: input reaches it, its output and exit status come
// back, and the window size is set.
func TestShellOverStream(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no pty support")
	}
	a, b := net.Pipe()
	cmd := exec.Command("/bin/sh", "-c", `stty size; read x; echo "got $x"; exit 3`)
	go Serve(b, bufio.NewReader(b), cmd, 33, 101)
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	out := &syncBuf{}
	res := make(chan int, 1)
	go func() {
		st, err := Client(a, bufio.NewReader(a), inR, out, nil)
		if err != nil {
			t.Error(err)
		}
		res <- st
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "33 101") {
		if time.Now().After(deadline) {
			t.Fatalf("no window size: %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	inW.Write([]byte("hello\n"))
	select {
	case st := <-res:
		if st != 3 || !strings.Contains(out.String(), "got hello") {
			t.Errorf("status %d, output %q", st, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("shell did not end: %q", out.String())
	}
	// Input after the end stays unread.
	inW.Write([]byte("x"))
	buf := make([]byte, 1)
	inR.SetReadDeadline(time.Now().Add(time.Second))
	if n, _ := inR.Read(buf); n != 1 {
		t.Error("input after the shell ended was consumed")
	}
}
