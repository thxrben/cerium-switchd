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
	// Relay returns a connection to the configuration master's CLI service
	// (ServeRemote) when this member is not the master. nil, nil: handle
	// the command here. An error is reported to the user.
	Relay func() (net.Conn, error)
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
	s.serve(c, name, class, true)
}

// RemoteHello is the first line of a relayed configuration session: the
// user as authenticated by the member it logged in to.
type RemoteHello struct {
	User   string `json:"user"`
	Class  string `json:"class"`
	Member int    `json:"member"`
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
			s.Log.Info("cli: configuration session relayed from another member", "facility", "authorization",
				"user", h.User, "member", h.Member, "class", class)
			s.serve(newConn(bufferedConn{r, nc}), h.User, class, false)
		}()
	}
}

type bufferedConn struct {
	r *bufio.Reader
	net.Conn
}

func (b bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// relay is a configuration session relayed to the master.
type relay struct {
	nc  net.Conn
	enc *json.Encoder
	wmu sync.Mutex
	// prompt and banner of the master's session (for local notices);
	// busy: a request waits for its reply.
	mu             sync.Mutex
	prompt, banner string
	busy           bool
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
// is on this member (it may be relayed to the master).
func (s *Server) serve(c *conn, name string, class commit.Class, local bool) {
	sh := cli.New(s.Env(name, class))
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

	if err := c.send(Msg{T: "hello", Name: class.String(), Prompt: sh.Prompt(), Banner: sh.Banner()}); err != nil {
		return
	}
	var rlMu sync.Mutex
	var rl *relay
	current := func() *relay {
		rlMu.Lock()
		defer rlMu.Unlock()
		return rl
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
		if local && m.T == "exec" && s.Relay != nil && sh.NeedsMaster(m.Line) {
			nc, err := s.Relay()
			if err != nil {
				c.shMu.Lock()
				err = c.send(Msg{T: "done", Text: fmt.Sprintf("error: configuration unavailable: %v\n", err), Prompt: sh.Prompt(), Banner: sh.Banner()})
				c.shMu.Unlock()
				if err != nil {
					return
				}
				continue
			}
			if nc != nil {
				r, err := s.startRelay(nc, name, class)
				if err != nil {
					c.shMu.Lock()
					err = c.send(Msg{T: "done", Text: fmt.Sprintf("error: configuration unavailable: master: %v\n", err), Prompt: sh.Prompt(), Banner: sh.Banner()})
					c.shMu.Unlock()
					if err != nil {
						return
					}
					continue
				}
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
			reply = Msg{T: "done", Text: rep.Output, NoMore: rep.NoMore, Exit: rep.Exit, Shell: rep.Shell, Prompt: sh.Prompt(), Banner: sh.Banner(), Cfg: sh.InConfig()}
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

// startRelay opens the relayed session on the master.
func (s *Server) startRelay(nc net.Conn, name string, class commit.Class) (*relay, error) {
	b, _ := json.Marshal(RemoteHello{User: name, Class: class.String()})
	nc.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := nc.Write(append(b, '\n')); err != nil {
		nc.Close()
		return nil, err
	}
	r := &relay{nc: nc, enc: json.NewEncoder(nc)}
	// The master's hello (its prompt) is read by pump.
	nc.SetDeadline(time.Time{})
	return r, nil
}

// pump passes the master's messages to the client until the session
// leaves configuration mode or the master is lost.
func (s *Server) pump(c *conn, sh *cli.Shell, r *relay, rlMu *sync.Mutex, rl **relay) {
	sc := bufio.NewScanner(r.nc)
	sc.Buffer(make([]byte, 64<<10), MaxMsg)
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			break
		}
		switch m.T {
		case "hello":
			r.mu.Lock()
			r.prompt, r.banner = m.Prompt, m.Banner
			r.mu.Unlock()
			continue
		case "done", "completions":
			r.mu.Lock()
			r.busy = false
			r.mu.Unlock()
		}
		leaving := (m.T == "done" || m.T == "notify") && (!m.Cfg || m.Exit)
		if leaving && r.end() {
			rlMu.Lock()
			*rl = nil
			rlMu.Unlock()
			if m.Rev != 0 && s.Synced != nil {
				s.Synced(m.Rev)
			}
			m.Rev = 0
			if !m.Exit {
				// Back to this member's operational mode.
				c.shMu.Lock()
				m.Prompt, m.Banner = sh.Prompt(), sh.Banner()
				c.shMu.Unlock()
			}
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
	busy := r.busy
	r.mu.Unlock()
	text := "connection to the master lost; configuration mode ended (the shared candidate is kept)"
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
