// Package swcli is the CLI client: the login shell for SSH and serial
// users. It does line editing, completion display, paging and file access;
// every command is executed by switchd.
package swcli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/term"

	"mclag/internal/rpc"
)

// DefaultSocket is where switchd listens for CLI sessions.
const DefaultSocket = "/run/switchd/cli.sock"

// Main runs the client and returns the exit code. args excludes the
// program name.
func Main(args []string) int {
	fs := flag.NewFlagSet("swcli", flag.ContinueOnError)
	cmd := fs.String("c", "", "run one command (or several, separated by newlines) and exit")
	sock := fs.String("s", DefaultSocket, "switchd socket")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ui := &ui{in: os.Stdin, out: os.Stdout}
	ui.tty = term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))

	var c *rpc.Client
	for {
		var err error
		c, err = rpc.Dial(*sock, ui)
		if err == nil {
			break
		}
		if errors.Is(err, rpc.ErrRejected) {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		if !ui.degraded(err) {
			return 1
		}
	}
	defer c.Close()
	ui.c = c

	switch {
	case *cmd != "":
		return ui.batch(strings.NewReader(*cmd))
	case !ui.tty:
		return ui.batch(os.Stdin)
	}
	return ui.interactive()
}

type ui struct {
	c        *rpc.Client
	in       *os.File
	out      *os.File
	tty      bool
	mu       sync.Mutex // guards terminal output and the editor
	ed       *editor    // non-nil while a line is being edited
	rawState *term.State
}

// degraded handles an unreachable switchd. It returns true to retry.
func (u *ui) degraded(err error) bool {
	fmt.Fprintf(os.Stderr, "switchd is not reachable: %v\n", err)
	if os.Getuid() == 0 {
		fmt.Fprintln(os.Stderr, "degraded mode: starting a root shell (/bin/bash); run 'systemctl status switchd'")
		if e := syscall.Exec("/bin/bash", []string{"-bash"}, os.Environ()); e != nil {
			fmt.Fprintf(os.Stderr, "cannot start /bin/bash: %v\n", e)
		}
		return false
	}
	if !u.tty {
		return false
	}
	fmt.Fprint(os.Stderr, "Press Enter to retry, Ctrl-D to log out: ")
	var b [1]byte
	for {
		n, err := u.in.Read(b[:])
		if err != nil || n == 0 {
			fmt.Fprintln(os.Stderr)
			return false
		}
		if b[0] == '\n' || b[0] == '\r' {
			return true
		}
	}
}

// batch runs commands from r without line editing.
func (u *ui) batch(r io.Reader) int {
	code := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		prompt := u.c.Prompt
		m, err := u.c.Exec(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "connection to switchd lost: %v\n", err)
			return 1
		}
		if strings.Contains(m.Text, "^\n") {
			// The caret is aligned with the prompt, which batch mode does not
			// print otherwise.
			fmt.Fprintln(u.out, prompt+line)
		}
		fmt.Fprint(u.out, m.Text)
		if strings.HasPrefix(m.Text, "error") || strings.Contains(m.Text, "\nerror") || strings.Contains(m.Text, "^\n") {
			code = 1
		}
		if m.Exit {
			break
		}
	}
	return code
}

// ---- rpc.Handler ----

func (u *ui) home(name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, name)
	}
	return name
}

func (u *ui) ReadFile(name string) ([]byte, error) { return os.ReadFile(u.home(name)) }

func (u *ui) WriteFile(name string, data []byte) error {
	return os.WriteFile(u.home(name), data, 0o600)
}

// cooked runs f with the terminal in normal (non-raw) mode.
func (u *ui) cooked(f func()) {
	if u.rawState != nil {
		_ = term.Restore(int(u.in.Fd()), u.rawState)
		defer u.makeRaw()
	}
	f()
}

func (u *ui) Ask(prompt string, echo bool) (string, error) {
	var line string
	var err error
	u.cooked(func() {
		fmt.Fprint(u.out, prompt)
		if !echo && u.tty {
			var b []byte
			b, err = term.ReadPassword(int(u.in.Fd()))
			fmt.Fprintln(u.out)
			line = string(b)
			return
		}
		line, err = readLine(u.in)
	})
	return line, err
}

func (u *ui) ReadText(prompt string) (string, error) {
	var text string
	var err error
	u.cooked(func() {
		fmt.Fprintln(u.out, prompt)
		var b []byte
		b, err = io.ReadAll(u.in) // until Ctrl-D (EOF on a terminal is not sticky)
		text = string(b)
	})
	return text, err
}

// readLine reads up to a newline without buffering beyond it.
func readLine(f *os.File) (string, error) {
	var b []byte
	var c [1]byte
	for {
		n, err := f.Read(c[:])
		if n == 1 {
			if c[0] == '\n' {
				return strings.TrimSuffix(string(b), "\r"), nil
			}
			b = append(b, c[0])
		}
		if err != nil {
			return string(b), err
		}
	}
}

func (u *ui) Notify(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	msg := "\r\n*** " + strings.TrimSpace(text) + " ***\r\n"
	if u.ed != nil {
		u.ed.clear()
		u.write(msg)
		u.ed.redraw()
		return
	}
	u.write(msg)
}

// write prints text, translating newlines while the terminal is raw.
func (u *ui) write(s string) {
	if u.rawState != nil {
		s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
	}
	_, _ = io.WriteString(u.out, s)
}

func (u *ui) makeRaw() {
	st, err := term.MakeRaw(int(u.in.Fd()))
	if err == nil {
		u.rawState = st
	}
}

// ---- interactive loop ----

func (u *ui) interactive() int {
	u.makeRaw()
	defer func() {
		if u.rawState != nil {
			u.write("\x1b[?2004l")
			_ = term.Restore(int(u.in.Fd()), u.rawState)
		}
	}()
	u.write("\x1b[?2004h") // bracketed paste
	hist := &history{}
	keys := newKeyReader(u.in)
	var queued []string // further lines of a multi-line paste
	for {
		var line string
		if len(queued) > 0 {
			line, queued = queued[0], queued[1:]
			u.write(u.c.Banner + u.c.Prompt + line + "\n")
		} else {
			u.mu.Lock()
			u.write(u.c.Banner)
			u.ed = newEditor(u, u.c.Prompt, hist)
			u.ed.redraw()
			u.mu.Unlock()
			var ok bool
			line, queued, ok = u.edit(keys)
			u.mu.Lock()
			u.ed = nil
			u.mu.Unlock()
			if !ok {
				line = "exit"
				u.write("exit\n")
			}
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		hist.add(line)
		m, err := u.execInterruptible(line)
		if err != nil {
			u.write(fmt.Sprintf("connection to switchd lost: %v\n", err))
			return 1
		}
		u.page(m.Text, m.NoMore, keys)
		if m.Exit {
			return 0
		}
	}
}

// execInterruptible runs a command; Ctrl-C (SIGINT in cooked mode) cancels
// it on the server.
func (u *ui) execInterruptible(line string) (rpc.Msg, error) {
	var m rpc.Msg
	var err error
	u.cooked(func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		done := make(chan struct{})
		go func() {
			for {
				select {
				case <-sig:
					u.c.Interrupt()
				case <-done:
					return
				}
			}
		}()
		m, err = u.c.Exec(line)
		signal.Stop(sig)
		close(done)
	})
	return m, err
}

// page shows output, pausing every screenful unless noMore.
func (u *ui) page(text string, noMore bool, keys *keyReader) {
	if text == "" {
		return
	}
	lines := strings.SplitAfter(strings.TrimSuffix(text, "\n"), "\n")
	_, h, err := term.GetSize(int(u.out.Fd()))
	if noMore || err != nil || h < 3 || len(lines) < h {
		u.write(text)
		if !strings.HasSuffix(text, "\n") {
			u.write("\n")
		}
		return
	}
	i := 0
	step := h - 1
	for i < len(lines) {
		end := min(i+step, len(lines))
		u.write(strings.Join(lines[i:end], ""))
		if !strings.HasSuffix(lines[end-1], "\n") {
			u.write("\n")
		}
		i = end
		if i >= len(lines) {
			return
		}
		u.write("---(more)---")
		k := keys.next()
		u.write("\r\x1b[K")
		switch {
		case k.r == ' ':
			step = h - 1
		case k.r == '\r' || k.r == '\n' || k.code == keyDown:
			step = 1
		default: // q, Ctrl-C, anything else
			return
		}
	}
}

// edit reads one line. It returns the line, further pasted lines, and
// false on Ctrl-D at an empty line or end of input.
func (u *ui) edit(keys *keyReader) (string, []string, bool) {
	for {
		k := keys.next()
		u.mu.Lock()
		ed := u.ed
		if k.eof {
			u.mu.Unlock()
			return "", nil, false
		}
		if k.paste != "" {
			text := strings.ReplaceAll(k.paste, "\r\n", "\n")
			text = strings.ReplaceAll(text, "\r", "\n")
			parts := strings.Split(text, "\n")
			ed.insert(parts[0])
			if len(parts) > 1 {
				line := string(ed.buf)
				u.write("\n")
				u.mu.Unlock()
				var rest []string
				for _, p := range parts[1:] {
					if strings.TrimSpace(p) != "" {
						rest = append(rest, p)
					}
				}
				return line, rest, true
			}
			u.mu.Unlock()
			continue
		}
		switch {
		case k.code == keyEnter:
			line := string(ed.buf)
			ed.end()
			u.write("\n")
			u.mu.Unlock()
			return line, nil, true
		case k.code == keyCtrlD:
			if len(ed.buf) == 0 {
				u.mu.Unlock()
				return "", nil, false
			}
			ed.deleteRight()
		case k.code == keyCtrlC:
			ed.end()
			u.write("^C\n")
			ed.buf, ed.pos = nil, 0
			ed.redraw()
		case k.code == keyTab:
			u.complete(ed)
		case k.r == '?' && !inQuote(string(ed.buf[:ed.pos])):
			line := string(ed.buf[:ed.pos])
			u.mu.Unlock()
			help, err := u.c.Help(line)
			u.mu.Lock()
			ed.end()
			u.write("?\n")
			if err == nil {
				u.write(help)
			}
			ed.redraw()
		default:
			ed.key(k)
		}
		u.mu.Unlock()
	}
}

func inQuote(s string) bool {
	in := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && in:
			i++
		case s[i] == '"':
			in = !in
		}
	}
	return in
}

// complete handles Tab. Caller holds u.mu.
func (u *ui) complete(ed *editor) {
	before := string(ed.buf[:ed.pos])
	u.mu.Unlock()
	items, err := u.c.Complete(before)
	u.mu.Lock()
	if err != nil || inQuote(before) {
		return
	}
	partial := ""
	if !strings.HasSuffix(before, " ") {
		if i := strings.LastIndexAny(before, " \t"); i >= 0 {
			partial = before[i+1:]
		} else {
			partial = before
		}
	}
	var words []string
	for _, it := range items {
		if !it.Placeholder && strings.HasPrefix(it.Text, partial) {
			words = append(words, it.Text)
		}
	}
	switch {
	case len(words) == 1:
		ed.insert(words[0][len(partial):] + " ")
		return
	case len(words) > 1:
		if cp := commonPrefix(words); len(cp) > len(partial) {
			ed.insert(cp[len(partial):])
			return
		}
	}
	help, err := u.c.Help(before)
	if err != nil {
		return
	}
	ed.end()
	u.write("\n" + help)
	ed.redraw()
}

func commonPrefix(ws []string) string {
	p := ws[0]
	for _, w := range ws[1:] {
		for !strings.HasPrefix(w, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}

// ---- history ----

type history struct {
	lines []string
}

func (h *history) add(l string) {
	if n := len(h.lines); n > 0 && h.lines[n-1] == l {
		return
	}
	h.lines = append(h.lines, l)
	if len(h.lines) > 1000 {
		h.lines = h.lines[len(h.lines)-1000:]
	}
}
