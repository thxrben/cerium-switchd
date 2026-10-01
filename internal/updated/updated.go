// Package updated is the update daemon, switchd-update (reference 3.6,
// docs/os-image.md §4): it writes a verified bundle into the backup slot,
// makes it the slot to boot and reboots; after the reboot it watches the
// new switchd and confirms the slot, or returns to the old slot when
// switchd does not become healthy. It runs as its own systemd unit, so a
// switchd that fails cannot stop its own rollback. switchd talks to it over
// a unix socket (one JSON request and one reply per connection).
package updated

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"mclag/internal/software"
)

// DefaultSocket is where the daemon listens.
const DefaultSocket = "/run/switchd-update/sock"

// HealthTimeout: a new version that is not healthy this long after its
// boot is replaced by the previous one.
const HealthTimeout = 5 * time.Minute

// ErrRejected starts the error of a check whose configuration the new
// version rejects.
var ErrRejected = errors.New("the new version rejects the active configuration")

// MaxStarts: a new switchd that starts this often without becoming healthy
// is given up (rollback before the timeout).
const MaxStarts = 3

// Request is what switchd sends.
type Request struct {
	// install, rollback, check, healthy, started, maintenance-done, status
	Op string `json:"op"`
	// install: the bundle, received and verified by switchd (verified
	// again here).
	Bundle string `json:"bundle,omitempty"`
	// healthy, started: the version switchd runs.
	Version string `json:"version,omitempty"`
	// ExitMaintenance: the update drained the member; switchd leaves
	// maintenance mode once the new version is healthy.
	ExitMaintenance bool `json:"exit_maintenance,omitempty"`
	// Standalone: the switch is not a member of a virtual chassis (its
	// configuration backup may be put back after a rollback).
	Standalone bool `json:"standalone,omitempty"`
	// NoValidate: a configuration the new version rejects is only a
	// warning.
	NoValidate bool `json:"no_validate,omitempty"`
}

// Reply is the daemon's answer.
type Reply struct {
	Err     string              `json:"err,omitempty"`
	State   string              `json:"state,omitempty"`
	Version string              `json:"version,omitempty"` // install/rollback: the version booted next
	Note    string              `json:"note,omitempty"`    // the result of the last update
	Text    string              `json:"text,omitempty"`    // install: what the checks said
	Slots   []software.SlotInfo `json:"slots,omitempty"`
	Active  string              `json:"active,omitempty"`
	Update  *State              `json:"update,omitempty"`
}

// State is the update in progress, kept on the configuration partition
// (/config/update/state.json).
type State struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	FromSlot string    `json:"from_slot"`
	Slot     string    `json:"slot"` // the slot written
	Since    time.Time `json:"since"`
	// Revision: the newest configuration revision when the update started.
	Revision   string `json:"revision,omitempty"`
	Standalone bool   `json:"standalone,omitempty"`
	// ExitMaintenance: see Request.
	ExitMaintenance bool `json:"exit_maintenance,omitempty"`
	// Booted: the new slot started (its daemon saw the update).
	Booted bool `json:"booted,omitempty"`
	// MaintenanceDone: switchd left the maintenance mode the update
	// entered (ExitMaintenance is then not acted upon again).
	MaintenanceDone bool `json:"maintenance_done,omitempty"`
	// Starts counts the new switchd's starts.
	Starts int `json:"starts,omitempty"`
	// Done: "" in progress, "ok", or "rolled back: <reason>".
	Done string `json:"done,omitempty"`
	// Recorded: the old slot recorded the rollback (it is final).
	Recorded bool `json:"recorded,omitempty"`
	// Rollback: a rollback (no image written).
	Rollback bool `json:"rollback,omitempty"`
}

// Platform is what the daemon needs from the machine; System implements
// it, tests use a fake.
type Platform interface {
	// Active is the running slot.
	Active() string
	// Version is the version of the running image.
	Version() string
	ReadEnv() (software.Env, error)
	WriteEnv(software.Env) error
	// WriteSlot writes and verifies the image.
	WriteSlot(slot string, img io.Reader, size int64, sha string) error
	// CheckConfig runs the configuration check of the image in slot on the
	// active configuration; warnings are returned as text, a rejection as
	// an error.
	CheckConfig(slot string, m *software.BundleManifest) (string, error)
	// Reboot reboots the machine (switchd drains first, as for every
	// reboot).
	Reboot() error
	// Keys are the trusted signing keys.
	Keys() ([]software.PublicKey, error)
}

// Daemon is the update daemon of one member.
type Daemon struct {
	P Platform
	// ConfigDir is the configuration partition's update directory
	// (/config/update); SwitchdDir the switchd state (/config/switchd) and
	// BackupDir where its configurations are kept (/config/backup).
	ConfigDir, SwitchdDir, BackupDir string
	Socket                           string
	Log                              *slog.Logger
	Timeout                          time.Duration // HealthTimeout if 0
	// Delay before a reboot (switchd answers its caller first).
	RebootDelay time.Duration

	mu      sync.Mutex
	state   string
	busy    bool
	healthy chan struct{}
}

func (d *Daemon) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return HealthTimeout
}

func (d *Daemon) setState(s string) {
	d.mu.Lock()
	d.state = s
	d.mu.Unlock()
	d.Log.Info("switchd-update: " + s)
}

func (d *Daemon) statePath() string { return filepath.Join(d.ConfigDir, "state.json") }

// Load returns the recorded update (nil: none).
func (d *Daemon) Load() *State {
	raw, err := os.ReadFile(d.statePath())
	if err != nil {
		return nil
	}
	var st State
	if json.Unmarshal(raw, &st) != nil {
		return nil
	}
	return &st
}

func (d *Daemon) save(st *State) error {
	if err := os.MkdirAll(d.ConfigDir, 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	return writeSync(d.statePath(), raw)
}

// writeSync writes a file atomically and durably.
func writeSync(path string, raw []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

// Run serves until ctx ends. At start it looks at the recorded update: a
// new slot that just booted is watched; an old slot that came back after
// a failed update records the failure.
func (d *Daemon) Run(ctx context.Context) error {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	d.state = "idle"
	d.boot(ctx)
	if err := os.MkdirAll(filepath.Dir(d.Socket), 0o700); err != nil {
		return err
	}
	os.Remove(d.Socket)
	l, err := net.Listen("unix", d.Socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(d.Socket, 0o600); err != nil { // root (switchd) only
		l.Close()
		return err
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.serve(ctx, c)
	}
}

// boot handles the recorded update after a boot.
func (d *Daemon) boot(ctx context.Context) {
	st := d.Load()
	active := d.P.Active()
	if st == nil || st.Done == "ok" || st.Recorded || (st.Done != "" && active != st.FromSlot) {
		// No update: this boot is confirmed once switchd is healthy (a
		// slot that stops starting is skipped by the boot loader).
		d.healthy = make(chan struct{})
		go d.watchPlain(ctx)
		return
	}
	switch active {
	case st.Slot:
		st.Booted = true
		if err := d.save(st); err != nil {
			d.Log.Error("switchd-update: recording the boot", "err", err)
		}
		d.Log.Warn("switchd-update: booted "+st.To+" (slot "+st.Slot+"); waiting for switchd", "facility", "change-log")
		// The full time from this boot (the daemon may also have been
		// restarted: the boot loader then still guards the slot).
		d.busy = true
		d.healthy = make(chan struct{})
		go d.watchNew(ctx, st, d.timeout())
	case st.FromSlot:
		// The old slot runs again: the new one failed (the boot loader or
		// our own rollback brought us back).
		why := "the new system did not start (the boot loader returned to slot " + active + ")"
		if !st.Booted {
			why = "the new system did not start (kernel or boot failure; the boot loader returned to slot " + active + ")"
		}
		d.failed(st, why)
		d.healthy = make(chan struct{})
		go d.watchPlain(ctx)
	}
}

// failed records a failed update on the old slot: the new slot is marked
// failed, and the configuration backup is put back when nothing was
// committed meanwhile on a standalone switch.
func (d *Daemon) failed(st *State, why string) {
	if r, ok := strings.CutPrefix(st.Done, "rolled back: "+st.To+" "); ok {
		why = r // our own rollback on the new slot said why
	}
	env, err := d.P.ReadEnv()
	if err == nil {
		env.Invalidate(st.Slot)
		env.Confirm(d.P.Active())
		if err := d.P.WriteEnv(env); err != nil {
			d.Log.Error("switchd-update: boot state", "err", err)
		}
	}
	if st.Rollback {
		// A rollback that did not come up: the slot we left still works.
		why = "the rollback to " + st.To + " failed: " + why
	}
	note := fmt.Sprintf("rolled back: %s %s; running %s again (%s)", st.To, why, st.From, time.Now().UTC().Format(time.RFC3339))
	if st.Standalone && !st.Rollback && st.Revision != "" && newestRevision(d.SwitchdDir) != st.Revision {
		note += "; the configuration was changed meanwhile and is kept"
	} else if st.Standalone && !st.Rollback && st.Revision != "" {
		if err := d.restoreConfig(st.From); err != nil {
			note += "; the configuration backup could not be put back: " + err.Error()
		} else {
			note += "; the configuration of " + st.From + " was put back"
		}
	}
	st.Done, st.Recorded = note, true
	if err := d.save(st); err != nil {
		d.Log.Error("switchd-update: recording the rollback", "err", err)
	}
	d.Log.Error("switchd-update: "+note, "facility", "change-log")
	d.setState(note)
}

// newestRevision returns the name of the newest stored revision.
func newestRevision(switchdDir string) string {
	ents, _ := os.ReadDir(filepath.Join(switchdDir, "config", "rev"))
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		return ""
	}
	return names[len(names)-1]
}

// backupConfig copies the committed configurations to the backup directory
// of version from (the last two backups are kept).
func (d *Daemon) backupConfig(from string) error {
	src := filepath.Join(d.SwitchdDir, "config")
	dst := filepath.Join(d.BackupDir, from)
	if err := os.RemoveAll(dst + ".new"); err != nil {
		return err
	}
	if err := copyTree(src, dst+".new"); err != nil {
		return err
	}
	os.RemoveAll(dst)
	if err := os.Rename(dst+".new", dst); err != nil {
		return err
	}
	ents, _ := os.ReadDir(d.BackupDir)
	type b struct {
		name string
		t    time.Time
	}
	var bs []b
	for _, e := range ents {
		if i, err := e.Info(); err == nil && e.IsDir() && !strings.HasSuffix(e.Name(), ".new") {
			bs = append(bs, b{e.Name(), i.ModTime()})
		}
	}
	slices.SortFunc(bs, func(x, y b) int { return y.t.Compare(x.t) })
	for i, x := range bs {
		if i >= 2 && x.name != from {
			os.RemoveAll(filepath.Join(d.BackupDir, x.name))
		}
	}
	return nil
}

// restoreConfig puts the backup of version v back (before switchd starts).
func (d *Daemon) restoreConfig(v string) error {
	src := filepath.Join(d.BackupDir, v)
	if _, err := os.Stat(src); err != nil {
		return err
	}
	dst := filepath.Join(d.SwitchdDir, "config")
	tmp := dst + ".restore"
	os.RemoveAll(tmp)
	if err := copyTree(src, tmp); err != nil {
		return err
	}
	old := dst + ".after-failed-update"
	os.RemoveAll(old)
	if err := os.Rename(dst, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmp, dst)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if e.IsDir() {
			return os.MkdirAll(t, 0o700)
		}
		if !e.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return writeSync(t, raw)
	})
}

func (d *Daemon) serve(ctx context.Context, c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Minute)) // install writes a slot
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var r Request
	rep := Reply{}
	if err := json.Unmarshal(line, &r); err != nil {
		rep.Err = err.Error()
	} else if err := d.handle(ctx, r, &rep); err != nil {
		rep.Err = err.Error()
	}
	raw, _ := json.Marshal(rep)
	c.Write(append(raw, '\n'))
}

func (d *Daemon) handle(ctx context.Context, r Request, rep *Reply) error {
	switch r.Op {
	case "status":
	case "healthy":
		if r.Version == d.P.Version() {
			d.mu.Lock()
			if d.healthy != nil {
				close(d.healthy)
				d.healthy = nil
			}
			d.mu.Unlock()
		}
	case "maintenance-done":
		if st := d.Load(); st != nil {
			st.MaintenanceDone = true
			st.ExitMaintenance = false
			d.save(st)
		}
	case "check":
		// The configuration check of a bundle's image before the update
		// (on the master): written into the backup slot like an install,
		// but not made bootable.
		d.mu.Lock()
		if d.busy {
			d.mu.Unlock()
			return errors.New("an update is in progress on this member")
		}
		d.busy = true
		d.mu.Unlock()
		text, err := d.check(r.Bundle)
		d.mu.Lock()
		d.busy = false
		d.mu.Unlock()
		d.setState("idle")
		if err != nil {
			return err
		}
		rep.Text = text
	case "started":
		// switchd started (counts the starts of a new version).
		if st := d.Load(); st != nil && st.Done == "" && st.Slot == d.P.Active() && r.Version == st.To {
			st.Starts++
			d.save(st)
			if st.Starts > MaxStarts {
				go d.rollback(st, fmt.Sprintf("switchd did not come up after %d starts", MaxStarts))
			}
		}
	case "install", "rollback":
		d.mu.Lock()
		if d.busy {
			d.mu.Unlock()
			return errors.New("an update is in progress on this member")
		}
		d.busy = true
		d.mu.Unlock()
		var err error
		if r.Op == "install" {
			err = d.install(r, rep)
		} else {
			err = d.startRollback(r, rep)
		}
		if err != nil {
			d.mu.Lock()
			d.busy = false
			d.mu.Unlock()
			d.setState("idle (the last update failed: " + err.Error() + ")")
			return err
		}
		go func() {
			time.Sleep(d.RebootDelay)
			d.setState("rebooting into " + rep.Version)
			if err := d.P.Reboot(); err != nil {
				d.setState("failed: reboot: " + err.Error())
			}
		}()
	default:
		return fmt.Errorf("unknown request %q", r.Op)
	}
	d.mu.Lock()
	rep.State = d.state
	d.mu.Unlock()
	rep.Active = d.P.Active()
	if env, err := d.P.ReadEnv(); err == nil {
		for _, s := range software.Slots {
			rep.Slots = append(rep.Slots, env.Slot(s))
		}
	}
	if st := d.Load(); st != nil {
		rep.Update = st
		if st.Done != "" && st.Done != "ok" {
			rep.Note = st.Done
		}
	}
	return nil
}

// openBundle opens and checks a bundle for this system.
func (d *Daemon) openBundle(path string) (*os.File, *software.Bundle, error) {
	keys, err := d.P.Keys()
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	b, err := software.OpenBundle(f, keys)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	m := &b.Manifest
	running := d.P.Version()
	if m.Compatible != software.Compatible {
		f.Close()
		return nil, nil, fmt.Errorf("the bundle is for %s, this system is %s", m.Compatible, software.Compatible)
	}
	if m.MinFrom != "" && software.CompareVersions(running, m.MinFrom) < 0 {
		f.Close()
		return nil, nil, fmt.Errorf("%s updates from %s or later; this member runs %s (update to %s first)", m.Version, m.MinFrom, running, m.MinFrom)
	}
	return f, b, nil
}

// writeBackup writes the bundle's image into the backup slot, which is
// marked not bootable first; it returns the slot.
func (d *Daemon) writeBackup(b *software.Bundle) (string, software.Env, error) {
	m := &b.Manifest
	active := d.P.Active()
	slot := software.OtherSlot(active)
	env, err := d.P.ReadEnv()
	if err != nil {
		return "", nil, err
	}
	env.Invalidate(slot)
	env.Confirm(active)
	if err := d.P.WriteEnv(env); err != nil {
		return "", nil, fmt.Errorf("boot state: %w", err)
	}
	d.setState(fmt.Sprintf("writing %s into slot %s", m.Version, slot))
	img, err := b.Image()
	if err != nil {
		return "", nil, err
	}
	if err := d.P.WriteSlot(slot, img, m.ImageSize, m.ImageSHA256); err != nil {
		return "", nil, err
	}
	if !b.Verified() {
		return "", nil, errors.New("the image was not verified completely")
	}
	return slot, env, nil
}

// check writes the bundle into the backup slot (not bootable) and runs its
// configuration check.
func (d *Daemon) check(path string) (string, error) {
	f, b, err := d.openBundle(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	slot, _, err := d.writeBackup(b)
	if err != nil {
		return "", err
	}
	d.setState("checking the configuration with " + b.Manifest.Version)
	text, err := d.P.CheckConfig(slot, &b.Manifest)
	if err != nil {
		return "", fmt.Errorf("%w (%s):\n%v", ErrRejected, b.Manifest.Version, err)
	}
	return text, nil
}

// install writes the bundle into the backup slot and makes it the slot to
// boot (docs/os-image.md §4.2); the caller reboots.
func (d *Daemon) install(r Request, rep *Reply) error {
	f, b, err := d.openBundle(r.Bundle)
	if err != nil {
		return err
	}
	defer f.Close()
	m := &b.Manifest
	running := d.P.Version()
	if m.Version == running {
		return fmt.Errorf("this member already runs %s", m.Version)
	}
	active := d.P.Active()
	// 1, 2: the backup slot, not bootable while it is written and read back.
	slot, env, err := d.writeBackup(b)
	if err != nil {
		return err
	}
	// 3. The new system's configuration check.
	d.setState("checking the configuration with " + m.Version)
	text, err := d.P.CheckConfig(slot, m)
	if err != nil {
		if !r.NoValidate {
			return fmt.Errorf("%w (%s; 'no-validate' updates anyway):\n%v", ErrRejected, m.Version, err)
		}
		text = fmt.Sprintf("%s rejects the active configuration (no-validate: continuing):\n%s", m.Version, err)
	}
	rep.Text = text
	// 4. Configuration backup and the update record.
	if err := d.backupConfig(running); err != nil {
		return fmt.Errorf("keeping a copy of the configuration: %w", err)
	}
	st := &State{From: running, To: m.Version, FromSlot: active, Slot: slot, Since: time.Now().UTC(),
		Revision: newestRevision(d.SwitchdDir), Standalone: r.Standalone, ExitMaintenance: r.ExitMaintenance}
	if err := d.save(st); err != nil {
		return err
	}
	// 5. The boot state, last.
	env.Installed(slot, m)
	if err := d.P.WriteEnv(env); err != nil {
		return fmt.Errorf("boot state: %w", err)
	}
	d.Log.Warn(fmt.Sprintf("switchd-update: %s written to slot %s; rebooting", m.Version, slot), "facility", "change-log", "from", running)
	rep.Version = m.Version
	return nil
}

// startRollback makes the backup slot the one to boot.
func (d *Daemon) startRollback(r Request, rep *Reply) error {
	active := d.P.Active()
	slot := software.OtherSlot(active)
	env, err := d.P.ReadEnv()
	if err != nil {
		return err
	}
	info := env.Slot(slot)
	if !info.OK || env[slot+"_ROOTHASH"] == "" {
		return fmt.Errorf("slot %s holds no working system to return to", slot)
	}
	st := &State{From: d.P.Version(), To: info.Version, FromSlot: active, Slot: slot, Since: time.Now().UTC(),
		Revision: newestRevision(d.SwitchdDir), ExitMaintenance: r.ExitMaintenance, Rollback: true}
	if err := d.save(st); err != nil {
		return err
	}
	env.Confirm(active)
	env[slot+"_TRY"] = "0"
	env.First(slot)
	if err := d.P.WriteEnv(env); err != nil {
		return err
	}
	d.Log.Warn("switchd-update: returning to "+info.Version+" (slot "+slot+"); rebooting", "facility", "change-log")
	rep.Version = info.Version
	return nil
}

// watchNew waits until the new switchd is healthy, then confirms the slot;
// else it rolls back.
func (d *Daemon) watchNew(ctx context.Context, st *State, within time.Duration) {
	d.mu.Lock()
	ch := d.healthy
	d.mu.Unlock()
	d.setState("waiting for switchd " + st.To)
	t := time.NewTimer(within)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-ch:
	case <-t.C:
		d.rollback(st, fmt.Sprintf("switchd was not healthy within %s", within))
		return
	}
	env, err := d.P.ReadEnv()
	if err == nil {
		env.Confirm(st.Slot)
		err = d.P.WriteEnv(env)
	}
	if err != nil {
		d.setState("failed: confirming slot " + st.Slot + ": " + err.Error())
		return
	}
	cur := d.Load()
	if cur != nil {
		st = cur
	}
	st.Done = "ok"
	d.save(st)
	d.mu.Lock()
	d.busy = false
	d.mu.Unlock()
	d.Log.Warn("switchd-update: "+st.To+" runs and is healthy; slot "+st.Slot+" confirmed", "facility", "change-log")
	d.setState("idle (" + st.To + " is healthy)")
}

// rollback returns to the old slot and reboots.
func (d *Daemon) rollback(st *State, why string) {
	d.Log.Error("switchd-update: "+st.To+": "+why+"; returning to "+st.From, "facility", "change-log")
	env, err := d.P.ReadEnv()
	if err == nil {
		env.Invalidate(st.Slot)
		err = d.P.WriteEnv(env)
	}
	if err != nil {
		// The boot loader still skips the unconfirmed slot after a reboot.
		d.Log.Error("switchd-update: boot state", "err", err)
	}
	cur := d.Load()
	if cur != nil {
		st = cur
	}
	st.Done = "rolled back: " + st.To + " " + why
	d.save(st)
	d.setState("rolled back: " + st.To + " " + why + "; rebooting into " + st.From)
	if err := d.P.Reboot(); err != nil {
		d.setState("failed: reboot: " + err.Error())
	}
}

// watchPlain confirms a boot without an update once switchd is healthy.
func (d *Daemon) watchPlain(ctx context.Context) {
	d.mu.Lock()
	ch := d.healthy
	d.mu.Unlock()
	select {
	case <-ctx.Done():
		return
	case <-ch:
	}
	env, err := d.P.ReadEnv()
	if err != nil {
		return
	}
	if env.Slot(d.P.Active()).Tried {
		env.Confirm(d.P.Active())
		if err := d.P.WriteEnv(env); err != nil {
			d.Log.Error("switchd-update: confirming the boot", "err", err)
		}
	}
}

// Call sends one request to the daemon at socket.
func Call(socket string, r Request) (Reply, error) {
	return CallTimeout(socket, r, 20*time.Second)
}

// CallTimeout is Call with a deadline (install writes a slot).
func CallTimeout(socket string, r Request, timeout time.Duration) (Reply, error) {
	c, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return Reply{}, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	raw, _ := json.Marshal(r)
	if _, err := c.Write(append(raw, '\n')); err != nil {
		return Reply{}, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return Reply{}, err
	}
	var rep Reply
	if err := json.Unmarshal(line, &rep); err != nil {
		return Reply{}, err
	}
	if rep.Err != "" {
		return rep, errors.New(rep.Err)
	}
	return rep, nil
}
