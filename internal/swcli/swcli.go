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
	"net"
	"os"
	"os/exec"
	"os/signal"
	osuser "os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/thxrben/cerium-switchd/internal/rpc"
	"github.com/thxrben/cerium-switchd/internal/rshell"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// Main runs the client and returns the exit code. args excludes the
// program name.
func Main(args []string) int {
	fs := flag.NewFlagSet("swcli", flag.ContinueOnError)
	cmd := fs.String("c", "", "run one command (or several, separated by newlines) and exit")
	sock := fs.String("s", rpc.DefaultSocket, "switchd socket")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// With sshd's ForceCommand (root on the CLI SSH server) the requested
	// command arrives in SSH_ORIGINAL_COMMAND.
	if *cmd == "" {
		*cmd = os.Getenv("SSH_ORIGINAL_COMMAND")
	}
	u := &ui{sock: *sock, in: os.Stdin, out: os.Stdout}
	u.tty = term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	interactive := *cmd == "" && u.tty
	if interactive && os.Getenv(childEnv) == "" {
		return supervise(args)
	}
	if os.Getenv(childEnv) != "" {
		u.report = os.NewFile(3, "supervisor")
	}

	c, err := rpc.Dial(*sock, u)
	switch {
	case err == nil:
		u.connected(c)
	case errors.Is(err, rpc.ErrRejected):
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	case !interactive:
		fmt.Fprintf(os.Stderr, "switchd is not available: %v\n", err)
		return 1
	default:
		u.cur.Store(rpc.Offline(offlinePrompt("")))
	}
	defer func() { u.cl().Close() }()

	switch {
	case *cmd != "":
		return u.batch(strings.NewReader(*cmd))
	case !u.tty:
		return u.batch(os.Stdin)
	}
	return u.interactive()
}

// exitUsage is returned for bad arguments; exit status 2 means a crash
// (Go runtime) to the supervisor.
const exitUsage = 64

type ui struct {
	sock     string
	cur      atomic.Pointer[rpc.Client] // replaced on reconnect
	in       *os.File
	out      *os.File
	tty      bool
	mu       sync.Mutex  // guards terminal output, the editor and the fields below
	ed       *editor     // non-nil while a line is being edited
	rawState *term.State // the terminal's normal (cooked) state
	raw      bool        // the terminal is raw now (main goroutine only)
	// running is set while a command executes; interrupts counts Ctrl-C
	// during it.
	running    atomic.Bool
	interrupts atomic.Int32
	inShell    bool // a Linux shell owns the terminal: print nothing
	retrying   bool // a reconnect loop is running
	wasConfig  bool // the lost session was in configuration mode
	class      string
	report     *os.File // tells the supervisor the session's class
	// wakeR/wakeW: the SIGINT handler writes to wakeW while asking is set,
	// so that a question waiting for input gives up (readInput).
	// Raw descriptors (os.File.Fd would make them blocking again); -1:
	// none.
	wakeR, wakeW int
	asking       atomic.Bool
	// nextLine is set in batch mode: questions are answered by the next
	// input line (stdin is read ahead, so it cannot be read directly).
	nextLine func() (string, bool)
}

// cl returns the current connection (possibly offline).
func (u *ui) cl() *rpc.Client { return u.cur.Load() }

// connected installs a new connection.
func (u *ui) connected(c *rpc.Client) {
	u.cur.Store(c)
	u.class = c.Class
	if u.report != nil {
		fmt.Fprintln(u.report, c.Class)
	}
}

// shellAllowed reports whether the user may get a Linux shell without
// asking switchd: root, or a user switchd reported as super-user.
func (u *ui) shellAllowed() bool {
	return os.Getuid() == 0 || u.class == "super-user"
}

// offlinePrompt derives the prompt shown while switchd is away.
func offlinePrompt(prev string) string {
	name := strings.TrimSpace(prev)
	if name != "" {
		name = name[:len(name)-1] // > or #
	} else {
		user := strconv.Itoa(os.Getuid())
		if cu, err := osuser.Current(); err == nil {
			user = cu.Username
		}
		host, _ := os.Hostname()
		host, _, _ = strings.Cut(host, ".")
		name = user + "@" + host
	}
	return name + " (switchd not available)> "
}

func (u *ui) offlineHelp() string {
	if u.shellAllowed() {
		return "Enter 'start shell' for a Linux shell or 'exit' to log out, or wait."
	}
	return "Enter 'exit' to log out, or wait."
}

// Disconnected switches to offline mode and reconnects in the background.
func (u *ui) Disconnected(c *rpc.Client) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cl() != c {
		return
	}
	p, _ := c.State()
	u.wasConfig = strings.HasSuffix(p, "# ")
	off := rpc.Offline(offlinePrompt(p))
	u.cur.Store(off)
	u.announce("switchd is not available (connection lost); reconnecting in the background ***\r\n*** "+u.offlineHelp(), off)
	if !u.retrying {
		u.retrying = true
		go u.reconnect()
	}
}

// announce shows a notice and redraws the line being edited with c's
// prompt and banner. Caller holds u.mu.
func (u *ui) announce(text string, c *rpc.Client) {
	if u.inShell {
		return
	}
	msg := "\r\n*** " + strings.TrimSpace(text) + " ***\r\n"
	if u.ed == nil {
		u.write(msg)
		return
	}
	prompt, banner := c.State()
	u.ed.clear()
	u.write(msg + banner)
	u.ed.prompt = prompt
	u.ed.redraw()
}

// reconnect retries until switchd accepts the session again.
func (u *ui) reconnect() {
	delay := time.Second
	for {
		time.Sleep(delay)
		c, err := rpc.Dial(u.sock, u)
		if err != nil {
			if errors.Is(err, rpc.ErrRejected) {
				delay = 10 * time.Second
			}
			continue
		}
		u.mu.Lock()
		if c.Closed() { // lost again before it was installed
			u.mu.Unlock()
			continue
		}
		u.connected(c)
		u.retrying = false
		msg := "switchd is available again"
		if u.wasConfig {
			msg += " ***\r\n*** You are in operational mode; uncommitted changes to the shared configuration were kept (private/exclusive ones are lost)"
			u.wasConfig = false
		}
		u.announce(msg, c)
		u.mu.Unlock()
		return
	}
}

// start begins offline mode when switchd was not reachable at login.
func (u *ui) startOffline() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.cl().Closed() || u.retrying {
		return
	}
	u.announce("switchd is not available; reconnecting in the background ***\r\n*** "+u.offlineHelp(), u.cl())
	u.retrying = true
	go u.reconnect()
}

// offline handles a command line while switchd is away. It returns true
// to log out.
func (u *ui) offline(line string) bool {
	switch strings.Join(strings.Fields(line), " ") {
	case "exit", "quit", "logout":
		return true
	case "start shell":
		if !u.shellAllowed() {
			u.write("error: permission denied\n")
			return false
		}
		u.runShell()
	default:
		u.write("error: switchd is not available; the command was not run. " + u.offlineHelp() + "\n")
	}
	return false
}

// batch runs commands from r without line editing.
func (u *ui) batch(r io.Reader) int {
	code := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	u.nextLine = func() (string, bool) {
		if !sc.Scan() {
			return "", false
		}
		return sc.Text(), true
	}
	defer func() { u.nextLine = nil }()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		prompt, _ := u.cl().State()
		m, err := u.cl().Exec(line)
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
		if m.Shell {
			fmt.Fprintln(os.Stderr, "error: start shell needs an interactive terminal")
			code = 1
		}
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

func (u *ui) ReadFile(name string) ([]byte, error) { return hwio.ReadFile(u.home(name)) }

func (u *ui) WriteFile(name string, data []byte) error {
	return hwio.WriteFile(u.home(name), data, 0o600)
}

// cooked runs f with the terminal in normal (non-raw) mode. It nests: a
// question asked while a command runs (already cooked) leaves the terminal
// cooked for the rest of the command.
func (u *ui) cooked(f func()) {
	if u.rawState != nil && u.raw {
		_ = term.Restore(int(u.in.Fd()), u.rawState)
		u.raw = false
		defer u.makeRaw()
	}
	f()
}

func (u *ui) Ask(prompt string, echo bool) (string, error) {
	if u.nextLine != nil {
		fmt.Fprint(u.out, prompt)
		line, ok := u.nextLine()
		if !ok {
			fmt.Fprintln(u.out)
			return "", io.EOF
		}
		if echo {
			fmt.Fprintln(u.out, line)
		} else {
			fmt.Fprintln(u.out)
		}
		return line, nil
	}
	var line string
	var err error
	u.cooked(func() {
		fmt.Fprint(u.out, prompt)
		line, err = u.readInput(true, !echo && u.tty)
		if errors.Is(err, errInterrupted) {
			fmt.Fprintln(u.out)
		}
	})
	return line, err
}

// errInterrupted: Ctrl-C at a question or while text is read.
var errInterrupted = errors.New("interrupted")

// readInput reads an answer (up to a newline) or text (up to end of
// input) in cooked mode. Ctrl-C ends it with errInterrupted: the signal
// handler wakes the poll through u.wake (a blocked read would keep the
// prompt waiting for Enter while the command was already cancelled).
// hidden turns the echo off (passwords).
func (u *ui) readInput(line, hidden bool) (string, error) {
	fd := int(u.in.Fd())
	if hidden {
		if tio, err := unix.IoctlGetTermios(fd, unix.TCGETS); err == nil {
			old := *tio
			tio.Lflag &^= unix.ECHO
			_ = unix.IoctlSetTermios(fd, unix.TCSETS, tio)
			defer func() {
				_ = unix.IoctlSetTermios(fd, unix.TCSETS, &old)
				fmt.Fprintln(u.out)
			}()
		}
	}
	u.drainWake()
	u.asking.Store(true)
	defer u.asking.Store(false)
	var b []byte
	var c [1]byte
	for {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if u.wakeR > 0 {
			fds = append(fds, unix.PollFd{Fd: int32(u.wakeR), Events: unix.POLLIN})
		}
		if _, err := unix.Poll(fds, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return string(b), err
		}
		if len(fds) > 1 && fds[1].Revents != 0 {
			u.drainWake()
			return "", errInterrupted
		}
		if fds[0].Revents == 0 {
			continue
		}
		// One byte at a time: nothing beyond the answer is taken from the
		// input (the line editor reads the rest).
		n, err := unix.Read(fd, c[:])
		switch {
		case errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN):
			continue
		case err != nil:
			return string(b), err
		case n == 0:
			return string(b), io.EOF
		}
		if line && c[0] == '\n' {
			return strings.TrimSuffix(string(b), "\r"), nil
		}
		b = append(b, c[0])
	}
}

// drainWake empties the wake pipe (a Ctrl-C from before the question).
func (u *ui) drainWake() {
	if u.wakeR <= 0 {
		return
	}
	var buf [64]byte
	for {
		if n, err := unix.Read(u.wakeR, buf[:]); n <= 0 || err != nil {
			return
		}
	}
}

func (u *ui) ReadText(prompt string) (string, error) {
	var text string
	var err error
	u.cooked(func() {
		fmt.Fprintln(u.out, prompt)
		// Until Ctrl-D (EOF on a terminal is not sticky), or Ctrl-C.
		text, err = u.readInput(false, false)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		if errors.Is(err, errInterrupted) {
			fmt.Fprintln(u.out)
		}
	})
	return text, err
}

// Print shows output of a command that is still running.
func (u *ui) Print(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.write(text)
}

func (u *ui) Notify(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.announce(text, u.cl())
}

// write prints text, translating newlines while the terminal is raw.
func (u *ui) write(s string) {
	if u.rawState != nil {
		s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
	}
	_, _ = io.WriteString(u.out, s)
}

// makeRaw puts the terminal into raw mode. rawState keeps the terminal's
// normal state from the first call: a later call while raw would otherwise
// save the raw state as the one to go back to, and every question after it
// would be asked in raw mode (no echo, Enter not ending the answer).
func (u *ui) makeRaw() {
	st, err := term.MakeRaw(int(u.in.Fd()))
	if err == nil {
		if u.rawState == nil {
			u.rawState = st
		}
		u.raw = true
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
	// Ctrl-C in cooked mode (while a command runs, or at a question)
	// interrupts the command; a second one while the same command still
	// runs drops a switchd that does not react.
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err == nil {
		u.wakeR, u.wakeW = p[0], p[1]
	}
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		for range sig {
			if u.asking.Load() && u.wakeW > 0 {
				_, _ = unix.Write(u.wakeW, []byte{0})
			}
			c := u.cl()
			if n := u.interrupts.Add(1); n >= 2 && u.running.Load() {
				c.Abort()
			}
			c.Interrupt()
		}
	}()
	u.startOffline()
	hist := &history{}
	keys := newKeyReader(u.in)
	var queued []string // further lines of a multi-line paste
	for {
		var line string
		prompt, banner := u.cl().State()
		if len(queued) > 0 {
			line, queued = queued[0], queued[1:]
			u.write(banner + prompt + line + "\n")
		} else {
			u.mu.Lock()
			u.write(banner)
			u.ed = newEditor(u, prompt, hist)
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
		c := u.cl()
		if strings.TrimSpace(line) == "" {
			if !c.Closed() {
				_, _ = c.Exec("") // refreshes prompt and banner
			}
			continue
		}
		hist.add(line)
		if c.Closed() {
			if u.offline(line) {
				return 0
			}
			continue
		}
		m, err := u.execInterruptible(c, line)
		if err != nil {
			u.write("error: the connection to switchd broke while the command was running; it may or may not have completed\n")
			continue
		}
		u.page(m.Text, m.NoMore, keys)
		switch {
		case m.Shell && m.Member != 0:
			u.runRemoteShell(m.Member)
		case m.Shell:
			u.runShell()
		}
		if m.Exit {
			return 0
		}
	}
}

// execInterruptible runs a command in cooked mode, so that Ctrl-C (SIGINT)
// cancels it on the server.
func (u *ui) execInterruptible(c *rpc.Client, line string) (rpc.Msg, error) {
	var m rpc.Msg
	var err error
	u.cooked(func() {
		u.interrupts.Store(0)
		u.running.Store(true)
		m, err = c.Exec(line)
		u.running.Store(false)
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
			help, err := u.cl().Help(line)
			u.mu.Lock()
			ed.end()
			u.write("?\n")
			if err == nil {
				u.write(help)
			} else {
				u.write(u.offlineHelp() + "\n")
			}
			u.repaint(ed)
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
	items, err := u.cl().Complete(before)
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
	help, err := u.cl().Help(before)
	if err != nil {
		return
	}
	ed.end()
	u.write("\n" + help)
	u.repaint(ed)
}

// repaint draws the banner (the role line and the edit level above the
// prompt) again, then the prompt and the line being edited: after a help
// or completion listing the prompt is no longer at the top of the screen
// section it was drawn with. Caller holds u.mu.
func (u *ui) repaint(ed *editor) {
	_, banner := u.cl().State()
	u.write(banner)
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

// runShell runs the user's Linux shell with a normal terminal and returns
// to the CLI when it exits.
func (u *ui) runShell() {
	u.mu.Lock()
	u.inShell = true
	u.mu.Unlock()
	u.cooked(func() {
		u.write("\x1b[?2004l")
		saneTerminal(int(u.in.Fd()))
		runBash()
		reclaimTerminal(int(u.in.Fd()))
		u.write("\x1b[?2004h")
	})
	u.mu.Lock()
	u.inShell = false
	if u.cl().Closed() {
		u.write("*** switchd is still not available. " + u.offlineHelp() + " ***\n")
	}
	u.mu.Unlock()
	if c := u.cl(); !c.Closed() {
		_, _ = c.Exec("") // notices were not shown during the shell: refresh
	}
}

// runRemoteShell runs a shell on another member (the master of a session
// forwarded to it, reference 1.8) through switchd's shell socket.
func (u *ui) runRemoteShell(member int) {
	nc, err := net.Dial("unix", filepath.Join(filepath.Dir(u.sock), rshell.SocketName))
	if err != nil {
		u.write(fmt.Sprintf("error: %v\n", err))
		return
	}
	defer nc.Close()
	req := rshell.Request{Member: member, Term: os.Getenv("TERM")}
	if w, h, err := term.GetSize(int(u.out.Fd())); err == nil {
		req.Rows, req.Cols = uint16(h), uint16(w)
	}
	r := bufio.NewReader(nc)
	var st rshell.Status
	if err := rshell.WriteLine(nc, req); err == nil {
		err = rshell.ReadLine(r, &st)
		if err != nil {
			st.Err = err.Error()
		}
	} else {
		st.Err = err.Error()
	}
	if st.Err != "" {
		u.write("error: shell on member " + strconv.Itoa(member) + ": " + st.Err + "\n")
		return
	}
	u.mu.Lock()
	u.inShell = true
	u.mu.Unlock()
	resize := make(chan [2]uint16, 1)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-winch:
				if w, h, err := term.GetSize(int(u.out.Fd())); err == nil {
					select {
					case resize <- [2]uint16{uint16(h), uint16(w)}:
					default:
					}
				}
			}
		}
	}()
	u.write("\x1b[?2004l")
	// The terminal stays raw: the member's pty does the line editing.
	_, err = rshell.Client(nc, r, u.in, u.out, resize)
	signal.Stop(winch)
	close(stop)
	// A program on the member may have left the terminal (emulator) in the
	// alternate screen or with application cursor keys and keypad.
	u.write("\x1b[?1049l\x1b[?1l\x1b>\x1b[?25h")
	u.write("\x1b[?2004h")
	if err != nil {
		u.write(fmt.Sprintf("\n*** shell on member %d: %v ***\n", member, err))
	}
	u.mu.Lock()
	u.inShell = false
	u.mu.Unlock()
	if c := u.cl(); !c.Closed() {
		_, _ = c.Exec("") // notices were not shown during the shell: refresh
	}
}

// saneTerminal makes sure a shell gets a usable terminal: line editing,
// echo, signals, CR/NL handling and output processing on, whatever state
// the terminal was left in (users had to type "reset" in the shell).
func saneTerminal(fd int) {
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return
	}
	want := *tio
	want.Lflag |= unix.ICANON | unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ISIG | unix.IEXTEN
	want.Iflag |= unix.ICRNL | unix.IXON
	want.Iflag &^= unix.INLCR | unix.IGNCR
	want.Oflag |= unix.OPOST | unix.ONLCR
	want.Cc[unix.VMIN], want.Cc[unix.VTIME] = 1, 0
	if want != *tio {
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, &want)
	}
}

// reclaimTerminal makes swcli's process group the terminal's foreground
// group again: an interactive bash takes the terminal for its own group
// and does not give it back when it is killed; swcli would then be stopped
// (SIGTTIN) at its next read.
func reclaimTerminal(fd int) {
	pg := unix.Getpgrp()
	if cur, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP); err != nil || cur == pg {
		return
	}
	signal.Ignore(syscall.SIGTTOU) // a background group may not set it otherwise
	_ = unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, pg)
	signal.Reset(syscall.SIGTTOU)
}

// runBash runs a login bash on the terminal. SWITCHD_SHELL tells the
// console profile hook not to start the CLI again.
func runBash() {
	cmd := exec.Command("/bin/bash", "-l")
	// Under the supervisor, stderr is a pipe (for crash reports); the
	// shell gets the terminal.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stdout
	cmd.Env = slices.DeleteFunc(os.Environ(), func(e string) bool {
		return strings.HasPrefix(e, childEnv+"=") || strings.HasPrefix(e, "SWITCHD_SHELL=") || strings.HasPrefix(e, "SHELL=")
	})
	cmd.Env = append(cmd.Env, "SHELL=/bin/bash", "SWITCHD_SHELL=cli")
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
	}
}
