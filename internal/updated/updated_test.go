package updated

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mclag/internal/software"
)

// fakeMachine is a machine with two slots, a boot loader that follows the
// boot state like grub.cfg, and a power switch. Each boot starts a new
// Daemon (as systemd does).
type fakeMachine struct {
	t      *testing.T
	dir    string
	keys   []software.PublicKey
	priv   ed25519.PrivateKey
	mu     sync.Mutex
	env    software.Env
	slots  map[string]string // slot -> version written
	active string
	// boots: slot of every boot; reboots requested by the daemon.
	boots   []string
	reboots int
	// rejects: versions whose configuration check fails.
	rejects map[string]bool
	// broken: versions whose kernel does not start (the boot loader
	// passes over them on the next boot).
	broken map[string]bool
	// writeErr fails the slot write.
	writeErr error
}

type fakePlatform struct{ m *fakeMachine }

func (p fakePlatform) Active() string {
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	return p.m.active
}
func (p fakePlatform) Version() string {
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	return p.m.slots[p.m.active]
}
func (p fakePlatform) ReadEnv() (software.Env, error) {
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	e := software.Env{}
	for k, v := range p.m.env {
		e[k] = v
	}
	return e, nil
}
func (p fakePlatform) WriteEnv(e software.Env) error {
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	p.m.env = software.Env{}
	for k, v := range e {
		p.m.env[k] = v
	}
	return nil
}
func (p fakePlatform) WriteSlot(slot string, img io.Reader, size int64, sha string) error {
	if slot == p.m.active {
		p.m.t.Fatal("the active slot was written")
	}
	if p.m.writeErr != nil {
		return p.m.writeErr
	}
	b, err := io.ReadAll(img)
	if err != nil {
		return err
	}
	p.m.mu.Lock()
	p.m.slots[slot] = strings.TrimPrefix(string(b[:bytes.IndexByte(b, 0)]), "image ")
	p.m.mu.Unlock()
	return nil
}
func (p fakePlatform) CheckConfig(slot string, m *software.BundleManifest) (string, error) {
	if p.m.rejects[m.Version] {
		return "", errors.New("E: something new is stricter")
	}
	return "", nil
}
func (p fakePlatform) Reboot() error {
	p.m.mu.Lock()
	p.m.reboots++
	p.m.mu.Unlock()
	return nil
}
func (p fakePlatform) Keys() ([]software.PublicKey, error) { return p.m.keys, nil }

func newMachine(t *testing.T) *fakeMachine {
	priv, pub, _ := software.GenerateKey("t")
	k, _ := software.ParsePrivateKey(priv)
	p, _ := software.ParsePublicKey(pub)
	m := &fakeMachine{t: t, dir: t.TempDir(), priv: k, keys: []software.PublicKey{{ID: software.KeyID(p), Key: p}},
		env:   software.Env{"ORDER": "A B", "A_OK": "1", "B_OK": "1", "A_TRY": "0", "B_TRY": "0", "A_VERSION": "v1", "A_ROOTHASH": "aa"},
		slots: map[string]string{"A": "v1"}, rejects: map[string]bool{}, broken: map[string]bool{}}
	m.boot()
	// One committed revision.
	rev := filepath.Join(m.dir, "switchd", "config", "rev")
	os.MkdirAll(rev, 0o700)
	os.WriteFile(filepath.Join(rev, "000000000001.json"), []byte(`{"seq":1,"config":{"system":{}}}`), 0o600)
	return m
}

// boot runs the boot loader logic of image/grub/grub.cfg; a broken slot
// "panics" and the loop boots again. The daemon of the previous boot is
// stopped.
func (m *fakeMachine) boot() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < 5; i++ {
		e := m.env
		slot := ""
		for _, s := range e.Order() {
			if slot == "" && e[s+"_OK"] != "0" && e[s+"_TRY"] != "1" {
				slot = s
			}
		}
		if slot == "" {
			e["A_TRY"], e["B_TRY"] = "0", "0"
			for _, s := range e.Order() {
				if slot == "" && e[s+"_OK"] != "0" {
					slot = s
				}
			}
		}
		e[slot+"_TRY"] = "1"
		m.active = slot
		m.boots = append(m.boots, slot)
		if !m.broken[m.slots[slot]] {
			return
		}
	}
	m.t.Fatal("no slot boots")
}

// e returns a copy of the boot state.
func (m *fakeMachine) e() software.Env {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := software.Env{}
	for k, v := range m.env {
		c[k] = v
	}
	return c
}

func (m *fakeMachine) rebootCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reboots
}

func (m *fakeMachine) bundle(t *testing.T, v string) string {
	img := filepath.Join(m.dir, "img-"+v)
	raw := append([]byte("image "+v), make([]byte, 3*4096)...)
	os.WriteFile(img, raw, 0o644)
	path := filepath.Join(m.dir, "ceros-"+v+".bundle")
	f, _ := os.Create(path)
	man := software.BundleManifest{Version: v, Arch: "amd64", Compatible: software.Compatible, RootHash: "bb", HashOffset: 2 * 4096}
	if err := software.WriteBundle(f, man, img, m.priv); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

// start runs a daemon for the current boot; it returns a stop function.
func (m *fakeMachine) start(t *testing.T, timeout time.Duration) (*Daemon, func()) {
	d := &Daemon{P: fakePlatform{m}, ConfigDir: filepath.Join(m.dir, "update"), SwitchdDir: filepath.Join(m.dir, "switchd"),
		BackupDir: filepath.Join(m.dir, "backup"), Socket: filepath.Join(m.dir, "s", "sock"), Timeout: timeout,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	for i := 0; i < 100; i++ {
		if _, err := Call(d.Socket, Request{Op: "status"}); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return d, func() { cancel(); <-done }
}

func (m *fakeMachine) waitReboots(t *testing.T, n int) {
	for i := 0; i < 200; i++ {
		m.mu.Lock()
		r := m.reboots
		m.mu.Unlock()
		if r >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no reboot (%d)", n)
}

func TestUpdateHealthy(t *testing.T) {
	m := newMachine(t)
	d, stop := m.start(t, time.Second)
	if _, err := Call(d.Socket, Request{Op: "healthy", Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	rep, err := Call(d.Socket, Request{Op: "install", Bundle: m.bundle(t, "v2"), Standalone: true, ExitMaintenance: true})
	if err != nil || rep.Version != "v2" {
		t.Fatalf("%+v %v", rep, err)
	}
	m.waitReboots(t, 1)
	stop()
	if m.e().Order()[0] != "B" || m.e()["B_VERSION"] != "v2" || m.e()["B_OK"] != "1" || m.e()["B_ROOTHASH"] != "bb" {
		t.Fatalf("boot state %v", m.env)
	}
	m.boot()
	if m.active != "B" || m.e()["B_TRY"] != "1" {
		t.Fatalf("booted %s, %v", m.active, m.env)
	}
	d, stop = m.start(t, time.Second)
	defer stop()
	Call(d.Socket, Request{Op: "started", Version: "v2"})
	rep, err = Call(d.Socket, Request{Op: "healthy", Version: "v2"})
	if err != nil || rep.Update == nil || !rep.Update.ExitMaintenance {
		t.Fatalf("%+v %v", rep, err)
	}
	for i := 0; i < 100 && d.Load().Done != "ok"; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if d.Load().Done != "ok" || m.e()["B_TRY"] != "0" || m.e().Order()[0] != "B" || m.e()["A_OK"] != "1" {
		t.Fatalf("not confirmed: %+v %v", d.Load(), m.env)
	}
	if _, err := os.Stat(filepath.Join(m.dir, "backup", "v1", "rev", "000000000001.json")); err != nil {
		t.Fatal("no configuration backup")
	}
}

func TestUpdateNotHealthyRollsBack(t *testing.T) {
	m := newMachine(t)
	d, stop := m.start(t, 200*time.Millisecond)
	Call(d.Socket, Request{Op: "install", Bundle: m.bundle(t, "v2"), Standalone: true})
	m.waitReboots(t, 1)
	stop()
	m.boot()
	// The new version changes the configuration on reading (not committed).
	os.WriteFile(filepath.Join(m.dir, "switchd", "config", "rev", "000000000001.json"), []byte(`{"seq":1,"config":{"converted":{}}}`), 0o600)
	d, stop = m.start(t, 200*time.Millisecond)
	m.waitReboots(t, 2) // never healthy: the daemon rolls back
	stop()
	if m.e()["B_OK"] != "0" || m.e().Order()[0] != "A" {
		t.Fatalf("boot state %v", m.env)
	}
	m.boot()
	if m.active != "A" {
		t.Fatalf("booted %s", m.active)
	}
	d, stop = m.start(t, time.Second)
	defer stop()
	st := d.Load()
	if !strings.Contains(st.Done, "rolled back: v2 switchd was not healthy") || !strings.Contains(st.Done, "configuration of v1 was put back") {
		t.Fatalf("state %+v", st)
	}
	raw, _ := os.ReadFile(filepath.Join(m.dir, "switchd", "config", "rev", "000000000001.json"))
	if !strings.Contains(string(raw), `"system"`) {
		t.Fatalf("configuration not restored: %s", raw)
	}
	rep, _ := Call(d.Socket, Request{Op: "status"})
	if !strings.Contains(rep.Note, "rolled back") || len(rep.Slots) != 2 || rep.Slots[1].OK {
		t.Fatalf("status %+v", rep)
	}
}

// A new kernel that does not start: the boot loader returns to the old
// slot by itself; the daemon there records it.
func TestUpdateKernelFailsBootLoaderReturns(t *testing.T) {
	m := newMachine(t)
	m.broken["v2"] = true
	d, stop := m.start(t, time.Second)
	Call(d.Socket, Request{Op: "install", Bundle: m.bundle(t, "v2")})
	m.waitReboots(t, 1)
	stop()
	m.boot()
	if strings.Join(m.boots, " ") != "A B A" {
		t.Fatalf("boots %v", m.boots)
	}
	d, stop = m.start(t, time.Second)
	defer stop()
	st := d.Load()
	if !strings.Contains(st.Done, "did not start") || m.e()["B_OK"] != "0" {
		t.Fatalf("state %+v env %v", st, m.env)
	}
	// Not standalone: the configuration is kept (the stack's).
	if strings.Contains(st.Done, "put back") {
		t.Fatalf("restored on a stack member: %s", st.Done)
	}
}

func TestUpdateCrashLoop(t *testing.T) {
	m := newMachine(t)
	d, stop := m.start(t, time.Minute)
	Call(d.Socket, Request{Op: "install", Bundle: m.bundle(t, "v2")})
	m.waitReboots(t, 1)
	stop()
	m.boot()
	d, stop = m.start(t, time.Minute)
	defer stop()
	for i := 0; i <= MaxStarts; i++ {
		Call(d.Socket, Request{Op: "started", Version: "v2"})
	}
	m.waitReboots(t, 2)
	if m.e()["B_OK"] != "0" {
		t.Fatalf("env %v", m.env)
	}
}

func TestUpdateRejectedAndFailures(t *testing.T) {
	m := newMachine(t)
	m.rejects["v2"] = true
	d, stop := m.start(t, time.Second)
	defer stop()
	_, err := Call(d.Socket, Request{Op: "install", Bundle: m.bundle(t, "v2")})
	if err == nil || !strings.Contains(err.Error(), "rejects") {
		t.Fatalf("rejected config installed: %v", err)
	}
	// The written backup slot is not bootable; A stays first.
	if m.e()["B_OK"] != "0" || m.e().Order()[0] != "A" || m.rebootCount() != 0 {
		t.Fatalf("env %v reboots %d", m.env, m.reboots)
	}
	// check reports the same as an error the master understands.
	_, err = Call(d.Socket, Request{Op: "check", Bundle: m.bundle(t, "v2")})
	if err == nil || !strings.HasPrefix(err.Error(), ErrRejected.Error()) {
		t.Fatalf("check: %v", err)
	}
	// no-validate installs anyway.
	rep, err := Call(d.Socket, Request{Op: "install", Bundle: m.bundle(t, "v2"), NoValidate: true})
	if err != nil || !strings.Contains(rep.Text, "no-validate") {
		t.Fatalf("%+v %v", rep, err)
	}
	m.waitReboots(t, 1)
}

func TestUpdateWriteErrorAndSignature(t *testing.T) {
	m := newMachine(t)
	d, stop := m.start(t, time.Second)
	defer stop()
	m.writeErr = errors.New("I/O error")
	if _, err := Call(d.Socket, Request{Op: "install", Bundle: m.bundle(t, "v2")}); err == nil {
		t.Fatal("write error ignored")
	}
	if m.e()["B_OK"] != "0" || m.e().Order()[0] != "A" {
		t.Fatalf("env %v", m.env)
	}
	m.writeErr = nil
	// Signed by another key.
	other := newMachine(t)
	if _, err := Call(d.Socket, Request{Op: "install", Bundle: other.bundle(t, "v3")}); err == nil || !strings.Contains(err.Error(), "does not trust") {
		t.Fatalf("foreign bundle: %v", err)
	}
	// The running version.
	if _, err := Call(d.Socket, Request{Op: "install", Bundle: m.bundle(t, "v1")}); err == nil {
		t.Fatal("the running version installed again")
	}
}

func TestRollbackCommand(t *testing.T) {
	m := newMachine(t)
	d, stop := m.start(t, time.Second)
	if _, err := Call(d.Socket, Request{Op: "rollback"}); err == nil {
		t.Fatal("rollback to an empty slot")
	}
	m.mu.Lock()
	m.env["B_VERSION"], m.env["B_ROOTHASH"] = "v0", "cc"
	m.slots["B"] = "v0"
	m.mu.Unlock()
	rep, err := Call(d.Socket, Request{Op: "rollback"})
	if err != nil || rep.Version != "v0" {
		t.Fatalf("%+v %v", rep, err)
	}
	m.waitReboots(t, 1)
	stop()
	m.boot()
	d, stop = m.start(t, time.Second)
	defer stop()
	Call(d.Socket, Request{Op: "healthy", Version: "v0"})
	for i := 0; i < 100 && d.Load().Done != "ok"; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if m.active != "B" || d.Load().Done != "ok" || m.e()["B_TRY"] != "0" || m.e()["A_OK"] != "1" {
		t.Fatalf("active %s state %+v env %v", m.active, d.Load(), m.env)
	}
}

// A boot without an update is confirmed once switchd is healthy.
func TestPlainBootConfirmed(t *testing.T) {
	m := newMachine(t)
	d, stop := m.start(t, time.Second)
	defer stop()
	if m.e()["A_TRY"] != "1" {
		t.Fatal(m.env)
	}
	Call(d.Socket, Request{Op: "healthy", Version: "v1"})
	for i := 0; i < 100 && m.e()["A_TRY"] != "0"; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if m.e()["A_TRY"] != "0" {
		t.Fatal(m.env)
	}
}
