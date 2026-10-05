package webapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
)

// The configuration and command endpoints (reference 5.1 web-management,
// PLAN 18.1) run the CLI itself: a shell per request or per configuration
// session, with the user's class, so the API does exactly what the CLI
// does (commit engine, locks, confirmation, permissions, per-member
// results).

// SetShells gives the server the CLI (f makes a session for a user); the
// configuration and command endpoints answer 503 until then.
func (s *Server) SetShells(f func(User) Shell) {
	s.mu.Lock()
	s.newShell = f
	s.mu.Unlock()
}

// NewShell makes a CLI session (nil: none yet).
func (s *Server) NewShell(u User) Shell {
	s.mu.Lock()
	f := s.newShell
	s.mu.Unlock()
	if f == nil {
		return nil
	}
	u.Class = classOf(u)
	return f(u)
}

// classOf is a user's class; SuperUser decides about super-user (the zero
// Class is super-user: a User made without a class must not become one).
func classOf(u User) commit.Class {
	switch {
	case u.SuperUser:
		return commit.SuperUser
	case u.Class == commit.SuperUser:
		return commit.ReadOnly
	}
	return u.Class
}

// Shell is a CLI session (cli.Shell).
type Shell interface {
	Execute(ctx context.Context, line string, term cli.Terminal) cli.Reply
	Close()
}

// apiTerm answers a command's questions from the request; nothing is read
// interactively.
type apiTerm struct {
	answers []string
	text    string // for load … terminal
	hasText bool
}

var errNoCLI = errors.New("the switch is starting; try again in a moment")

var errNoAnswer = errors.New("the command asks a question: give its answer in \"answers\"")

func (t *apiTerm) Ask(prompt string, _ bool) (string, error) {
	if len(t.answers) == 0 {
		return "", fmt.Errorf("%w (%s)", errNoAnswer, strings.TrimSpace(prompt))
	}
	a := t.answers[0]
	t.answers = t.answers[1:]
	return a, nil
}

func (t *apiTerm) ReadText(string) (string, error) {
	if !t.hasText {
		return "", io.EOF
	}
	t.hasText = false
	return t.text, nil
}

func (t *apiTerm) ReadFile(string) ([]byte, error) {
	return nil, errors.New("files are not available through the API (send the text)")
}

func (t *apiTerm) WriteFile(string, []byte) error {
	return errors.New("files are not available through the API")
}

// Result is one command's output.
type Result struct {
	Command string `json:"command,omitempty"`
	Output  string `json:"output"`
	OK      bool   `json:"ok"`
}

// failed reports whether CLI output contains an error.
func failed(out string) bool {
	for l := range strings.Lines(out) {
		if strings.HasPrefix(strings.TrimSpace(l), "error:") {
			return true
		}
	}
	return false
}

func run(ctx context.Context, sh Shell, t *apiTerm, line string) Result {
	rep := sh.Execute(ctx, line, t)
	return Result{Command: line, Output: rep.Output, OK: !failed(rep.Output)}
}

func decode(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("bad request body: %v", err)
	}
	return nil
}

// CommandsRequest is the body of POST /api/v1/cli and of a session's
// commands.
type CommandsRequest struct {
	Commands []string `json:"commands"`
	Answers  []string `json:"answers,omitempty"`
}

func (s *Server) cliRun(w http.ResponseWriter, r *http.Request, u User) {
	var req CommandsRequest
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Commands) == 0 {
		fail(w, http.StatusBadRequest, errors.New("no commands"))
		return
	}
	sh := s.NewShell(u)
	if sh == nil {
		fail(w, http.StatusServiceUnavailable, errNoCLI)
		return
	}
	defer sh.Close()
	t := &apiTerm{answers: req.Answers}
	out := make([]Result, 0, len(req.Commands))
	for _, c := range req.Commands {
		res := run(r.Context(), sh, t, c)
		if strings.HasPrefix(strings.TrimSpace(c), "configure") {
			res = Result{Command: c, Output: "error: configuration mode is in POST /api/v1/config/sessions\n"}
		}
		out = append(out, res)
	}
	reply(w, http.StatusOK, out)
}

func (s *Server) configGet(w http.ResponseWriter, r *http.Request, u User) {
	line := "show configuration"
	switch f := r.URL.Query().Get("format"); f {
	case "", "json":
		line += " | display json"
	case "set":
		line += " | display set"
	case "text":
	default:
		fail(w, http.StatusBadRequest, fmt.Errorf("unknown format %q (json, set, text)", f))
		return
	}
	s.oneCommand(w, r, u, line, r.URL.Query().Get("format") == "" || r.URL.Query().Get("format") == "json")
}

// oneCommand runs a reading command; asJSON: its output is JSON itself.
func (s *Server) oneCommand(w http.ResponseWriter, r *http.Request, u User, line string, asJSON bool) {
	sh := s.NewShell(u)
	if sh == nil {
		fail(w, http.StatusServiceUnavailable, errNoCLI)
		return
	}
	defer sh.Close()
	res := run(r.Context(), sh, &apiTerm{}, line+" | no-more")
	switch {
	case !res.OK:
		fail(w, http.StatusBadRequest, errors.New(strings.TrimSpace(res.Output)))
	case asJSON:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, res.Output)
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, res.Output)
	}
}

// ---- configuration sessions ----

const (
	sessionIdle    = 30 * time.Minute
	sessionsByUser = 8
)

type session struct {
	id   string
	user string
	sh   Shell
	mu   sync.Mutex // one request at a time
	used time.Time
}

type sessions struct {
	mu sync.Mutex
	m  map[string]*session
}

func (s *Server) newSession(w http.ResponseWriter, r *http.Request, u User) {
	s.sess.mu.Lock()
	if s.sess.m == nil {
		s.sess.m = map[string]*session{}
	}
	s.expireLocked(time.Now())
	n := 0
	for _, x := range s.sess.m {
		if x.user == u.Name {
			n++
		}
	}
	s.sess.mu.Unlock()
	if n >= sessionsByUser {
		fail(w, http.StatusTooManyRequests, fmt.Errorf("%s has %d configuration sessions already; close one", u.Name, n))
		return
	}
	sh := s.NewShell(u)
	if sh == nil {
		fail(w, http.StatusServiceUnavailable, errNoCLI)
		return
	}
	if res := run(r.Context(), sh, &apiTerm{}, "configure private"); !res.OK {
		sh.Close()
		fail(w, http.StatusConflict, errors.New(strings.TrimSpace(res.Output)))
		return
	}
	var b [16]byte
	rand.Read(b[:])
	x := &session{id: hex.EncodeToString(b[:]), user: u.Name, sh: sh, used: time.Now()}
	s.sess.mu.Lock()
	s.sess.m[x.id] = x
	s.sess.mu.Unlock()
	s.Log.Info("web-management: configuration session opened", "facility", "change-log", "user", u.Name, "session", x.id[:8])
	reply(w, http.StatusCreated, map[string]string{"id": x.id})
}

// expireLocked closes idle sessions (their changes are discarded).
func (s *Server) expireLocked(now time.Time) {
	for id, x := range s.sess.m {
		if x.mu.TryLock() {
			if now.Sub(x.used) >= sessionIdle {
				x.sh.Close()
				delete(s.sess.m, id)
				s.Log.Info("web-management: idle configuration session closed", "user", x.user, "session", id[:8])
			}
			x.mu.Unlock()
		}
	}
}

// withSession runs h with the session of the request (its user's only).
func (s *Server) withSession(h func(w http.ResponseWriter, r *http.Request, x *session)) func(http.ResponseWriter, *http.Request, User) {
	return func(w http.ResponseWriter, r *http.Request, u User) {
		id := r.PathValue("id")
		s.sess.mu.Lock()
		s.expireLocked(time.Now())
		x := s.sess.m[id]
		s.sess.mu.Unlock()
		if x == nil || x.user != u.Name {
			fail(w, http.StatusNotFound, errors.New("no such configuration session (closed, expired or another user's)"))
			return
		}
		x.mu.Lock()
		defer x.mu.Unlock()
		x.used = time.Now()
		h(w, r, x)
	}
}

func (s *Server) closeSession(x *session) {
	x.sh.Close()
	s.sess.mu.Lock()
	delete(s.sess.m, x.id)
	s.sess.mu.Unlock()
}

// LoadRequest is the body of a session's load.
type LoadRequest struct {
	Mode string `json:"mode"`
	Text string `json:"text"`
}

func (s *Server) sessionLoad(w http.ResponseWriter, r *http.Request, x *session) {
	var req LoadRequest
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	switch req.Mode {
	case "merge", "replace", "override", "set":
	default:
		fail(w, http.StatusBadRequest, fmt.Errorf("mode must be merge, replace, override or set, not %q", req.Mode))
		return
	}
	res := run(r.Context(), x.sh, &apiTerm{text: req.Text, hasText: true}, "load "+req.Mode+" terminal")
	reply(w, http.StatusOK, res)
}

func (s *Server) sessionCommands(w http.ResponseWriter, r *http.Request, x *session) {
	var req CommandsRequest
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	t := &apiTerm{answers: req.Answers}
	var out []Result
	for _, c := range req.Commands {
		f := strings.Fields(c)
		if len(f) > 0 && (f[0] == "exit" || f[0] == "quit" || f[0] == "commit") {
			out = append(out, Result{Command: c, Output: "error: use the session's commit or DELETE endpoints\n"})
			continue
		}
		out = append(out, run(r.Context(), x.sh, t, c))
	}
	reply(w, http.StatusOK, out)
}

func (s *Server) sessionCompare(w http.ResponseWriter, r *http.Request, x *session) {
	reply(w, http.StatusOK, run(r.Context(), x.sh, &apiTerm{}, "show | compare | no-more"))
}

func (s *Server) sessionCheck(w http.ResponseWriter, r *http.Request, x *session) {
	reply(w, http.StatusOK, run(r.Context(), x.sh, &apiTerm{}, "commit check"))
}

// CommitRequest is the body of a session's commit.
type CommitRequest struct {
	Comment   string `json:"comment,omitempty"`
	Confirmed int    `json:"confirmed,omitempty"` // minutes (0: not confirmed)
}

func (s *Server) sessionCommit(w http.ResponseWriter, r *http.Request, x *session) {
	var req CommitRequest
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	line := "commit"
	if req.Confirmed > 0 {
		line += " confirmed " + strconv.Itoa(req.Confirmed)
	}
	if req.Comment != "" {
		line += " comment " + strconv.Quote(req.Comment)
	}
	res := run(r.Context(), x.sh, &apiTerm{}, line)
	if res.OK {
		s.closeSession(x)
	}
	reply(w, http.StatusOK, res)
}

func (s *Server) sessionDelete(w http.ResponseWriter, r *http.Request, x *session) {
	s.closeSession(x)
	reply(w, http.StatusOK, map[string]string{"status": "closed; its changes are discarded"})
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request, u User) {
	sh := s.NewShell(u)
	if sh == nil {
		fail(w, http.StatusServiceUnavailable, errNoCLI)
		return
	}
	defer sh.Close()
	res := run(r.Context(), sh, &apiTerm{}, "confirm")
	reply(w, http.StatusOK, res)
}

// classOK: the user's class is at least need.
func classOK(u User, need commit.Class) bool { return classOf(u) <= need }
