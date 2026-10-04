package swcli

import (
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPTY returns the terminal side of a new pseudo terminal.
func openPTY(t *testing.T) *os.File {
	_, s := openPTYPair(t)
	return s
}

// openPTYPair returns both sides: the master (the user's keyboard and
// screen) and the terminal.
func openPTYPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no pseudo terminals:", err)
	}
	t.Cleanup(func() { m.Close() })
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skip("unlockpt:", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skip("ptsname:", err)
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip("pts:", err)
	}
	t.Cleanup(func() { s.Close() })
	return m, s
}

func canonical(t *testing.T, f *os.File) bool {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag&unix.ICANON != 0 && tio.Lflag&unix.ECHO != 0
}

// A question while a command runs (cooked inside cooked) must not leave
// the terminal raw for the next question: the user's join answer could not
// be typed (no echo, Enter did not end the line) and the prompt's lines
// were not returned to the left edge.
func TestQuestionsStayCooked(t *testing.T) {
	pty := openPTY(t)
	u := &ui{in: pty, out: pty, tty: true}
	u.makeRaw()
	if canonical(t, pty) {
		t.Fatal("not raw after makeRaw")
	}
	for i := range 3 { // command after command, each asking a question
		u.cooked(func() { // the running command
			u.cooked(func() { // its question
				if !canonical(t, pty) {
					t.Fatalf("question %d asked in raw mode", i+1)
				}
			})
			if !canonical(t, pty) {
				t.Fatalf("command %d raw again after its question", i+1)
			}
		})
		if canonical(t, pty) {
			t.Fatalf("line editor not raw after command %d", i+1)
		}
	}
}

// Ctrl-C at a question ends it at once (the prompt hung until Enter, and
// the late answer went to the next question); a typed answer is returned.
func TestQuestionInterrupted(t *testing.T) {
	m, pty := openPTYPair(t)
	u := &ui{in: pty, out: pty, tty: true}
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	u.wakeR, u.wakeW = p[0], p[1]
	go func() { // the screen: keep the pty's output flowing
		var b [256]byte
		for {
			if _, err := m.Read(b[:]); err != nil {
				return
			}
		}
	}()
	type res struct {
		a   string
		err error
	}
	ask := func(echo bool) chan res {
		ch := make(chan res, 1)
		go func() {
			a, err := u.Ask("Continue? [yes,no] (no) ", echo)
			ch <- res{a, err}
		}()
		for !u.asking.Load() {
			time.Sleep(time.Millisecond)
		}
		return ch
	}
	// Answered.
	ch := ask(true)
	m.Write([]byte("yes\n"))
	if r := <-ch; r.err != nil || r.a != "yes" {
		t.Fatalf("answer %+v", r)
	}
	// Ctrl-C: the signal handler writes the wake pipe.
	ch = ask(true)
	unix.Write(u.wakeW, []byte{0})
	select {
	case r := <-ch:
		if !errors.Is(r.err, errInterrupted) {
			t.Fatalf("interrupted question returned %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the question did not give up on Ctrl-C")
	}
	// The next question works, also hidden (password).
	ch = ask(false)
	m.Write([]byte("secret\n"))
	if r := <-ch; r.err != nil || r.a != "secret" {
		t.Fatalf("hidden answer %+v", r)
	}
	if !canonical(t, pty) {
		t.Fatal("echo not restored after a hidden answer")
	}
}
