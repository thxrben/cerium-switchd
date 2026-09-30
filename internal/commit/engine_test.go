package commit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mclag/internal/config"
)

// fakeClock fires timers only when advanced.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
	c       *fakeClock
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f, c: c}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves time forward and runs due timers synchronously.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for _, t := range c.timers {
		if !t.stopped && !t.at.After(c.now) {
			t.stopped = true
			due = append(due, t.f)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

// fakeApplier records applies; fail decides per member whether to fail.
type fakeApplier struct {
	mu      sync.Mutex
	applied []*config.Tree // the "to" configurations
	froms   []*config.Tree
	fail    func(member string, to *config.Tree) error
}

func (a *fakeApplier) Apply(_ context.Context, from, to *config.Tree) []MemberResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applied = append(a.applied, to.Clone())
	if from != nil {
		from = from.Clone()
	}
	a.froms = append(a.froms, from)
	var out []MemberResult
	for _, m := range []string{"member1", "member2"} {
		r := MemberResult{Member: m}
		if a.fail != nil {
			r.Err = a.fail(m, to)
		}
		out = append(out, r)
	}
	return out
}

func (a *fakeApplier) last() *config.Tree {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.applied[len(a.applied)-1]
}

type rig struct {
	t       *testing.T
	dir     string
	clock   *fakeClock
	applier *fakeApplier
	e       *Engine
	notes   []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir(), clock: newClock(), applier: &fakeApplier{}}
	r.open()
	return r
}

// open (re)creates the engine on the rig's directory, like a restart.
func (r *rig) open() {
	r.t.Helper()
	st, err := OpenFileStore(r.dir, 50)
	if err != nil {
		r.t.Fatal(err)
	}
	e, err := New(Options{
		Store: st, Applier: r.applier, Clock: r.clock,
		Notify: func(_ context.Context, m string) { r.notes = append(r.notes, m) },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.e = e
	r.t.Cleanup(e.Close)
	e.Start(context.Background())
}

func (r *rig) session(user string, class Class, mode Mode) *Session {
	r.t.Helper()
	s, _, err := r.e.Configure(user, class, mode)
	if err != nil {
		r.t.Fatalf("configure %s: %v", mode, err)
	}
	return s
}

func setLines(t *testing.T, s *Session, lines string) {
	t.Helper()
	if err := s.Modify(func(c *config.Tree) error { return config.ApplySetLines(c, lines) }); err != nil {
		t.Fatalf("%q: %v", lines, err)
	}
}

func commit(t *testing.T, s *Session, o CommitOptions) *Result {
	t.Helper()
	res, err := s.Commit(context.Background(), o)
	if err != nil {
		t.Fatalf("commit: %v (issues: %v)", err, res)
	}
	return res
}

func hostName(e *Engine) string { return e.Active().Root.Leaf("system", "host-name") }

func TestCommitRequiresConfirmationAndRollsBack(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	setLines(t, s, "set system host-name a")
	res := commit(t, s, CommitOptions{Comment: "first"})
	if want := r.clock.Now().Add(10 * time.Minute); !res.Deadline.Equal(want) {
		t.Fatalf("deadline %v, want %v", res.Deadline, want)
	}
	if h := r.e.History(); h[0].Confirmed || !h[1].Confirmed || h[0].Comment != "first" || h[0].User != "alice" {
		t.Fatalf("history: %+v", h)
	}
	if hostName(r.e) != "a" {
		t.Fatal("not active")
	}
	r.clock.Advance(9*time.Minute + 59*time.Second)
	if hostName(r.e) != "a" {
		t.Fatal("rolled back early")
	}
	r.clock.Advance(time.Second)
	if hostName(r.e) != "" {
		t.Fatal("not rolled back after the timeout")
	}
	h := r.e.History()
	if h[0].Comment != "automatic rollback: revisions 2–2 not confirmed" || h[0].User != "system" || !h[0].Confirmed {
		t.Fatalf("rollback revision: %+v", h[0])
	}
	if len(r.notes) != 2 || !strings.Contains(r.notes[0], "alice: commit complete (revision 2), must be confirmed within 10 minutes") ||
		!strings.Contains(r.notes[1], "automatic rollback") {
		t.Errorf("notifications: %v", r.notes)
	}
	if r.applier.last().Root.Has("system") || r.e.Pending() != nil {
		t.Error("rollback not applied or still pending")
	}
	// The shared candidate had no further edits, so it follows the rollback.
	if s.Changed() {
		t.Error("shared candidate differs from the rolled back configuration")
	}
}

func TestConfirm(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	setLines(t, s, "set system host-name a")
	commit(t, s, CommitOptions{})
	if err := r.e.Confirm(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Hour)
	if hostName(r.e) != "a" || !r.e.History()[0].Confirmed {
		t.Fatal("confirmed commit rolled back")
	}
	if err := r.e.Confirm(context.Background(), "alice"); !errors.Is(err, ErrNothingToConf) {
		t.Errorf("second confirm: %v", err)
	}

	// commit without changes confirms as well.
	setLines(t, s, "set system host-name b")
	commit(t, s, CommitOptions{})
	res := commit(t, s, CommitOptions{})
	if !res.NoChanges || !res.Confirmed || r.e.Pending() != nil {
		t.Errorf("empty commit did not confirm: %+v", res)
	}
	// ... and without anything pending it is a no-op.
	if res := commit(t, s, CommitOptions{}); !res.NoChanges || res.Confirmed {
		t.Errorf("empty commit: %+v", res)
	}
}

func TestSeriesRestartsTimerAndKeepsTarget(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	setLines(t, s, "set system host-name a")
	commit(t, s, CommitOptions{})
	r.clock.Advance(5 * time.Minute)
	setLines(t, s, "set system host-name b")
	res := commit(t, s, CommitOptions{Confirmed: true, Minutes: 3})
	if p := r.e.Pending(); p.Target != 1 || p.First != 2 || p.Last != 3 || !p.Deadline.Equal(res.Deadline) {
		t.Fatalf("pending: %+v", p)
	}
	r.clock.Advance(6 * time.Minute) // past the first commit's deadline
	if hostName(r.e) != "" {
		t.Fatal("no rollback after the restarted timer")
	}
	if c := r.e.History()[0].Comment; c != "automatic rollback: revisions 2–3 not confirmed" {
		t.Errorf("comment %q", c)
	}
}

func TestOptionalModeAndStricterPolicy(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	setLines(t, s, "set system commit confirmation mode optional")
	if res := commit(t, s, CommitOptions{}); res.Deadline.IsZero() {
		t.Fatal("switching to optional must itself be confirmed (stricter policy)")
	}
	if err := r.e.Confirm(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	setLines(t, s, "set system host-name a")
	if res := commit(t, s, CommitOptions{}); !res.Deadline.IsZero() {
		t.Fatal("plain commit in optional mode needs confirmation")
	}
	setLines(t, s, "set system host-name b")
	res := commit(t, s, CommitOptions{Confirmed: true, Minutes: 2})
	if !res.Deadline.Equal(r.clock.Now().Add(2 * time.Minute)) {
		t.Fatalf("commit confirmed 2: deadline %v", res.Deadline)
	}
	// While pending, even a plain commit in optional mode restarts the timer.
	setLines(t, s, "set system host-name c")
	// The restarted timer uses the configured timeout (10 minutes).
	if res := commit(t, s, CommitOptions{}); !res.Deadline.Equal(r.clock.Now().Add(10 * time.Minute)) {
		t.Fatalf("commit during pending confirmation: deadline %v", res.Deadline)
	}
	r.clock.Advance(3 * time.Minute)
	if hostName(r.e) != "c" {
		t.Fatal("rolled back by the old 2-minute timer")
	}
	r.clock.Advance(7 * time.Minute)
	if hostName(r.e) != "a" {
		t.Fatalf("rolled back to %q, want a", hostName(r.e))
	}
	// Changing the timeout.
	setLines(t, s, "set system commit confirmation mode required\nset system commit confirmation timeout 1")
	if res := commit(t, s, CommitOptions{}); !res.Deadline.Equal(r.clock.Now().Add(time.Minute)) {
		t.Fatalf("configured timeout not used: %v", res.Deadline)
	}
}

func TestApplyFailureReverts(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	setLines(t, s, "set system host-name a")
	commit(t, s, CommitOptions{})
	r.e.Confirm(context.Background(), "alice")
	r.applier.fail = func(m string, to *config.Tree) error {
		if m == "member2" && to.Root.Leaf("system", "host-name") == "bad" {
			return errors.New("netlink: operation not supported")
		}
		return nil
	}
	setLines(t, s, "set system host-name bad")
	res, err := s.Commit(context.Background(), CommitOptions{})
	if !errors.Is(err, ErrApplyFailed) {
		t.Fatalf("err = %v", err)
	}
	if res.Members[1].Err == nil || len(res.Reverted) != 2 || r.applier.last().Root.Leaf("system", "host-name") != "a" {
		t.Errorf("result %+v, last applied %q", res, r.applier.last().Root.Leaf("system", "host-name"))
	}
	if hostName(r.e) != "a" || len(r.e.History()) != 2 || r.e.Pending() != nil {
		t.Error("failed commit changed the active configuration, history or pending state")
	}
	if !s.Changed() {
		t.Error("candidate lost after failed commit")
	}
}

func TestCheckFailureAppliesNothing(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	setLines(t, s, "set vlans a vlan-id 10\nset vlans b vlan-id 10")
	n := len(r.applier.applied)
	if issues := s.Check(); !issues.HasErrors() {
		t.Fatal("check passed")
	}
	res, err := s.Commit(context.Background(), CommitOptions{})
	if !errors.Is(err, ErrCheckFailed) || !res.Issues.HasErrors() || len(r.applier.applied) != n {
		t.Fatalf("err=%v applies=%d", err, len(r.applier.applied)-n)
	}
	if r.e.Pending() != nil || len(r.e.History()) != 1 {
		t.Error("state changed")
	}
}

func TestInactiveStatementsIgnoredByCheck(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	setLines(t, s, "set vlans a vlan-id 10\nset vlans b vlan-id 10\ndeactivate vlans b")
	commit(t, s, CommitOptions{})
	if !r.e.Active().Root.Get("vlans", "b").Inactive {
		t.Error("inactive statement not stored")
	}
}

func TestRestartDuringPendingConfirmation(t *testing.T) {
	// Restart before the deadline: the timer is re-armed.
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	setLines(t, s, "set system host-name a")
	commit(t, s, CommitOptions{})
	r.e.Close()
	r.clock.Advance(4 * time.Minute)
	r.open()
	if hostName(r.e) != "a" || r.e.Pending() == nil {
		t.Fatal("pending commit lost across restart")
	}
	r.clock.Advance(6 * time.Minute)
	if hostName(r.e) != "" {
		t.Fatal("re-armed timer did not roll back")
	}

	// Restart after the deadline: boot straight into the rollback target.
	r2 := newRig(t)
	s2 := r2.session("alice", SuperUser, Shared)
	setLines(t, s2, "set system host-name a")
	commit(t, s2, CommitOptions{})
	r2.e.Close()
	r2.clock.Advance(time.Hour)
	before := len(r2.applier.applied)
	r2.open()
	if hostName(r2.e) != "" || r2.e.Pending() != nil {
		t.Fatal("expired commit not rolled back at startup")
	}
	if got := r2.applier.applied[before:]; len(got) != 1 || got[0].Root.Has("system") || r2.applier.froms[before] != nil {
		t.Errorf("startup should apply only the rollback target, got %d applies", len(got))
	}
}

func TestStoreDiscardsDanglingPending(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenFileStore(dir, 50)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := newRevision(1, time.Now(), "system", "", config.New())
	if err := st.Put(r, 0); err != nil {
		t.Fatal(err)
	}
	// Crash after writing pending state, before the revision it covers.
	if err := st.SetPending(&Pending{Deadline: time.Now(), Target: 1, First: 2}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "rev", "000000000002.json.tmp"), []byte("{garbage"), 0o600)
	st2, err := OpenFileStore(dir, 50)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Pending() != nil || len(st2.Revisions()) != 1 {
		t.Fatalf("pending %+v, %d revisions", st2.Pending(), len(st2.Revisions()))
	}
	if _, err := os.Stat(filepath.Join(dir, pendingFile)); !os.IsNotExist(err) {
		t.Error("dangling pending file not removed")
	}
}

func TestRevisionTrimmingKeepsTarget(t *testing.T) {
	dir := t.TempDir()
	st, _ := OpenFileStore(dir, 3)
	for i := uint64(1); i <= 6; i++ {
		r, _ := newRevision(i, time.Now(), "u", "", config.New())
		if err := st.Put(r, 2); err != nil {
			t.Fatal(err)
		}
	}
	var seqs []uint64
	for _, r := range st.Revisions() {
		seqs = append(seqs, r.Seq)
	}
	if len(seqs) != 3 || seqs[0] != 2 || seqs[2] != 6 {
		t.Errorf("kept %v, want [2 5 6]", seqs)
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "rev"))
	if len(ents) != 3 {
		t.Errorf("%d files on disk", len(ents))
	}
	st2, _ := OpenFileStore(dir, 3)
	if len(st2.Revisions()) != 3 {
		t.Error("reload")
	}
}

func TestExclusiveLock(t *testing.T) {
	r := newRig(t)
	x := r.session("alice", SuperUser, Exclusive)
	if _, _, err := r.e.Configure("bob", SuperUser, Shared); !errors.As(err, new(*LockedError)) {
		t.Errorf("shared configure during lock: %v", err)
	}
	if _, _, err := r.e.Configure("bob", SuperUser, Exclusive); !errors.As(err, new(*LockedError)) {
		t.Errorf("second exclusive: %v", err)
	}
	p := r.session("bob", SuperUser, Private)
	setLines(t, p, "set system host-name bob")
	if _, err := p.Commit(context.Background(), CommitOptions{}); !errors.As(err, new(*LockedError)) {
		t.Errorf("private commit during lock: %v", err)
	}
	setLines(t, x, "set system host-name alice")
	if !x.Close() {
		t.Error("uncommitted changes not reported")
	}
	s := r.session("carol", SuperUser, Shared)
	if s.Changed() {
		t.Error("exclusive changes not discarded on exit")
	}
}

func TestPrivateSessions(t *testing.T) {
	r := newRig(t)
	sh := r.session("alice", SuperUser, Shared)
	setLines(t, sh, "set system host-name shared")
	if _, _, err := r.e.Configure("bob", SuperUser, Private); err == nil {
		t.Error("private allowed while the shared candidate has changes")
	}
	if _, _, err := r.e.Configure("bob", SuperUser, Exclusive); err == nil {
		t.Error("exclusive allowed while the shared candidate has changes")
	}
	active := r.e.Active()
	sh.Modify(func(c *config.Tree) error { c.Root = active.Root; return nil })

	p1 := r.session("bob", SuperUser, Private)
	p2 := r.session("carol", SuperUser, Private)
	setLines(t, p1, "set vlans v1 vlan-id 11")
	setLines(t, p2, "set vlans v2 vlan-id 12")
	commit(t, p1, CommitOptions{})
	if sh.Changed() {
		t.Error("shared candidate should follow a commit it did not diverge from")
	}
	if _, err := p2.Commit(context.Background(), CommitOptions{}); !errors.As(err, new(OutOfDateError)) {
		t.Fatalf("stale private commit: %v", err)
	}
	if err := p2.Update(); err != nil {
		t.Fatal(err)
	}
	commit(t, p2, CommitOptions{})
	a := r.e.Active()
	if !a.Root.Has("vlans", "v1") || !a.Root.Has("vlans", "v2") {
		t.Errorf("private commits did not combine:\n%s", config.FormatSet(a))
	}
	if err := sh.Update(); err == nil {
		t.Error("update allowed in shared mode")
	}
	st := r.e.Status()
	if len(st) != 3 || st[0].User != "alice" {
		t.Errorf("status: %+v", st)
	}
}

func TestPermissions(t *testing.T) {
	r := newRig(t)
	if _, _, err := r.e.Configure("ro", ReadOnly, Shared); !errors.Is(err, ErrPermission) {
		t.Errorf("read-only configure: %v", err)
	}
	op := r.session("op", Operator, Private)
	setLines(t, op, "set system login user eve class super-user")
	issues := op.Check()
	if !issues.HasErrors() || !strings.Contains(issues.String(), "permission denied") || !strings.Contains(issues.String(), "system login") {
		t.Errorf("operator login change: %v", issues)
	}
	op2 := r.session("op2", Operator, Private)
	setLines(t, op2, "set vlans v vlan-id 5")
	commit(t, op2, CommitOptions{})
}

func TestRollbackCommand(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	for _, h := range []string{"a", "b"} {
		setLines(t, s, "set system host-name "+h)
		commit(t, s, CommitOptions{})
		r.e.Confirm(context.Background(), "alice")
	}
	if err := s.Rollback(1); err != nil {
		t.Fatal(err)
	}
	if s.Candidate().Root.Leaf("system", "host-name") != "a" {
		t.Error("rollback 1 did not load the previous revision")
	}
	if d, _ := s.Compare(0); !strings.Contains(d, "-   host-name b;") || !strings.Contains(d, "+   host-name a;") {
		t.Errorf("compare:\n%s", d)
	}
	if err := s.Rollback(99); !errors.Is(err, ErrNoRevision) {
		t.Errorf("rollback 99: %v", err)
	}
	if err := s.Rollback(0); err != nil || s.Changed() {
		t.Error("rollback 0 did not discard changes")
	}
}

func TestSharedEditsSurviveAutomaticRollback(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	setLines(t, s, "set system host-name a")
	commit(t, s, CommitOptions{})
	setLines(t, s, "set system domain-name example.org")
	r.clock.Advance(11 * time.Minute)
	if s.Candidate().Root.Leaf("system", "domain-name") != "example.org" {
		t.Error("uncommitted shared edit lost by the automatic rollback")
	}
}

func TestClosedSession(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	s.Close()
	if err := s.Modify(func(*config.Tree) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Errorf("modify after close: %v", err)
	}
	if _, err := s.Commit(context.Background(), CommitOptions{}); !errors.Is(err, ErrClosed) {
		t.Errorf("commit after close: %v", err)
	}
	if s.Close() {
		t.Error("double close")
	}
}

func TestConcurrentUse(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for u := 0; u < 4; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			mode := Shared
			if u%2 == 1 {
				mode = Private
			}
			for i := 0; i < 50; i++ {
				s, _, err := r.e.Configure("u", SuperUser, mode)
				if err != nil {
					continue // e.g. shared candidate modified: private refused
				}
				_ = s.Modify(func(c *config.Tree) error {
					return config.ApplySetLines(c, "set system host-name h"+string(rune('a'+u))+string(rune('a'+i%26)))
				})
				if mode == Private {
					_ = s.Update()
				}
				_, _ = s.Commit(ctx, CommitOptions{})
				if i%3 == 0 {
					_ = r.e.Confirm(context.Background(), "u")
				}
				_ = r.e.History()
				_ = r.e.Status()
				s.Close()
			}
		}(u)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			r.clock.Advance(time.Minute)
		}
	}()
	wg.Wait()
	// The engine is still consistent: the active configuration equals the
	// newest revision.
	newest, err := r.e.Revision(0)
	if err != nil || !config.Equal(newest, r.e.Active()) {
		t.Fatalf("active configuration differs from the newest revision: %v", err)
	}
}

// The shared candidate survives a restart; once committed it is no longer
// stored.
func TestSharedCandidatePersists(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	setLines(t, s, "set system host-name kept")
	r.e.Close()
	r.open()
	s = r.session("bob", SuperUser, Shared)
	if s.Candidate().Root.Leaf("system", "host-name") != "kept" {
		t.Fatal("shared candidate lost across restart")
	}
	commit(t, s, CommitOptions{})
	r.e.Confirm(context.Background(), "bob")
	if _, err := os.Stat(filepath.Join(r.dir, candidateFile)); !os.IsNotExist(err) {
		t.Errorf("candidate file left after commit: %v", err)
	}
	// Private candidates are not stored.
	p := r.session("carol", SuperUser, Private)
	setLines(t, p, "set system host-name private")
	if _, err := os.Stat(filepath.Join(r.dir, candidateFile)); !os.IsNotExist(err) {
		t.Errorf("private candidate stored: %v", err)
	}
}

// A member that stops being master ends the configuration sessions; with
// Writable failing, nothing can be configured or committed, and an
// expiring confirmation does not roll back (the master does). Resume
// enforces a deadline that passed meanwhile.
func TestDemoteAndResume(t *testing.T) {
	r := newRig(t)
	s := r.session("alice", SuperUser, Shared)
	setLines(t, s, "set system host-name one")
	commit(t, s, CommitOptions{Confirmed: true, Minutes: 5})
	master := true
	r.e.o.Writable = func() error {
		if !master {
			return errors.New("not master")
		}
		return nil
	}
	master = false
	r.e.Demote("mastership moved")
	if !s.Closed() {
		t.Fatal("session still open after Demote")
	}
	if err := s.Modify(func(*config.Tree) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("modify after demote: %v", err)
	}
	if _, _, err := r.e.Configure("bob", SuperUser, Shared); err == nil {
		t.Fatal("configure without master succeeded")
	}
	if err := r.e.Confirm(context.Background(), "bob"); err == nil {
		t.Fatal("confirm without master succeeded")
	}
	r.clock.Advance(10 * time.Minute) // the (stopped) timer must not roll back
	if r.e.Active().Root.Leaf("system", "host-name") != "one" {
		t.Fatal("rolled back while not master")
	}
	master = true
	r.e.Resume(context.Background())
	if r.e.Active().Root.Leaf("system", "host-name") == "one" || r.e.Pending() != nil {
		t.Fatal("expired confirmation not enforced on resume")
	}
}
