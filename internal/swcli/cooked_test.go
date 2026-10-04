package swcli

import (
	"os"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// openPTY returns the terminal side of a new pseudo terminal.
func openPTY(t *testing.T) *os.File {
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
	return s
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
