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
	c       *net.UnixConn
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

func (s *Server) handle(uc *net.UnixConn) {
	defer uc.Close()
	c := &conn{c: uc, enc: json.NewEncoder(uc), replies: make(chan Msg, 1), done: make(chan struct{}), notes: make(chan string, 32)}
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
	go func() {
		for {
			select {
			case text := <-c.notes:
				c.shMu.Lock()
				_ = c.send(Msg{T: "notify", Text: text, Prompt: sh.Prompt(), Banner: sh.Banner()})
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
		sc := bufio.NewScanner(uc)
		sc.Buffer(make([]byte, 64<<10), MaxMsg)
		for sc.Scan() {
			var m Msg
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				s.Log.Warn("cli: bad message", "user", name, "err", err)
				return
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
			reply = Msg{T: "done", Text: rep.Output, NoMore: rep.NoMore, Exit: rep.Exit, Shell: rep.Shell, Prompt: sh.Prompt(), Banner: sh.Banner()}
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

func items(cs []cli.Completion) []Item {
	out := make([]Item, len(cs))
	for i, c := range cs {
		out[i] = Item{Text: c.Text, Help: c.Help, Placeholder: c.Placeholder}
	}
	return out
}
