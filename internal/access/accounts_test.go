package access

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mclag/internal/model"
)

type fakeSys struct {
	accounts map[string]Entry
	keys     map[string][]string
	calls    []string
	onAdd    func(Entry) // before the account exists
	failAdd  bool
}

func newFakeSys() *fakeSys {
	return &fakeSys{accounts: map[string]Entry{
		"root": {Name: "root", UID: 0, Home: "/root", Shell: "/bin/bash", Hash: "$6$x"},
		"user": {Name: "user", UID: 1000, Home: "/home/user", Shell: "/bin/bash", Hash: "$6$y"},
	}, keys: map[string][]string{}}
}

func (f *fakeSys) Lookup(n string) (Entry, bool) { e, ok := f.accounts[n]; return e, ok }
func (f *fakeSys) UIDTaken(uid int) bool {
	for _, e := range f.accounts {
		if e.UID == uid {
			return true
		}
	}
	return false
}
func (f *fakeSys) Add(e Entry) error {
	f.calls = append(f.calls, "add "+e.Name)
	if f.onAdd != nil {
		f.onAdd(e)
	}
	if f.failAdd {
		return errors.New("useradd failed")
	}
	f.accounts[e.Name] = e
	return nil
}
func (f *fakeSys) Modify(e Entry) error {
	f.calls = append(f.calls, "mod "+e.Name)
	f.accounts[e.Name] = e
	return nil
}
func (f *fakeSys) Delete(n string) error {
	f.calls = append(f.calls, "del "+n)
	delete(f.accounts, n)
	return nil
}
func (f *fakeSys) SetPassword(e Entry) error {
	f.calls = append(f.calls, "passwd "+e.Name)
	a := f.accounts[e.Name]
	a.Hash = e.Hash
	f.accounts[e.Name] = a
	return nil
}
func (f *fakeSys) WriteKeys(e Entry, k []string) error {
	if len(k) == 0 {
		delete(f.keys, e.Name)
	} else {
		f.keys[e.Name] = k
	}
	return nil
}

func cfgWith(users ...*model.User) *model.Config {
	c := &model.Config{}
	c.System.Users = map[string]*model.User{}
	for _, u := range users {
		c.System.Users[u.Name] = u
	}
	return c
}

func TestSyncLifecycle(t *testing.T) {
	sys := newFakeSys()
	m := &Manager{Sys: sys, StateFile: filepath.Join(t.TempDir(), "accounts.json"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	alice := &model.User{Name: "alice", Class: "super-user", FullName: "Alice", PasswordHash: "$6$a$b", SSHKeys: []string{"ssh-ed25519 AAAA alice"}}
	bob := &model.User{Name: "bob", Class: "operator"}
	if err := m.Sync(cfgWith(alice, bob)); err != nil {
		t.Fatal(err)
	}
	a := sys.accounts["alice"]
	if a.UID != 2000 || a.Shell != Shell || a.Hash != "$6$a$b" || a.FullName != "Alice" || len(sys.keys["alice"]) != 1 {
		t.Errorf("alice: %+v keys %v", a, sys.keys["alice"])
	}
	if b := sys.accounts["bob"]; b.UID != 2001 || b.Hash != "!" {
		t.Errorf("bob (no password: locked): %+v", b)
	}
	// No changes: no OS calls.
	sys.calls = nil
	if err := m.Sync(cfgWith(alice, bob)); err != nil || len(sys.calls) != 0 {
		t.Errorf("idempotence: %v %v", err, sys.calls)
	}
	// Change and removal; UIDs stay stable.
	alice2 := *alice
	alice2.FullName = "Alice A."
	alice2.SSHKeys = nil
	if err := m.Sync(cfgWith(&alice2)); err != nil {
		t.Fatal(err)
	}
	if sys.accounts["alice"].FullName != "Alice A." || sys.keys["alice"] != nil {
		t.Error("modification not applied")
	}
	if _, ok := sys.accounts["bob"]; ok {
		t.Error("removed user still exists")
	}
	carol := &model.User{Name: "carol"}
	m.Sync(cfgWith(&alice2, carol))
	if sys.accounts["carol"].UID != 2001 || sys.accounts["alice"].UID != 2000 {
		t.Errorf("uids: alice %d carol %d", sys.accounts["alice"].UID, sys.accounts["carol"].UID)
	}
	// Unmanaged accounts are never touched.
	if _, ok := sys.accounts["user"]; !ok {
		t.Fatal("unmanaged account deleted")
	}
	err := m.Sync(cfgWith(&model.User{Name: "user", Class: "super-user"}))
	if err == nil || !strings.Contains(err.Error(), "not managed by switchd") || sys.accounts["user"].Shell != "/bin/bash" {
		t.Errorf("takeover of an existing account: %v %+v", err, sys.accounts["user"])
	}
}

func TestCheck(t *testing.T) {
	sys := newFakeSys()
	m := &Manager{Sys: sys, StateFile: filepath.Join(t.TempDir(), "a.json"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	is := m.Check(cfgWith(&model.User{Name: "user"}, &model.User{Name: "root"}, &model.User{Name: "dave", UID: 1000}, &model.User{Name: "ok"}))
	s := is.String()
	for _, want := range []string{"OS account named user exists", "use system root-authentication", "uid 1000 is already used"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "user ok") {
		t.Errorf("false positive:\n%s", s)
	}
}

// switchd may stop right after creating an account: the account must
// already be recorded as its own, or it would be refused afterwards.
func TestAccountRecordedBeforeCreation(t *testing.T) {
	sys := newFakeSys()
	state := filepath.Join(t.TempDir(), "accounts.json")
	m := &Manager{Sys: sys, StateFile: state, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sys.onAdd = func(e Entry) {
		raw, _ := os.ReadFile(state)
		if !strings.Contains(string(raw), `"`+e.Name+`"`) {
			t.Errorf("%s created before it was recorded: %s", e.Name, raw)
		}
	}
	if err := m.Sync(cfgWith(&model.User{Name: "dave"})); err != nil {
		t.Fatal(err)
	}
	// A failed creation is not left recorded.
	sys.onAdd, sys.failAdd = nil, true
	if err := m.Sync(cfgWith(&model.User{Name: "dave"}, &model.User{Name: "erin"})); err == nil {
		t.Fatal("failed creation not reported")
	}
	if raw, _ := os.ReadFile(state); strings.Contains(string(raw), "erin") {
		t.Errorf("failed account recorded: %s", raw)
	}
}

func TestRootAuthentication(t *testing.T) {
	sys := newFakeSys()
	m := &Manager{Sys: sys, StateFile: filepath.Join(t.TempDir(), "accounts.json"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c := cfgWith()
	c.System.Root = &model.User{Name: "root", PasswordHash: "$6$new", SSHKeys: []string{"ssh-ed25519 AAAA r"}}
	if err := m.Sync(c); err != nil {
		t.Fatal(err)
	}
	if sys.accounts["root"].Hash != "$6$new" || len(sys.keys["root"]) != 1 || sys.accounts["root"].Shell != "/bin/bash" {
		t.Fatalf("root %+v keys %v", sys.accounts["root"], sys.keys["root"])
	}
	// Removed: no password, no keys (console login only).
	if err := m.Sync(cfgWith()); err != nil {
		t.Fatal(err)
	}
	if sys.accounts["root"].Hash != "" || sys.keys["root"] != nil {
		t.Fatalf("root %+v keys %v", sys.accounts["root"], sys.keys["root"])
	}
}
