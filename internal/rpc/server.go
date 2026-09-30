package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"mclag/internal/cli"
	"mclag/internal/commit"
)

// Authorizer maps a connecting local user to a permission class. An error
// rejects the connection with that message.
type Authorizer func(uid int, name string) (commit.Class, error)

// Server serves CLI sessions on a unix socket.
type Server struct {
	// Env returns the CLI environment for a user (Engine, HostName, …).
	Env       func(name string, class commit.Class) cli.Env
	Authorize Authorizer
	Log       *slog.Logger
	// Relay returns a connection to the master's CLI service (ServeRemote)
	// when this member is not the master: every session of this member
	// runs there (reference 1.8). nil, nil: handle the command here. An
	// error means the master cannot be reached.
	Relay func() (net.Conn, error)
	// Member is this member's id (sent to the master with a relayed
	// session: "local" and "start shell local" mean it).
	Member int
	// Synced waits (briefly) until this member has the master's revision
	// rev, when a relayed session returns to operational mode.
	Synced func(rev uint64)

	mu    sync.Mutex
	conns map[*conn]struct{}
}

// Serve accepts connections until l is closed.
func (s *Server) Serve(l *net.UnixListener) error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(c)
	}
}

type originKey struct{}

// Notify sends a message to every connected session except the one whose
// command caused it (identified through ctx). It does not block: each
// session delivers its notices in order, together with its current prompt
// and banner, as soon as it is not executing a command.
func (s *Server) Notify(ctx context.Context, text string) {
	origin, _ := ctx.Value(originKey{}).(*conn)
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		if c == origin {
			continue
		}
		select {
		case c.notes <- text:
		default: // a session that does not read: drop
		}
	}
}

// CloseSessions disconnects every session (at shutdown).
func (s *Server) CloseSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		c.c.Close()
	}
}

// peer returns the uid of the process on the other end of c.
func peer(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	return int(cred.Uid), nil
}

type conn struct {
	c       io.ReadWriteCloser
	wmu     sync.Mutex
	enc     *json.Encoder
	replies chan Msg
	done    chan struct{}
	notes   chan string
	// shMu is held while the shell executes a command and while its state
	// (prompt, banner) is sent, so replies and notices are consistent.
	shMu sync.Mutex
}

func (c *conn) send(m Msg) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.enc.Encode(m)
}

// wait blocks for the client's reply to a question.
func (c *conn) wait(ctx context.Context) (Msg, error) {
	select {
	case m := <-c.replies:
		if m.Err != "" {
			return m, errors.New(m.Err)
		}
		return m, nil
	case <-c.done:
		return Msg{}, io.EOF
	case <-ctx.Done():
		return Msg{}, ctx.Err()
	}
}

// term implements cli.Terminal over the connection.
type term struct {
	c   *conn
	ctx context.Context
}

func (t *term) Ask(prompt string, echo bool) (string, error) {
	if err := t.c.send(Msg{T: "ask", Prompt: prompt, Echo: echo}); err != nil {
		return "", err
	}
	m, err := t.c.wait(t.ctx)
	return m.Text, err
}

func (t *term) ReadText(prompt string) (string, error) {
	if err := t.c.send(Msg{T: "readtext", Prompt: prompt}); err != nil {
		return "", err
	}
	m, err := t.c.wait(t.ctx)
	return m.Text, err
}

func (t *term) ReadFile(name string) ([]byte, error) {
	if err := t.c.send(Msg{T: "readfile", Name: name}); err != nil {
		return nil, err
	}
	m, err := t.c.wait(t.ctx)
	return m.Data, err
}

func (t *term) WriteFile(name string, data []byte) error {
	if err := t.c.send(Msg{T: "writefile", Name: name, Data: data}); err != nil {
		return err
	}
	_, err := t.c.wait(t.ctx)
	return err
}

func newConn(rw io.ReadWriteCloser) *conn {
	return &conn{c: rw, enc: json.NewEncoder(rw), replies: make(chan Msg, 1), done: make(chan struct{}), notes: make(chan string, 32)}
}

func (s *Server) handle(uc *net.UnixConn) {
	defer uc.Close()
	c := newConn(uc)
	uid, err := peer(uc)
	if err != nil {
		s.Log.Error("cli: peer credentials", "err", err)
		return
	}
	name := strconv.Itoa(uid)
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	class, err := s.Authorize(uid, name)
	if err != nil {
		s.Log.Warn("cli: login rejected", "facility", "authorization", "user", name, "uid", uid, "err", err)
		_ = c.send(Msg{T: "done", Text: fmt.Sprintf("error: %v\n", err), Exit: true})
		return
	}
	s.Log.Info("cli: login", "facility", "authorization", "user", name, "class", class)
	s.serve(c, name, class, true, 0)
}

// RemoteHello is the first line of a relayed configuration session: the
// user as authenticated by the member it logged in to.
type RemoteHello struct {
	User   string `json:"user"`
	Class  string `json:"class"`
	Member int    `json:"member"` // the member the user is connected to
}

// ServeRemote serves configuration sessions relayed by other members
// (the listener only accepts authenticated stack members).
func (s *Server) ServeRemote(l net.Listener) error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	for {
		nc, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			defer nc.Close()
			nc.SetReadDeadline(time.Now().Add(10 * time.Second))
			r := bufio.NewReader(nc)
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			nc.SetReadDeadline(time.Time{})
			var h RemoteHello
			if json.Unmarshal(line, &h) != nil || h.User == "" {
				return
			}
			class := commit.ParseClass(h.Class)
			s.Log.Info("cli: session relayed from another member", "facility", "authorization",
				"user", h.User, "member", h.Member, "class", class)
			s.serve(newConn(bufferedConn{r, nc}), h.User, class, false, h.Member)
		}()
	}
}

type bufferedConn struct {
	r *bufio.Reader
	net.Conn
}

func (b bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// relay is a session relayed to the master.
type relay struct {
	nc  net.Conn
	rd  *bufio.Reader
	enc *json.Encoder
	wmu sync.Mutex
	// prompt and banner of the master's session (for local notices);
	// busy: a request waits for its reply; cfg: the master's session is
	// in configuration mode.
	mu             sync.Mutex
	prompt, banner string
	busy, cfg      bool
	ended          bool
}

func (r *relay) forward(m Msg) error {
	if m.T == "exec" || m.T == "complete" || m.T == "help" {
		r.mu.Lock()
		r.busy = true
		r.mu.Unlock()
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	return r.enc.Encode(m)
}

// end marks the relay ended; it reports whether it was still active.
func (r *relay) end() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := !r.ended
	r.ended = true
	r.nc.Close()
	return was
}

// serve runs a CLI session for an authenticated user. local: the session
// is on this member (it is relayed to the master when this member is not
// the master); otherwise origin is the member it was relayed from.
func (s *Server) serve(c *conn, name string, class commit.Class, local bool, origin int) {
	env := s.Env(name, class)
	env.Origin = origin
	sh := cli.New(env)
	defer sh.Close()

	s.mu.Lock()
	if s.conns == nil {
		s.conns = map[*conn]struct{}{}
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		s.Log.Info("cli: logout", "facility", "authorization", "user", name)
	}()

	var rlMu sync.Mutex
	var rl *relay
	current := func() *relay {
		rlMu.Lock()
		defer rlMu.Unlock()
		return rl
	}
	// lost: the master could not be reached (commands run here), since
	// when (retried at most every relayRetry).
	var lost time.Time
	connect := func() (*relay, error) {
		nc, err := s.Relay()
		if err != nil || nc == nil {
			return nil, err
		}
		return s.startRelay(nc, name, class)
	}
	hello := Msg{T: "hello", Name: class.String(), Prompt: sh.Prompt(), Banner: sh.Banner()}
	if local && s.Relay != nil {
		r, err := connect()
		switch {
		case err != nil:
			lost = time.Now()
			hello.Banner = s.lostBanner(err) + hello.Banner
		case r != nil:
			rl = r
			hello.Prompt, hello.Banner = r.prompt, r.banner
		}
	}
	if err := c.send(hello); err != nil {
		return
	}
	if r := current(); r != nil {
		go s.pump(c, sh, r, &rlMu, &rl)
	}
	defer func() {
		if r := current(); r != nil {
			r.end()
		}
	}()
	go func() {
		for {
			select {
			case text := <-c.notes:
				c.shMu.Lock()
				if r := current(); r != nil {
					r.mu.Lock()
					_ = c.send(Msg{T: "notify", Text: text, Prompt: r.prompt, Banner: r.banner, Cfg: true})
					r.mu.Unlock()
				} else {
					_ = c.send(Msg{T: "notify", Text: text, Prompt: sh.Prompt(), Banner: sh.Banner(), Cfg: sh.InConfig()})
				}
				c.shMu.Unlock()
			case <-c.done:
				return
			}
		}
	}()

	// The reader routes replies to the running command and queues requests.
	reqs := make(chan Msg)
	var mu sync.Mutex
	cancel := context.CancelFunc(func() {})
	go func() {
		defer close(c.done)
		sc := bufio.NewScanner(c.c)
		sc.Buffer(make([]byte, 64<<10), MaxMsg)
		for sc.Scan() {
			var m Msg
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				s.Log.Warn("cli: bad message", "user", name, "err", err)
				return
			}
			if r := current(); r != nil && (m.T == "answer" || m.T == "file" || m.T == "interrupt") {
				_ = r.forward(m)
				continue
			}
			switch m.T {
			case "answer", "file":
				select {
				case c.replies <- m:
				default: // nobody asked; drop
				}
			case "interrupt":
				mu.Lock()
				cancel()
				mu.Unlock()
			default:
				select {
				case reqs <- m:
				case <-c.done:
					return
				}
			}
		}
	}()

	for {
		var m Msg
		select {
		case m = <-reqs:
		case <-c.done:
			return
		}
		if r := current(); r != nil {
			if err := r.forward(m); err != nil {
				s.endRelay(c, sh, r, &rlMu, &rl)
			}
			continue
		}
		// Every command runs on the master (reference 1.8). Without it,
		// commands run here; configuration mode needs it.
		needs := m.T == "exec" && sh.NeedsMaster(m.Line)
		if local && m.T == "exec" && s.Relay != nil && (needs || lost.IsZero() || time.Since(lost) > relayRetry) {
			r, err := connect()
			if err != nil {
				wasLost := !lost.IsZero()
				lost = time.Now()
				if needs {
					c.shMu.Lock()
					err = c.send(Msg{T: "done", Text: fmt.Sprintf("error: configuration unavailable: %v\n", err), Prompt: sh.Prompt(), Banner: s.lostBanner(err) + sh.Banner()})
					c.shMu.Unlock()
					if err != nil {
						return
					}
					continue
				}
				if !wasLost {
					c.shMu.Lock()
					_ = c.send(Msg{T: "notify", Text: strings.TrimSuffix(strings.TrimPrefix(s.lostBanner(err), "*** "), " ***\n"), Prompt: sh.Prompt(), Banner: sh.Banner()})
					c.shMu.Unlock()
				}
			}
			if r != nil {
				lost = time.Time{}
				rlMu.Lock()
				rl = r
				rlMu.Unlock()
				go s.pump(c, sh, r, &rlMu, &rl)
				if err := r.forward(m); err != nil {
					s.endRelay(c, sh, r, &rlMu, &rl)
				}
				continue
			}
		}
		var reply Msg
		c.shMu.Lock()
		switch m.T {
		case "exec":
			if strings.TrimSpace(m.Line) != "" { // an empty line only refreshes prompt and banner
				s.Log.Info("cli command", "facility", "interactive-commands", "user", name, "command", m.Line)
			}
			ctx, cf := context.WithCancel(context.WithValue(context.Background(), originKey{}, c))
			mu.Lock()
			cancel = cf
			mu.Unlock()
			rep := sh.Execute(ctx, m.Line, &term{c: c, ctx: ctx})
			cf()
			reply = Msg{T: "done", Text: rep.Output, NoMore: rep.NoMore, Exit: rep.Exit, Shell: rep.Shell, Member: rep.ShellMember,
				Prompt: sh.Prompt(), Banner: sh.Banner(), Cfg: sh.InConfig()}
			if !local {
				reply.Rev = sh.ActiveSeq()
			}
		case "complete":
			reply = Msg{T: "completions", Items: items(sh.Complete(m.Line))}
		case "help":
			reply = Msg{T: "completions", Text: sh.Help(m.Line), Items: items(sh.Complete(m.Line))}
		default:
			reply = Msg{T: "done", Text: fmt.Sprintf("error: unknown request %q\n", m.T), Prompt: sh.Prompt(), Banner: sh.Banner()}
		}
		err := c.send(reply)
		c.shMu.Unlock()
		if err != nil || reply.Exit {
			return
		}
	}
}

// relayRetry: while the master cannot be reached, commands run on this
// member and reaching it is tried again at most this often.
const relayRetry = 5 * time.Second

// lostBanner is the banner line while the master cannot be reached.
func (s *Server) lostBanner(err error) string {
	return fmt.Sprintf("*** master not reachable (%v); commands run on member %d, 'start shell local' for a shell ***\n", err, s.Member)
}

// startRelay opens the relayed session on the master and reads its hello.
func (s *Server) startRelay(nc net.Conn, name string, class commit.Class) (*relay, error) {
	b, _ := json.Marshal(RemoteHello{User: name, Class: class.String(), Member: s.Member})
	nc.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := nc.Write(append(b, '\n')); err != nil {
		nc.Close()
		return nil, err
	}
	r := &relay{nc: nc, rd: bufio.NewReaderSize(nc, 64<<10), enc: json.NewEncoder(nc)}
	line, err := r.rd.ReadBytes('\n')
	var m Msg
	if err == nil && (json.Unmarshal(line, &m) != nil || m.T != "hello") {
		err = errors.New("unexpected answer")
	}
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("master: %w", err)
	}
	r.prompt, r.banner = m.Prompt, m.Banner
	nc.SetDeadline(time.Time{})
	return r, nil
}

// pump passes the master's messages to the client until the session ends
// or the master is lost.
func (s *Server) pump(c *conn, sh *cli.Shell, r *relay, rlMu *sync.Mutex, rl **relay) {
	sc := bufio.NewScanner(r.rd)
	sc.Buffer(make([]byte, 64<<10), MaxMsg)
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			break
		}
		switch m.T {
		case "done", "completions":
			r.mu.Lock()
			r.busy = false
			r.mu.Unlock()
		}
		if m.T == "done" || m.T == "notify" {
			// Leaving configuration mode: this member shows the commit
			// at once.
			r.mu.Lock()
			left := r.cfg && !m.Cfg
			r.cfg = m.Cfg
			r.mu.Unlock()
			if left && m.Rev != 0 && s.Synced != nil {
				s.Synced(m.Rev)
			}
			m.Rev = 0
		}
		if m.T == "done" && m.Exit && r.end() {
			rlMu.Lock()
			*rl = nil
			rlMu.Unlock()
			c.send(m)
			return
		}
		if m.Prompt != "" {
			r.mu.Lock()
			r.prompt, r.banner = m.Prompt, m.Banner
			r.mu.Unlock()
		}
		if c.send(m) != nil {
			break
		}
	}
	s.endRelay(c, sh, r, rlMu, rl)
}

// endRelay reports a lost master and returns to operational mode.
func (s *Server) endRelay(c *conn, sh *cli.Shell, r *relay, rlMu *sync.Mutex, rl **relay) {
	if !r.end() {
		return
	}
	rlMu.Lock()
	*rl = nil
	rlMu.Unlock()
	r.mu.Lock()
	busy, cfg := r.busy, r.cfg
	r.mu.Unlock()
	text := fmt.Sprintf("connection to the master lost; commands run on member %d now", s.Member)
	if cfg {
		text += "; configuration mode ended (the shared candidate is kept)"
	}
	c.shMu.Lock()
	defer c.shMu.Unlock()
	if busy {
		c.send(Msg{T: "done", Text: "error: " + text + "\n", Prompt: sh.Prompt(), Banner: sh.Banner()})
	} else {
		c.send(Msg{T: "notify", Text: text, Prompt: sh.Prompt(), Banner: sh.Banner()})
	}
}

func items(cs []cli.Completion) []Item {
	out := make([]Item, len(cs))
	for i, c := range cs {
		out[i] = Item{Text: c.Text, Help: c.Help, Placeholder: c.Placeholder}
	}
	return out
}
