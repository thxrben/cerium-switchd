package access

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mclag/internal/model"
)

// Shell is the login shell of managed accounts.
const Shell = "/usr/local/bin/swcli"

// Entry is an OS account as far as switchd cares.
type Entry struct {
	Name     string
	UID      int
	FullName string
	Home     string
	Shell    string
	Hash     string // crypt hash, "!" = locked
}

// System is the OS account database.
type System interface {
	Lookup(name string) (Entry, bool)
	UIDTaken(uid int) bool
	Add(e Entry) error
	Modify(e Entry) error // all fields of e, identified by name
	Delete(name string) error
	// WriteKeys installs the SSH keys (none: remove the file).
	WriteKeys(e Entry, keys []string) error
}

// Manager keeps the configured users and the OS accounts in sync. It only
// ever touches accounts it created (recorded in StateFile).
type Manager struct {
	Sys       System
	StateFile string
	Log       *slog.Logger
}

type state struct {
	Users map[string]int `json:"users"` // managed name -> uid
}

func (m *Manager) load() state {
	st := state{Users: map[string]int{}}
	if raw, err := os.ReadFile(m.StateFile); err == nil {
		_ = json.Unmarshal(raw, &st)
		if st.Users == nil {
			st.Users = map[string]int{}
		}
	}
	return st
}

func (m *Manager) save(st state) error {
	raw, _ := json.Marshal(st)
	tmp := m.StateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.StateFile)
}

// Check reports configuration problems that depend on the OS: accounts
// with the same name that switchd does not manage, and root.
func (m *Manager) Check(cfg *model.Config) model.Issues {
	st := m.load()
	var is model.Issues
	for _, name := range sortedUsers(cfg) {
		path := "system login user " + name
		if name == "root" {
			is = append(is, model.Issue{Severity: model.Error, Path: path, Msg: "root is not managed by switchd; its password is maintained by the OS"})
			continue
		}
		if _, managed := st.Users[name]; !managed {
			if _, exists := m.Sys.Lookup(name); exists {
				is = append(is, model.Issue{Severity: model.Error, Path: path,
					Msg: fmt.Sprintf("an OS account named %s exists and is not managed by switchd", name)})
			}
		}
		if u := cfg.System.Users[name]; u.UID != 0 {
			if owner, taken := m.uidOwner(u.UID, st); taken && owner != name {
				is = append(is, model.Issue{Severity: model.Error, Path: path + " uid", Msg: fmt.Sprintf("uid %d is already used by %s", u.UID, owner)})
			}
		}
	}
	return is
}

func (m *Manager) uidOwner(uid int, st state) (string, bool) {
	for n, u := range st.Users {
		if u == uid {
			return n, true
		}
	}
	if m.Sys.UIDTaken(uid) {
		return "another account", true
	}
	return "", false
}

func sortedUsers(cfg *model.Config) []string {
	names := make([]string, 0, len(cfg.System.Users))
	for n := range cfg.System.Users {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Sync makes the managed accounts match cfg. Errors for single accounts
// are collected; the others are still processed.
func (m *Manager) Sync(cfg *model.Config) error {
	st := m.load()
	var errs []error
	want := map[string]bool{}
	for _, name := range sortedUsers(cfg) {
		if name == "root" {
			continue
		}
		u := cfg.System.Users[name]
		want[name] = true
		cur, exists := m.Sys.Lookup(name)
		_, managed := st.Users[name]
		if exists && !managed {
			errs = append(errs, fmt.Errorf("%s: an OS account with this name exists and is not managed by switchd", name))
			continue
		}
		uid := u.UID
		if uid == 0 {
			uid = st.Users[name]
		}
		if uid == 0 {
			uid = m.nextUID(st)
		}
		hash := u.PasswordHash
		if hash == "" {
			hash = "!"
		}
		e := Entry{Name: name, UID: uid, FullName: u.FullName, Home: "/home/" + name, Shell: Shell, Hash: hash}
		if exists {
			e.Home = cur.Home
		}
		switch {
		case !exists:
			// Recorded before the account is created: if switchd stops in
			// between, the account is still known as its own afterwards.
			st.Users[name] = uid
			if err := m.save(st); err != nil {
				delete(st.Users, name)
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			if err := m.Sys.Add(e); err != nil {
				if _, created := m.Sys.Lookup(name); !created {
					delete(st.Users, name)
				}
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			m.Log.Info("account created", "facility", "authorization", "user", name, "uid", uid)
		case cur != e:
			if err := m.Sys.Modify(e); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			m.Log.Info("account updated", "facility", "authorization", "user", name)
		}
		st.Users[name] = uid
		if err := m.Sys.WriteKeys(e, u.SSHKeys); err != nil {
			errs = append(errs, fmt.Errorf("%s: ssh keys: %w", name, err))
		}
	}
	for name := range st.Users {
		if want[name] {
			continue
		}
		if cur, ok := m.Sys.Lookup(name); ok {
			_ = m.Sys.WriteKeys(cur, nil)
			if err := m.Sys.Delete(name); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
		}
		m.Log.Info("account removed (home directory kept)", "facility", "authorization", "user", name)
		delete(st.Users, name)
	}
	if err := m.save(st); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *Manager) nextUID(st state) int {
	used := map[int]bool{}
	for _, u := range st.Users {
		used[u] = true
	}
	for uid := 2000; ; uid++ {
		if !used[uid] && !m.Sys.UIDTaken(uid) {
			return uid
		}
	}
}

// ---- the real system ----

// OS is the System of this host: /etc/passwd and /etc/shadow are read
// directly, changes go through useradd/usermod/userdel.
type OS struct {
	Root string // "" = /
}

func (o *OS) path(p string) string { return filepath.Join(o.Root, p) }

func readColonFile(path string) map[string][]string {
	out := map[string][]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Split(sc.Text(), ":")
		if len(fs) > 1 && fs[0] != "" {
			out[fs[0]] = fs
		}
	}
	return out
}

func (o *OS) Lookup(name string) (Entry, bool) {
	pw := readColonFile(o.path("/etc/passwd"))[name]
	if len(pw) < 7 {
		return Entry{}, false
	}
	uid, _ := strconv.Atoi(pw[2])
	e := Entry{Name: name, UID: uid, FullName: pw[4], Home: pw[5], Shell: pw[6]}
	if sh := readColonFile(o.path("/etc/shadow"))[name]; len(sh) > 1 {
		e.Hash = sh[1]
	}
	return e, true
}

func (o *OS) UIDTaken(uid int) bool {
	for _, pw := range readColonFile(o.path("/etc/passwd")) {
		if len(pw) > 2 && pw[2] == strconv.Itoa(uid) {
			return true
		}
	}
	return false
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (o *OS) Add(e Entry) error {
	if err := run("groupadd", "--force", "--system", CLIGroup); err != nil {
		return err
	}
	if err := run("useradd", "--create-home", "--user-group", "--groups", CLIGroup, "--uid", strconv.Itoa(e.UID),
		"--shell", e.Shell, "--comment", e.FullName, "--password", e.Hash, e.Name); err != nil {
		return err
	}
	// A home directory kept from an earlier account of this name belongs
	// to root (see Delete); it becomes the new account's.
	home := o.path(e.Home)
	if st, err := os.Stat(home); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != e.UID {
			return run("chown", "-R", "--no-dereference", e.Name+":", home)
		}
	}
	return nil
}

func (o *OS) Modify(e Entry) error {
	if err := run("groupadd", "--force", "--system", CLIGroup); err != nil {
		return err
	}
	return run("usermod", "--append", "--groups", CLIGroup, "--uid", strconv.Itoa(e.UID), "--shell", e.Shell,
		"--comment", e.FullName, "--password", e.Hash, e.Name)
}

// Delete ends the user's sessions (a removed user must not stay logged in)
// and deletes the account. The home directory is kept, owned by root and
// closed, so that a later account that gets the same uid cannot read it.
func (o *OS) Delete(name string) error {
	e, _ := o.Lookup(name)
	_ = run("loginctl", "terminate-user", name)
	var err error
	for i := 0; i < 10; i++ {
		if err = run("userdel", name); err == nil || !strings.Contains(err.Error(), "currently used") {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil || e.Home == "" || e.Home == "/" {
		return err
	}
	home := o.path(e.Home)
	if _, serr := os.Stat(home); serr != nil {
		return nil
	}
	if err := run("chown", "-R", "--no-dereference", "root:root", home); err != nil {
		return err
	}
	return os.Chmod(home, 0o700)
}

// WriteKeys writes ~/.ssh/authorized_keys owned by root (0644 in a root
// owned 0755 directory), so the user cannot change their own keys.
func (o *OS) WriteKeys(e Entry, keys []string) error {
	dir := o.path(filepath.Join(e.Home, ".ssh"))
	file := filepath.Join(dir, "authorized_keys")
	if len(keys) == 0 {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chown(dir, 0, 0); err != nil && o.Root == "" {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	content := strings.Join(slices.Clone(keys), "\n") + "\n"
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
