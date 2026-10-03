package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/thxrben/cerium-switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/software"
	"github.com/thxrben/cerium-switchd/internal/stack"
	"github.com/thxrben/cerium-switchd/internal/updated"
	"github.com/thxrben/cerium-switchd/internal/version"
)

// updater updates the stack's software (reference 3.6): the master fetches
// and checks a bundle, hands it to every member over the stacking protocol
// and updates the members one by one, drained, itself last. On each member
// the update daemon writes the bundle into the backup slot and reboots
// (docs/os-image.md §4).
type updater struct {
	member  int
	dir     string // bundles (on the data partition)
	vc      *stack.Manager
	ctl     *stackCtl // nil: standalone
	engine  func() *commit.Engine
	maint   func() *maintCtl
	mgmtVRF func() string
	// updateSocket is the update daemon's socket ("": the default).
	updateSocket string
	log          *slog.Logger

	mu  sync.Mutex
	run *cli.SoftwareRun // the current or last update started here
}

// softwareDir keeps received bundles: on the data partition of the image
// (they are large and can be fetched again).
const softwareDir = "/var/lib/ceros/software"

// memberWait is how long a member may take to come back after its update
// (a reboot, and a rollback with a second reboot inside it).
const memberWait = 10 * time.Minute

// installWait bounds the update daemon's install (it writes a slot).
const installWait = 15 * time.Minute

// swStatus is one member's software state.
type swStatus struct {
	Member      int               `json:"member"`
	Version     string            `json:"version"`
	Built       string            `json:"built"`
	Arch        string            `json:"arch"`
	Maintenance bool              `json:"maintenance"`
	Current     bool              `json:"current"`
	Packages    map[string]string `json:"packages,omitempty"` // version -> bundle file
	// Transit: member pairs that have no other stacking path than this
	// member (cut off while it reboots).
	Transit []string `json:"transit,omitempty"`
	// The update daemon's view ("" / nil: it does not run, e.g. not an
	// image).
	Daemon string              `json:"daemon,omitempty"`
	Slots  []software.SlotInfo `json:"slots,omitempty"`
	Active string              `json:"active,omitempty"`
	Update *updated.State      `json:"update,omitempty"`
	Note   string              `json:"note,omitempty"`
}

// updating: an update of this member is in progress (written, rebooting,
// or waiting for the new version to be healthy).
func (st swStatus) updating() bool { return st.Update != nil && st.Update.Done == "" }

// previous: the version in the backup slot ("": none that boots).
func (st swStatus) previous() string {
	for _, sl := range st.Slots {
		if sl.Name != st.Active && sl.OK && sl.Version != "" {
			return sl.Version
		}
	}
	return ""
}

type swInstall struct {
	Version  string `json:"version"`
	Rollback bool   `json:"rollback,omitempty"`
	// Force: update even if the member is the only path to others (they
	// are cut off while it reboots).
	Force      bool `json:"force,omitempty"`
	NoValidate bool `json:"no_validate,omitempty"`
}

// start registers the stacking protocol handlers.
func (u *updater) start(ctx context.Context) {
	if u.ctl == nil {
		return
	}
	u.ctl.node.Handle("sw-status", func(int, json.RawMessage) (any, error) { return u.status(), nil })
	u.ctl.node.Handle("sw-install", func(from int, raw json.RawMessage) (any, error) {
		var r swInstall
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return u.installHere(r, fmt.Sprintf("member %d", from))
	})
	l := u.vc.Mesh().Listen("software")
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go u.receive(nc)
		}
	}()
}

func (u *updater) status() swStatus {
	st := swStatus{Member: u.member, Version: version.Version, Built: version.Date, Arch: software.Arch(),
		Packages: map[string]string{}}
	if m := u.maint(); m != nil {
		st.Maintenance = m.active()
		st.Transit = m.transit()
	}
	st.Current = u.ctl == nil || u.ctl.node.Current()
	if rep, err := updated.Call(u.daemonSocket(), updated.Request{Op: "status"}); err == nil {
		st.Daemon, st.Slots, st.Active, st.Update, st.Note = rep.State, rep.Slots, rep.Active, rep.Update, rep.Note
	}
	files, _ := filepath.Glob(filepath.Join(u.dir, "ceros-*-"+software.Arch()+".bundle"))
	for _, f := range files {
		v := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "ceros-"), "-"+software.Arch()+".bundle")
		st.Packages[v] = f
	}
	return st
}

func (u *updater) pkgPath(v string) string {
	return filepath.Join(u.dir, "ceros-"+v+"-"+software.Arch()+".bundle")
}

// verify checks a bundle file completely (signature and image) with the
// keys this system trusts.
func verifyBundle(path string) (*software.BundleManifest, error) {
	keys, err := software.LoadKeys(updated.KeysDir)
	if err != nil {
		return nil, err
	}
	return software.VerifyBundleFile(path, keys)
}

// receive stores a package sent by the master: a JSON line with version
// and SHA-256, then the package; the answer is "ok" or an error line.
func (u *updater) receive(nc net.Conn) {
	defer nc.Close()
	r := bufio.NewReader(nc)
	var h struct {
		Version, SHA256 string
		Size            int64
	}
	line, err := r.ReadBytes('\n')
	if err != nil || json.Unmarshal(line, &h) != nil || h.Version == "" || strings.ContainsAny(h.Version, "/\x00") {
		return
	}
	fail := func(err error) { fmt.Fprintf(nc, "error: %v\n", err) }
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		fail(err)
		return
	}
	tmp := u.pkgPath(h.Version) + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		fail(err)
		return
	}
	_, err = io.CopyN(f, r, h.Size)
	f.Close()
	if err == nil {
		err = checkSum(tmp, h.SHA256)
	}
	if err == nil {
		var m *software.BundleManifest
		if m, err = verifyBundle(tmp); err == nil && m.Version != h.Version {
			err = fmt.Errorf("the bundle holds %s, not %s", m.Version, h.Version)
		}
	}
	if err != nil {
		os.Remove(tmp)
		fail(err)
		return
	}
	if err := os.Rename(tmp, u.pkgPath(h.Version)); err != nil {
		fail(err)
		return
	}
	u.prune(h.Version)
	fmt.Fprintln(nc, "ok")
}

// checkSum compares a file's SHA-256 with want ("": not checked).
func checkSum(path, want string) error {
	if want == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("the bundle's SHA-256 is %s, expected %s", got, strings.ToLower(want))
	}
	return nil
}

// prune keeps the newest bundles (the current one and one more).
func (u *updater) prune(keep string) {
	files, _ := filepath.Glob(filepath.Join(u.dir, "ceros-*.bundle"))
	slices.SortFunc(files, func(a, b string) int {
		ia, _ := os.Stat(a)
		ib, _ := os.Stat(b)
		if ia == nil || ib == nil {
			return 0
		}
		return ib.ModTime().Compare(ia.ModTime())
	})
	for i, f := range files {
		if i >= 2 && f != u.pkgPath(keep) {
			os.Remove(f)
		}
	}
}

// installHere updates this member: drains it (maintenance mode, kept over
// the reboot) and hands the bundle to the update daemon, which writes the
// backup slot and reboots; the new version leaves maintenance mode once it
// is healthy.
func (u *updater) installHere(r swInstall, by string) (string, error) {
	if _, err := updated.Call(u.daemonSocket(), updated.Request{Op: "status"}); err != nil {
		return "", fmt.Errorf("member %d cannot be updated: its update daemon does not run (%v); updates need the cerOS image", u.member, err)
	}
	req := updated.Request{Op: "install", Bundle: u.pkgPath(r.Version), Standalone: len(u.members()) == 1, NoValidate: r.NoValidate}
	if r.Rollback {
		req = updated.Request{Op: "rollback"}
	} else if _, err := os.Stat(req.Bundle); err != nil {
		return "", fmt.Errorf("member %d does not have the bundle %s", u.member, r.Version)
	}
	var out strings.Builder
	if m := u.maint(); m != nil && !r.Force {
		if t := m.transit(); len(t) > 0 {
			return "", fmt.Errorf("member %d is the only stacking path between %s: they would be cut off while it reboots "+
				"(cable them to another member, or update with 'force')", u.member, strings.Join(t, ", "))
		}
	}
	if m := u.maint(); m != nil && !m.active() {
		text, err := m.enter(false, true, "software update")
		if err != nil {
			return "", fmt.Errorf("cannot drain member %d: %w", u.member, err)
		}
		out.WriteString(text)
		req.ExitMaintenance = true
	}
	rep, err := updated.CallTimeout(u.daemonSocket(), req, installWait)
	if err != nil {
		if req.ExitMaintenance {
			u.maint().exit("software update")
		}
		return "", fmt.Errorf("member %d: %w", u.member, err)
	}
	if rep.Text != "" {
		fmt.Fprintf(&out, "%s\n", rep.Text)
	}
	u.log.Warn("software: the update daemon reboots into "+rep.Version, "facility", "change-log", "from", version.Version, "by", by)
	fmt.Fprintf(&out, "member %d reboots into %s\n", u.member, rep.Version)
	return out.String(), nil
}

// started runs when switchd starts: the update daemon counts the starts of
// a new version.
func (u *updater) started() {
	updated.Call(u.daemonSocket(), updated.Request{Op: "started", Version: version.Version})
}

// healthy runs once this switchd works (configuration applied, stack
// state current): the update daemon confirms the slot, and a member the
// update drained leaves maintenance mode.
func (u *updater) healthy() {
	rep, err := updated.Call(u.daemonSocket(), updated.Request{Op: "healthy", Version: version.Version})
	if err != nil {
		u.log.Debug("software: update daemon", "err", err)
		return
	}
	st := rep.Update
	if st == nil || !st.ExitMaintenance || (st.To != version.Version && st.From != version.Version) {
		return
	}
	if st.To == version.Version {
		u.log.Warn("software: now running "+version.Version, "facility", "change-log")
	}
	if m := u.maint(); m != nil && m.active() {
		m.exit("software update")
	}
	updated.Call(u.daemonSocket(), updated.Request{Op: "maintenance-done"})
}

// ---- the update, run by the master ----

// Start begins an update (or rollback) and returns at once; progress is in
// Status.
func (u *updater) Start(req cli.SoftwareRequest) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.run != nil && !u.run.Done {
		return errors.New("an update is running (show system software)")
	}
	if u.ctl != nil && !u.ctl.node.IsMaster() {
		return errors.New("updates run on the master")
	}
	u.run = &cli.SoftwareRun{Request: req, Started: time.Now()}
	go u.do(req)
	return nil
}

func (u *updater) say(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	u.mu.Lock()
	u.run.Lines = append(u.run.Lines, line)
	u.mu.Unlock()
	u.log.Info("software update: " + line)
}

func (u *updater) finish(err error) {
	u.mu.Lock()
	u.run.Done = true
	if err != nil {
		u.run.Failed = err.Error()
	}
	u.mu.Unlock()
	if err != nil {
		u.log.Error("software update failed", "facility", "change-log", "err", err)
	}
}

// members returns the stack's members (this one alone when standalone).
func (u *updater) members() []int {
	if u.ctl == nil {
		return []int{u.member}
	}
	var ids []int
	for id := range u.ctl.node.Members() {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (u *updater) statusOf(id int) (swStatus, error) {
	if id == u.member {
		return u.status(), nil
	}
	raw, err := u.ctl.node.Call(id, "sw-status", nil, 10*time.Second)
	if err != nil {
		return swStatus{}, err
	}
	var st swStatus
	return st, json.Unmarshal(raw, &st)
}

func (u *updater) do(req cli.SoftwareRequest) {
	ctx := context.Background()
	target := ""
	if !req.Rollback {
		v, err := u.prepare(ctx, req)
		if err != nil {
			u.finish(err)
			return
		}
		target = v
	}
	ids := u.members()
	if req.Member != 0 {
		if !slices.Contains(ids, req.Member) {
			u.finish(fmt.Errorf("member %d is not in this virtual chassis", req.Member))
			return
		}
		ids = []int{req.Member}
	}
	// The master last: it hands mastership on when it drains.
	slices.SortStableFunc(ids, func(a, b int) int {
		if a == u.member {
			return 1
		}
		if b == u.member {
			return -1
		}
		return 0
	})
	for _, id := range ids {
		st, err := u.statusOf(id)
		if err != nil {
			u.finish(fmt.Errorf("member %d: %v; the update stops here", id, err))
			return
		}
		want := target
		if req.Rollback {
			if st.previous() == "" {
				u.say("member %d: no previous version, skipped", id)
				continue
			}
			want = st.previous()
		} else if st.Version == target {
			u.say("member %d already runs %s", id, target)
			continue
		}
		if len(st.Transit) > 0 && !req.Force {
			u.finish(fmt.Errorf("member %d is the only stacking path between %s: they would be cut off while it restarts "+
				"(cable them to another member, or update with 'force'); the update stops here", id, strings.Join(st.Transit, ", ")))
			return
		}
		u.say("member %d: %s -> %s", id, st.Version, want)
		var text string
		if id == u.member {
			u.say("this member (the master) is last: it drains, hands mastership on, installs and reboots")
			text, err = u.installHere(swInstall{Version: target, Rollback: req.Rollback, Force: req.Force, NoValidate: req.NoValidate}, "the update")
			if err != nil {
				u.finish(fmt.Errorf("member %d: %v", id, err))
				return
			}
			u.say("%s", strings.TrimSpace(text))
			u.finish(nil)
			return
		}
		raw, err := u.ctl.node.Call(id, "sw-install", swInstall{Version: target, Rollback: req.Rollback, Force: req.Force, NoValidate: req.NoValidate}, installWait)
		if err == nil {
			err = json.Unmarshal(raw, &text)
		}
		if err != nil {
			u.finish(fmt.Errorf("member %d: %v; the update stops here", id, err))
			return
		}
		for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
			u.say("member %d: %s", id, l)
		}
		if err := u.waitFor(id, want); err != nil {
			u.finish(err)
			return
		}
		u.say("member %d runs %s and carries traffic again", id, want)
	}
	u.say("done")
	u.finish(nil)
}

// waitFor waits until member id runs v, is current and out of maintenance.
func (u *updater) waitFor(id int, v string) error {
	deadline := time.Now().Add(memberWait)
	time.Sleep(5 * time.Second) // it reboots
	var last swStatus
	for time.Now().Before(deadline) {
		st, err := u.statusOf(id)
		if err == nil {
			last = st
			if st.Version == v && st.Current && !st.Maintenance && !st.updating() {
				return nil
			}
			if st.Update != nil && strings.HasPrefix(st.Update.Done, "rolled back") && st.Version != v {
				return fmt.Errorf("member %d %s; the update stops here", id, st.Update.Done)
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("member %d is not back with %s after %s (it runs %q, maintenance %v); the update stops here",
		id, v, memberWait, last.Version, last.Maintenance)
}

// prepare fetches, verifies, checks and distributes the bundle; it returns
// its version.
func (u *updater) prepare(ctx context.Context, req cli.SoftwareRequest) (string, error) {
	src, err := software.ParseSource(req.Source)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		return "", err
	}
	f := &software.Fetcher{VRF: u.mgmtVRF()}
	tmp := filepath.Join(u.dir, "incoming.bundle")
	defer os.Remove(tmp)
	u.say("fetching %s", src.Raw)
	if err := f.Fetch(ctx, src, tmp, req.Password); err != nil {
		return "", err
	}
	sum := req.SHA256
	if sum == "" {
		if s, ok := f.FetchOptional(ctx, src, ".sha256"); ok {
			if fs := strings.Fields(s); len(fs) > 0 {
				sum = fs[0]
			}
		}
	}
	if err := checkSum(tmp, sum); err != nil {
		return "", err
	}
	m, err := verifyBundle(tmp)
	if err != nil {
		return "", err
	}
	v := m.Version
	if sum != "" {
		u.say("bundle %s for %s (built %s): signature and SHA-256 verified", v, m.Arch, m.Built)
	} else {
		u.say("bundle %s for %s (built %s): signature verified", v, m.Arch, m.Built)
	}
	// Every member must run the image on the bundle's platform.
	for _, id := range u.members() {
		if req.Member != 0 && id != req.Member {
			continue
		}
		st, err := u.statusOf(id)
		if err != nil {
			return "", fmt.Errorf("member %d: %v", id, err)
		}
		if st.Arch != m.Arch {
			return "", fmt.Errorf("member %d is %s, the bundle is for %s", id, st.Arch, m.Arch)
		}
		if st.Daemon == "" {
			return "", fmt.Errorf("member %d does not run the cerOS image (its update daemon does not answer)", id)
		}
	}
	if err := os.Rename(tmp, u.pkgPath(v)); err != nil {
		return "", err
	}
	u.prune(v)
	// The new version must accept the active configuration.
	if err := u.checkConfig(v, req.NoValidate); err != nil {
		return "", err
	}
	// Distribute.
	raw, err := os.ReadFile(u.pkgPath(v))
	if err != nil {
		return "", err
	}
	for _, id := range u.members() {
		if id == u.member || (req.Member != 0 && id != req.Member) {
			continue
		}
		if st, err := u.statusOf(id); err == nil && (st.Version == v || st.Packages[v] != "") {
			continue
		}
		u.say("sending the bundle to member %d", id)
		if err := u.send(id, v, raw); err != nil {
			return "", fmt.Errorf("member %d: %v", id, err)
		}
	}
	return v, nil
}

func (u *updater) send(id int, v string, raw []byte) error {
	nc, err := u.vc.Mesh().Dial(id, "software", 10*time.Second)
	if err != nil {
		return err
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(10 * time.Minute))
	h, _ := json.Marshal(map[string]any{"Version": v, "SHA256": software.Sum(raw), "Size": len(raw)})
	if _, err := nc.Write(append(h, '\n')); err != nil {
		return err
	}
	if _, err := nc.Write(raw); err != nil {
		return err
	}
	line, err := bufio.NewReader(nc).ReadString('\n')
	if err != nil {
		return err
	}
	if line = strings.TrimSpace(line); line != "ok" {
		return errors.New(strings.TrimPrefix(line, "error: "))
	}
	return nil
}

// checkConfig has the update daemon run the new image's configuration
// check on the active configuration.
func (u *updater) checkConfig(v string, noValidate bool) error {
	u.say("checking the configuration with %s", v)
	rep, err := updated.CallTimeout(u.daemonSocket(), updated.Request{Op: "check", Bundle: u.pkgPath(v)}, installWait)
	switch {
	case err == nil:
		if rep.Text != "" {
			u.say("the new version accepts the configuration, with warnings:\n%s", rep.Text)
		} else {
			u.say("the new version accepts the configuration")
		}
		return nil
	case errors.Is(err, updated.ErrRejected) || strings.HasPrefix(err.Error(), updated.ErrRejected.Error()):
		if noValidate {
			u.say("%v (no-validate: continuing)", err)
			return nil
		}
		return fmt.Errorf("%v ('no-validate' updates anyway)", err)
	default:
		return fmt.Errorf("checking the configuration with %s: %v", v, err)
	}
}

// Status is "show system software".
func (u *updater) Status() (cli.SoftwareStatus, error) {
	var out cli.SoftwareStatus
	u.mu.Lock()
	if u.run != nil {
		r := *u.run
		r.Lines = slices.Clone(r.Lines)
		out.Run = &r
	}
	u.mu.Unlock()
	for _, id := range u.members() {
		st, err := u.statusOf(id)
		m := cli.SoftwareMember{Member: id}
		if err != nil {
			m.Error = err.Error()
		} else {
			m.Version, m.Built, m.Previous, m.Note, m.Maintenance = st.Version, st.Built, st.previous(), st.Note, st.Maintenance
			m.Daemon = st.Daemon
			if st.updating() {
				m.Pending = st.Update.To
			}
			for _, sl := range st.Slots {
				m.Slots = append(m.Slots, cli.SoftwareSlot{Name: sl.Name, Version: sl.Version, Active: sl.Name == st.Active,
					OK: sl.OK, Next: sl.First})
			}
		}
		out.Members = append(out.Members, m)
	}
	return out, nil
}

func (u *updater) daemonSocket() string {
	if u.updateSocket != "" {
		return u.updateSocket
	}
	return updated.DefaultSocket
}
