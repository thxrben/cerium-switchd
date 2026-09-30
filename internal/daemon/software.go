package daemon

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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"mclag/internal/cli"
	"mclag/internal/commit"
	"mclag/internal/config"
	"mclag/internal/software"
	"mclag/internal/stack"
	"mclag/internal/version"
)

// updater updates the stack's software (reference 3.6): the master fetches
// and checks a package, hands it to every member over the stacking
// protocol and updates the members one by one, drained, itself last.
type updater struct {
	member  int
	dir     string // <state>/software: packages
	inst    *software.Installer
	vc      *stack.Manager
	ctl     *stackCtl // nil: standalone
	engine  func() *commit.Engine
	maint   func() *maintCtl
	mgmtVRF func() string
	restart func()
	log     *slog.Logger

	mu  sync.Mutex
	run *cli.SoftwareRun // the current or last update started here
}

// memberWait is how long a member may take to come back after its update.
const memberWait = 5 * time.Minute

// swStatus is one member's software state.
type swStatus struct {
	Member      int               `json:"member"`
	Version     string            `json:"version"`
	Built       string            `json:"built"`
	Arch        string            `json:"arch"`
	State       software.State    `json:"state"`
	Maintenance bool              `json:"maintenance"`
	Current     bool              `json:"current"`
	Packages    map[string]string `json:"packages,omitempty"` // version -> file
	// Transit: member pairs that have no other stacking path than this
	// member (cut off while it restarts).
	Transit []string `json:"transit,omitempty"`
}

type swInstall struct {
	Version  string `json:"version"`
	Rollback bool   `json:"rollback,omitempty"`
	// Force: update even if the member is the only path to others (they
	// are cut off while it restarts).
	Force bool `json:"force,omitempty"`
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
	st := swStatus{Member: u.member, Version: version.Version, Built: version.Date, Arch: software.Arch(), State: u.inst.Load(),
		Packages: map[string]string{}}
	if m := u.maint(); m != nil {
		st.Maintenance = m.active()
		st.Transit = m.transit()
	}
	st.Current = u.ctl == nil || u.ctl.node.Current()
	files, _ := filepath.Glob(filepath.Join(u.dir, "ceros-*.tar.gz"))
	for _, f := range files {
		v := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "ceros-"), ".tar.gz")
		st.Packages[v] = f
	}
	return st
}

func (u *updater) pkgPath(v string) string { return filepath.Join(u.dir, "ceros-"+v+".tar.gz") }

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
		_, err = software.ReadFile(tmp, h.SHA256)
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

// prune keeps the newest packages (the current one and one more).
func (u *updater) prune(keep string) {
	files, _ := filepath.Glob(filepath.Join(u.dir, "ceros-*.tar.gz"))
	slices.SortFunc(files, func(a, b string) int {
		ia, _ := os.Stat(a)
		ib, _ := os.Stat(b)
		if ia == nil || ib == nil {
			return 0
		}
		return ib.ModTime().Compare(ia.ModTime())
	})
	for i, f := range files {
		if i >= 2 && !strings.Contains(f, "ceros-"+keep+".tar.gz") {
			os.Remove(f)
		}
	}
}

// installHere updates this member: drains it (maintenance mode, kept over
// the restart), installs the program and restarts switchd; the new
// version leaves maintenance mode once it is healthy.
func (u *updater) installHere(r swInstall, by string) (string, error) {
	var prog []byte
	if !r.Rollback {
		p, err := software.ReadFile(u.pkgPath(r.Version), "")
		if err != nil {
			return "", err
		}
		if prog, err = p.Program(software.Arch()); err != nil {
			return "", err
		}
	}
	var out strings.Builder
	exitMaint := false
	if m := u.maint(); m != nil && !r.Force {
		if t := m.transit(); len(t) > 0 {
			return "", fmt.Errorf("member %d is the only stacking path between %s: they would be cut off while it restarts "+
				"(cable them to another member, or update with 'force')", u.member, strings.Join(t, ", "))
		}
	}
	if m := u.maint(); m != nil && !m.active() {
		text, err := m.enter(false, true, "software update")
		if err != nil {
			return "", fmt.Errorf("cannot drain member %d: %w", u.member, err)
		}
		out.WriteString(text)
		exitMaint = true
	}
	var err error
	to := r.Version
	if r.Rollback {
		to, err = u.inst.Rollback(version.Version, exitMaint)
	} else {
		err = u.inst.Install(prog, r.Version, version.Version, exitMaint)
	}
	if err != nil {
		if exitMaint {
			u.maint().exit("software update")
		}
		return "", err
	}
	u.log.Warn("software: installed; switchd restarts", "facility", "change-log", "version", to, "from", version.Version, "by", by)
	go func() {
		time.Sleep(time.Second) // the answer goes out first
		u.restart()
	}()
	fmt.Fprintf(&out, "member %d installs %s and restarts\n", u.member, to)
	return out.String(), nil
}

// healthy runs once this switchd works (configuration applied, stack
// state current): a pending update is done.
func (u *updater) healthy() {
	p, err := u.inst.Healthy(version.Version)
	if err != nil || p == nil {
		return
	}
	u.log.Warn("software: now running "+version.Version, "facility", "change-log")
	if p.ExitMaintenance {
		if m := u.maint(); m != nil && m.active() {
			m.exit("software update")
		}
	}
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
			if st.State.Previous == "" {
				u.say("member %d: no previous version, skipped", id)
				continue
			}
			want = st.State.Previous
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
			u.say("this member (the master) is last: it drains, hands mastership on, installs and restarts")
			text, err = u.installHere(swInstall{Version: target, Rollback: req.Rollback, Force: req.Force}, "the update")
			if err != nil {
				u.finish(fmt.Errorf("member %d: %v", id, err))
				return
			}
			u.say("%s", strings.TrimSpace(text))
			u.finish(nil)
			return
		}
		raw, err := u.ctl.node.Call(id, "sw-install", swInstall{Version: target, Rollback: req.Rollback, Force: req.Force}, 2*time.Minute)
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
	time.Sleep(3 * time.Second) // it restarts
	var last swStatus
	for time.Now().Before(deadline) {
		st, err := u.statusOf(id)
		if err == nil {
			last = st
			if st.Version == v && st.Current && !st.Maintenance && st.State.Pending == nil {
				return nil
			}
			if st.State.Note != "" && st.Version != v && st.State.Pending == nil {
				return fmt.Errorf("member %d returned to %s: %s; the update stops here", id, st.Version, st.State.Note)
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("member %d is not back with %s after %s (it runs %q, maintenance %v); the update stops here",
		id, v, memberWait, last.Version, last.Maintenance)
}

// prepare fetches, verifies, checks and distributes the package; it
// returns its version.
func (u *updater) prepare(ctx context.Context, req cli.SoftwareRequest) (string, error) {
	src, err := software.ParseSource(req.Source)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		return "", err
	}
	f := &software.Fetcher{VRF: u.mgmtVRF()}
	tmp := filepath.Join(u.dir, "incoming.tar.gz")
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
	pkg, err := software.ReadFile(tmp, sum)
	if err != nil {
		return "", err
	}
	v := pkg.Manifest.Version
	if sum != "" {
		u.say("package %s (built %s), SHA-256 verified", v, pkg.Manifest.Built)
	} else {
		u.say("package %s (built %s); no SHA-256 given, the files are verified against the manifest", v, pkg.Manifest.Built)
	}
	// Every member's architecture must be in the package.
	for _, id := range u.members() {
		st, err := u.statusOf(id)
		if err != nil {
			return "", fmt.Errorf("member %d: %v", id, err)
		}
		if _, err := pkg.Program(st.Arch); err != nil {
			return "", fmt.Errorf("member %d: %v", id, err)
		}
	}
	// The new version must accept the active configuration.
	if err := u.checkConfig(pkg, req.NoValidate); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, u.pkgPath(v)); err != nil {
		return "", err
	}
	u.prune(v)
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
		u.say("sending the package to member %d", id)
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

// checkConfig runs the new program's configuration check on the active
// configuration.
func (u *updater) checkConfig(pkg *software.Package, noValidate bool) error {
	prog, err := pkg.Program(software.Arch())
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "ceros-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "switchd")
	cfgFile := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(config.ToJSON(u.engine().Active().Root))
	if err := os.WriteFile(bin, prog, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(cfgFile, raw, 0o600); err != nil {
		return err
	}
	out, err := exec.Command(bin, "check-config", cfgFile).CombinedOutput()
	text := strings.TrimSpace(string(out))
	var ee *exec.ExitError
	switch {
	case err == nil:
		if text != "" {
			u.say("the new version accepts the configuration, with warnings:\n%s", text)
		} else {
			u.say("the new version accepts the configuration")
		}
		return nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		if noValidate {
			u.say("the new version rejects the configuration (no-validate: continuing):\n%s", text)
			return nil
		}
		return fmt.Errorf("the new version rejects the active configuration ('no-validate' updates anyway):\n%s", text)
	default:
		return fmt.Errorf("the new program cannot check the configuration: %v %s", err, text)
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
			m.Version, m.Built, m.Previous, m.Note, m.Maintenance = st.Version, st.Built, st.State.Previous, st.State.Note, st.Maintenance
			if st.State.Pending != nil {
				m.Pending = st.State.Pending.Version
			}
		}
		out.Members = append(out.Members, m)
	}
	return out, nil
}
