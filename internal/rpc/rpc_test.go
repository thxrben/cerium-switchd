package rpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mclag/internal/cli"
	"mclag/internal/commit"
	"mclag/internal/config"
)

type nopApplier struct{}

func (nopApplier) Apply(context.Context, *config.Tree, *config.Tree) []commit.MemberResult {
	return []commit.MemberResult{{Member: "member1"}}
}

type handler struct {
	mu      sync.Mutex
	answers []string
	text    string
	files   map[string][]byte
	notes   []string
}

func (h *handler) Ask(string, bool) (string, error) {
	if len(h.answers) == 0 {
		return "", io.EOF
	}
	a := h.answers[0]
	h.answers = h.answers[1:]
	return a, nil
}
func (h *handler) ReadText(string) (string, error) { return h.text, nil }
func (h *handler) ReadFile(n string) ([]byte, error) {
	if d, ok := h.files[n]; ok {
		return d, nil
	}
	return nil, errors.New(n + ": no such file")
}
func (h *handler) WriteFile(n string, d []byte) error { h.files[n] = d; return nil }
func (h *handler) Notify(t string) {
	h.mu.Lock()
	h.notes = append(h.notes, t)
	h.mu.Unlock()
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func startServer(t *testing.T, auth Authorizer) (string, *Server) {
	t.Helper()
	dir := t.TempDir()
	st, _ := commit.OpenFileStore(filepath.Join(dir, "state"), 50)
	e, err := commit.New(commit.Options{Store: st, Applier: nopApplier{}, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	e.Start(context.Background())
	srv := &Server{
		Env: func(name string, class commit.Class) cli.Env {
			return cli.Env{Engine: e, User: name, Class: class, Version: "t", Log: quiet, HostName: func() string { return "sw1" }}
		},
		Authorize: auth, Log: quiet,
	}
	path := filepath.Join(dir, "cli.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { l.Close(); e.Close() })
	return path, srv
}

func allow(uid int, name string) (commit.Class, error) {
	if uid != os.Getuid() {
		return 0, errors.New("unexpected uid")
	}
	return commit.SuperUser, nil
}

func TestSession(t *testing.T) {
	path, srv := startServer(t, allow)
	h := &handler{files: map[string][]byte{"in.conf": []byte("set vlans v vlan-id 5\n")}}
	c, err := Dial(path, h)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !strings.HasSuffix(c.Prompt, "@sw1> ") {
		t.Errorf("prompt %q", c.Prompt)
	}
	exec := func(line string) Msg {
		t.Helper()
		m, err := c.Exec(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		return m
	}
	exec("configure")
	if !strings.HasSuffix(c.Prompt, "@sw1# ") || c.Banner != "[edit]\n" {
		t.Errorf("config prompt %q banner %q", c.Prompt, c.Banner)
	}
	exec("load merge in.conf")
	h.text = "system { host-name viaterm; }"
	exec("load merge terminal")
	if m := exec("show"); !strings.Contains(m.Text, "vlan-id 5;") || !strings.Contains(m.Text, "host-name viaterm;") {
		t.Errorf("show:\n%s", m.Text)
	}
	exec("save out.conf")
	if !strings.Contains(string(h.files["out.conf"]), "vlan-id 5;") {
		t.Errorf("saved %q", h.files["out.conf"])
	}
	h.answers = []string{"yes"}
	exec("edit vlans")
	exec("delete")
	exec("top")
	if m := exec("show"); strings.Contains(m.Text, "vlan-id") {
		t.Error("delete with confirmation did not run")
	}
	if m := exec("load merge nope"); !strings.Contains(m.Text, "no such file") {
		t.Errorf("missing file: %q", m.Text)
	}
	items, err := c.Complete("set sys")
	if err != nil || len(items) != 1 || items[0].Text != "system" {
		t.Errorf("complete: %v %v", items, err)
	}
	help, err := c.Help("set system ")
	if err != nil || !strings.Contains(help, "host-name") {
		t.Errorf("help: %q %v", help, err)
	}
	srv.Notify("automatic rollback: test")
	exec("show") // round trip so the notification has arrived
	h.mu.Lock()
	if len(h.notes) != 1 {
		t.Errorf("notifications: %v", h.notes)
	}
	h.mu.Unlock()
	if m := exec("exit configuration-mode"); m.Exit {
		t.Error("leaving configuration mode ended the session")
	}
	if m := exec("exit"); !m.Exit {
		t.Error("exit did not end the session")
	}
}

func TestRejected(t *testing.T) {
	path, _ := startServer(t, func(int, string) (commit.Class, error) {
		return 0, errors.New("user nobody is not configured")
	})
	_, err := Dial(path, &handler{})
	if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err = %v", err)
	}
}

func TestDisconnectDuringQuestion(t *testing.T) {
	path, _ := startServer(t, allow)
	blocked := make(chan struct{})
	h := &blockingHandler{handler: handler{files: map[string][]byte{}}, blocked: blocked}
	c, err := Dial(path, h)
	if err != nil {
		t.Fatal(err)
	}
	c.Exec("configure")
	c.Exec("edit vlans")
	go c.Exec("delete") // asks; the handler never answers
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("question not asked")
	}
	c.Close() // the server must clean up the session
	c2, err := Dial(path, &handler{files: map[string][]byte{}})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if m, _ := c2.Exec("configure exclusive"); strings.Contains(m.Text, "error") {
		t.Errorf("stale session left behind: %s", m.Text)
	}
}

type blockingHandler struct {
	handler
	blocked chan struct{}
}

func (h *blockingHandler) Ask(string, bool) (string, error) {
	close(h.blocked)
	select {}
}
