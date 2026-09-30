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
	lost    chan struct{}
}

func (h *handler) Disconnected(*Client) {
	if h.lost != nil {
		close(h.lost)
	}
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
	path, srv, _ := startServerHost(t, auth, "sw1")
	return path, srv
}

func startServerHost(t *testing.T, auth Authorizer, host string, setup ...func(*Server)) (string, *Server, *commit.Engine) {
	t.Helper()
	dir := t.TempDir()
	st, _ := commit.OpenFileStore(filepath.Join(dir, "state"), 50)
	var srv *Server
	e, err := commit.New(commit.Options{Store: st, Applier: nopApplier{}, Log: quiet,
		Notify: func(ctx context.Context, m string) { srv.Notify(ctx, m) }})
	if err != nil {
		t.Fatal(err)
	}
	e.Start(context.Background())
	srv = &Server{
		Env: func(name string, class commit.Class) cli.Env {
			return cli.Env{Engine: e, User: name, Class: class, Version: "t", Log: quiet, HostName: func() string { return host }}
		},
		Authorize: auth, Log: quiet,
	}
	for _, f := range setup {
		f(srv)
	}
	path := filepath.Join(dir, "cli.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { l.Close(); e.Close() })
	return path, srv, e
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
	if p, _ := c.State(); !strings.HasSuffix(p, "@sw1> ") || c.Class != "super-user" {
		t.Errorf("prompt %q class %q", p, c.Class)
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
	if p, b := c.State(); !strings.HasSuffix(p, "@sw1# ") || b != "[edit]\n" {
		t.Errorf("config prompt %q banner %q", p, b)
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
	srv.Notify(context.Background(), "automatic rollback: test")
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

// Another user's commit and confirmation reach the other sessions at once,
// with their refreshed banner; the user who acted gets no notice.
func TestMultiUserNotices(t *testing.T) {
	path, _ := startServer(t, allow)
	ha, hb := &handler{}, &handler{}
	a, err := Dial(path, ha)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Dial(path, hb)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	wait := func(h *handler, n int) []string {
		t.Helper()
		for i := 0; i < 200; i++ {
			h.mu.Lock()
			notes := append([]string(nil), h.notes...)
			h.mu.Unlock()
			if len(notes) >= n {
				return notes
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("expected %d notices", n)
		return nil
	}
	a.Exec("configure")
	a.Exec("set system host-name x")
	if m, _ := a.Exec("commit confirmed 5"); !strings.Contains(m.Text, "commit complete") {
		t.Fatalf("commit: %q", m.Text)
	}
	notes := wait(hb, 1)
	if !strings.Contains(notes[0], "commit complete (revision 2), must be confirmed within 5 minutes") {
		t.Errorf("notice: %q", notes[0])
	}
	if _, banner := b.State(); !strings.Contains(banner, "commit pending confirmation") {
		t.Errorf("banner not refreshed: %q", banner)
	}
	b.Exec("confirm")
	wait(ha, 1)
	if _, banner := a.State(); strings.Contains(banner, "pending") || banner != "[edit]\n" {
		t.Errorf("a's banner after b confirmed: %q", banner)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(wait(hb, 1)); n != 1 {
		t.Errorf("b was notified of its own confirm")
	}
	if n := len(wait(ha, 1)); n != 1 {
		t.Errorf("a: %v", ha.notes)
	}
	// An empty line refreshes without being a command.
	if m, err := a.Exec(""); err != nil || m.Text != "" || m.Prompt == "" {
		t.Errorf("empty exec: %+v %v", m, err)
	}
}

func TestDisconnected(t *testing.T) {
	path, srv := startServer(t, allow)
	h := &handler{lost: make(chan struct{})}
	c, err := Dial(path, h)
	if err != nil {
		t.Fatal(err)
	}
	srv.CloseSessions()
	select {
	case <-h.lost:
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnected not called")
	}
	if !c.Closed() {
		t.Error("not closed")
	}
	if _, err := c.Exec("show version"); err == nil {
		t.Error("exec on a lost connection succeeded")
	}
	// Close does not report a disconnect.
	h2 := &handler{lost: make(chan struct{})}
	c2, _ := Dial(path, h2)
	c2.Close()
	select {
	case <-h2.lost:
		t.Error("Disconnected after Close")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := Offline("x> ").Exec("show"); !errors.Is(err, ErrOffline) {
		t.Errorf("offline: %v", err)
	}
}

type chanListener struct {
	c    chan net.Conn
	done chan struct{}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.c:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *chanListener) Close() error   { close(l.done); return nil }
func (l *chanListener) Addr() net.Addr { return &net.UnixAddr{} }

// A session on a member that is not master runs on the master (reference
// 1.8); without the master, operational commands run on the member.
func TestRelayToMaster(t *testing.T) {
	_, master, me := startServerHost(t, allow, "sw1")
	l := &chanListener{c: make(chan net.Conn), done: make(chan struct{})}
	go master.ServeRemote(l)
	t.Cleanup(func() { l.Close() })
	var mu sync.Mutex
	var masterSide net.Conn
	var noMaster bool
	var synced uint64
	path, _, _ := startServerHost(t, allow, "sw2", func(member *Server) {
		member.Member = 2
		member.Relay = func() (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			if noMaster {
				return nil, errors.New("no master (the stack has no majority)")
			}
			a, b := net.Pipe()
			masterSide = b
			l.c <- b
			return a, nil
		}
		member.Synced = func(rev uint64) { mu.Lock(); synced = rev; mu.Unlock() }
	})
	h := &handler{files: map[string][]byte{}}
	c, err := Dial(path, h)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if p, _ := c.State(); !strings.HasSuffix(p, "@sw1> ") {
		t.Fatalf("session not on the master from the start: prompt %q", p)
	}
	exec := func(line string) Msg {
		t.Helper()
		m, err := c.Exec(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		return m
	}
	if m := exec("configure"); !strings.Contains(m.Text, "Entering configuration mode") {
		t.Fatalf("configure: %q", m.Text)
	}
	if p, b := c.State(); !strings.HasSuffix(p, "@sw1# ") || b != "[edit]\n" {
		t.Fatalf("relayed prompt %q banner %q", p, b)
	}
	items, err := c.Complete("set sys")
	if err != nil || len(items) == 0 || items[0].Text != "system" {
		t.Fatalf("relayed completion: %v %v", items, err)
	}
	exec("set system host-name relayed")
	if m := exec("commit"); !strings.Contains(m.Text, "commit complete") {
		t.Fatalf("commit: %q", m.Text)
	}
	if me.Active().Root.Leaf("system", "host-name") != "relayed" {
		t.Fatal("commit did not reach the master's engine")
	}
	exec("exit")
	if p, _ := c.State(); !strings.HasSuffix(p, "@sw1> ") {
		t.Fatalf("operational mode left the master: prompt %q", p)
	}
	mu.Lock()
	if synced != me.ActiveSeq() || synced < 2 {
		t.Errorf("member waited for revision %d, the master has %d", synced, me.ActiveSeq())
	}
	mu.Unlock()
	if m := exec("show version"); !strings.Contains(m.Text, "Hostname: sw1") {
		t.Errorf("operational command not on the master: %q", m.Text)
	}

	// The master goes away during configuration mode.
	exec("configure")
	mu.Lock()
	masterSide.Close()
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, _ := c.State()
		if strings.HasSuffix(p, "@sw2> ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still relayed after the master was lost: %q", p)
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.mu.Lock()
	notes := strings.Join(h.notes, "\n")
	h.mu.Unlock()
	if !strings.Contains(notes, "connection to the master lost") {
		t.Errorf("notices: %q", notes)
	}

	// Without a master, configure fails; operational commands run here.
	mu.Lock()
	noMaster = true
	mu.Unlock()
	if m := exec("configure"); !strings.Contains(m.Text, "configuration unavailable: no master") {
		t.Fatalf("configure without master: %q", m.Text)
	}
	if p, b := c.State(); !strings.HasSuffix(p, "@sw2> ") || !strings.Contains(b, "master not reachable") {
		t.Fatalf("prompt %q banner %q", p, b)
	}
	if m := exec("show version"); !strings.Contains(m.Text, "Hostname: sw2") {
		t.Errorf("operational command without the master: %q", m.Text)
	}
	// The master is back: the next command (after the retry interval, or
	// configure at once) runs there again.
	mu.Lock()
	noMaster = false
	mu.Unlock()
	exec("configure")
	if p, _ := c.State(); !strings.HasSuffix(p, "@sw1# ") {
		t.Fatalf("not on the master again: prompt %q", p)
	}
}
