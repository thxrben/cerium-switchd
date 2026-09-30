// Package commit implements the candidate/commit/confirm/rollback model of
// docs/config-reference.md section 4: candidates per session (shared,
// private, exclusive), commit check, commit with apply and revert on
// failure, commit confirmation with automatic rollback, and revisions.
package commit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"mclag/internal/config"
	"mclag/internal/model"
)

// Class is a user's permission class (reference 4.3).
type Class int

const (
	SuperUser Class = iota
	Operator
	ReadOnly
)

// ParseClass converts the configuration value of a login class.
func ParseClass(s string) Class {
	switch s {
	case "super-user":
		return SuperUser
	case "operator":
		return Operator
	}
	return ReadOnly
}

func (c Class) String() string { return [...]string{"super-user", "operator", "read-only"}[c] }

// Mode is the configuration mode of a session.
type Mode int

const (
	Shared Mode = iota
	Private
	Exclusive
)

func (m Mode) String() string { return [...]string{"shared", "private", "exclusive"}[m] }

// MemberResult is the outcome of applying a configuration on one member.
type MemberResult struct {
	Member string
	Err    error
	// Pending means the member is unreachable and applies the
	// configuration when it reconnects.
	Pending bool
}

// Applier makes a configuration effective on all members. from is the
// configuration currently in effect (nil at startup). Implementations get
// full trees (including inactive statements) and use Tree.Active().
type Applier interface {
	Apply(ctx context.Context, from, to *config.Tree) []MemberResult
}

// Timer is a stoppable timer (see Clock).
type Timer interface{ Stop() bool }

// Clock abstracts time for tests.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Options configure an Engine.
type Options struct {
	Store     Store
	Applier   Applier
	Inventory model.Inventory // may be nil
	// Checks are additional commit checks that depend on the host (e.g.
	// OS accounts with the same name as a configured user).
	Checks []func(*model.Config) model.Issues
	Clock  Clock // nil: real time
	// Upgrade converts a stored configuration (JSON) written by an older
	// version, e.g. old interface names. It is applied to everything read
	// from the store before it is parsed: active configuration, revisions
	// and the shared candidate.
	Upgrade func(json.RawMessage) json.RawMessage
	// Notify broadcasts a message to the logged-in CLI sessions. ctx is the
	// context of the operation that caused it (the originating session is
	// identified through it and not notified); automatic events use
	// context.Background().
	Notify func(ctx context.Context, msg string)
	Log    *slog.Logger
	// StackCheck checks a candidate on the other stack members (their
	// hardware and OS); its issues are added to the local ones, without
	// duplicates. nil: standalone.
	StackCheck func(cand *config.Tree) model.Issues
	// Writable reports whether this engine may change the configuration
	// (in a stack: this member is the master). nil: always. It is checked
	// by Configure, Commit and Confirm; an expiring confirmation timer
	// does nothing while it fails (the master rolls back).
	Writable func() error
}

// Errors returned by sessions and the engine.
var (
	ErrPermission    = errors.New("permission denied")
	ErrClosed        = errors.New("configuration session closed")
	ErrCheckFailed   = errors.New("configuration check-out failed")
	ErrApplyFailed   = errors.New("commit failed: configuration could not be applied, previous configuration restored")
	ErrNothingToConf = errors.New("no commit pending confirmation")
	ErrNoRevision    = errors.New("no such revision")
)

// LockedError reports that another user holds the exclusive lock.
type LockedError struct{ User string }

func (e *LockedError) Error() string {
	return fmt.Sprintf("configuration database locked by %s (configure exclusive)", e.User)
}

// OutOfDateError reports that a private candidate is based on an old commit.
type OutOfDateError struct{}

func (OutOfDateError) Error() string {
	return "configuration has been committed by another user since your private candidate was created; run 'update' first"
}

// Engine owns the active configuration, the shared candidate and all
// configuration sessions of one switch (in a stack: of the stack leader).
type Engine struct {
	o Options

	// opMu serialises operations that change the active configuration
	// (commit, confirm, rollback). It is taken before mu.
	opMu sync.Mutex

	mu        sync.Mutex
	active    *config.Tree
	activeSeq uint64
	shared    *config.Tree
	lock      *Session
	sessions  map[*Session]struct{}
	timer     Timer
	gen       uint64 // invalidates stale confirmation timers
	nextID    uint64 // session order

	// The shared candidate is stored in the background (in a stack, it is
	// replicated), newest value first; persistMu keeps the writes in order.
	persistMu   sync.Mutex
	sharedWant  *config.Tree
	sharedDirty bool
	sharedKick  chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
}

// New opens the engine on a store. An empty store gets an empty initial
// revision. Call Start to apply the active configuration.
func New(o Options) (*Engine, error) {
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Notify == nil {
		o.Notify = func(context.Context, string) {}
	}
	if o.Writable == nil {
		o.Writable = func() error { return nil }
	}
	e := &Engine{o: o, sessions: map[*Session]struct{}{}, sharedKick: make(chan struct{}, 1), done: make(chan struct{})}
	revs := o.Store.Revisions()
	if len(revs) == 0 {
		r, err := newRevision(1, o.Clock.Now(), "system", "initial configuration", config.New())
		if err != nil {
			return nil, err
		}
		if err := o.Store.Put(r, 0); err != nil {
			// A store that cannot be written yet (a stack member that has
			// not received the replicated state) starts empty.
			o.Log.Warn("initial configuration not stored", "err", err)
			revs = []*Revision{r}
		} else {
			revs = o.Store.Revisions()
		}
	}
	last := revs[len(revs)-1]
	t, err := e.tree(last)
	if err != nil {
		return nil, err
	}
	e.active, e.activeSeq = t, last.Seq
	e.shared = t.Clone()
	if cs, ok := o.Store.(CandidateStore); ok {
		c, err := e.parse(cs.Candidate())
		switch {
		case err != nil:
			o.Log.Warn("stored shared candidate is unreadable; starting from the active configuration", "err", err)
		case c != nil:
			e.shared = c
		}
	}
	go e.sharedWriter()
	return e, nil
}

func newRevision(seq uint64, now time.Time, user, comment string, t *config.Tree) (*Revision, error) {
	raw, err := json.Marshal(config.ToJSON(t.Root))
	if err != nil {
		return nil, err
	}
	return &Revision{Seq: seq, Time: now.UTC(), User: user, Comment: comment, Config: raw}, nil
}

// Start applies the active configuration. If a commit confirmation expired
// while switchd was not running, the rollback target is applied instead
// (reference 4.2 step 5). Otherwise the confirmation timer is re-armed.
func (e *Engine) Start(ctx context.Context) []MemberResult {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	writable := e.o.Writable() == nil
	if p := e.o.Store.Pending(); p != nil && writable && !e.o.Clock.Now().Before(p.Deadline) {
		return e.rollbackPending(ctx, nil)
	}
	res := e.o.Applier.Apply(ctx, nil, e.Active())
	e.logResults("startup apply", res)
	if p := e.o.Store.Pending(); p != nil && writable {
		e.mu.Lock()
		e.armTimer(p.Deadline)
		e.mu.Unlock()
	}
	return res
}

// Close stops the confirmation timer and stores the shared candidate.
func (e *Engine) Close() {
	e.mu.Lock()
	e.gen++
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	e.mu.Unlock()
	e.closeOnce.Do(func() { close(e.done) })
	e.writeShared()
}

// Reload reads the active configuration and the shared candidate from the
// store again (a stack member that is not master, after the replicated
// state changed).
func (e *Engine) Reload() error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	return e.reload()
}

func (e *Engine) reload() error {
	revs := e.o.Store.Revisions()
	if len(revs) == 0 {
		return nil
	}
	last := revs[len(revs)-1]
	t, err := e.tree(last)
	if err != nil {
		return err
	}
	shared := t.Clone()
	if cs, ok := e.o.Store.(CandidateStore); ok {
		if c, err := e.parse(cs.Candidate()); err == nil && c != nil {
			shared = c
		}
	}
	e.mu.Lock()
	e.active, e.activeSeq = t, last.Seq
	if len(e.sessions) == 0 {
		e.shared = shared
	}
	e.mu.Unlock()
	return nil
}

// Resume takes over as the configuration master: it reloads the stored
// state and enforces a pending confirmation (re-arms the timer, or rolls
// back at once if the deadline has passed).
func (e *Engine) Resume(ctx context.Context) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reload(); err != nil {
		e.o.Log.Error("configuration master: reading the stored configuration", "err", err)
	}
	p := e.o.Store.Pending()
	if p == nil {
		return
	}
	if !e.o.Clock.Now().Before(p.Deadline) {
		e.rollbackPending(ctx, e.Active())
		return
	}
	e.mu.Lock()
	e.armTimer(p.Deadline)
	e.mu.Unlock()
}

// Demote ends all configuration sessions (this member is no longer the
// configuration master); msg is announced to the CLI sessions.
func (e *Engine) Demote(msg string) {
	e.mu.Lock()
	n := len(e.sessions)
	for s := range e.sessions {
		s.closed = true
		if s.Mode == Private {
			s.private = nil
		}
	}
	e.sessions = map[*Session]struct{}{}
	e.lock = nil
	e.gen++
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	e.mu.Unlock()
	if n > 0 && msg != "" {
		e.o.Notify(context.Background(), msg)
	}
}

// Active returns a copy of the active configuration.
func (e *Engine) Active() *config.Tree {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active.Clone()
}

// PendingInfo describes a commit waiting for confirmation.
type PendingInfo struct {
	Deadline    time.Time
	Target      uint64 // rollback target (last confirmed revision)
	First, Last uint64 // unconfirmed revisions
}

// Pending returns the pending confirmation, or nil.
func (e *Engine) Pending() *PendingInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.o.Store.Pending()
	if p == nil {
		return nil
	}
	return &PendingInfo{Deadline: p.Deadline, Target: p.Target, First: p.First, Last: e.activeSeq}
}

// RevisionInfo is one line of "show system commit".
type RevisionInfo struct {
	Number    int // 0 = active
	Seq       uint64
	Time      time.Time
	User      string
	Comment   string
	Confirmed bool
}

// History returns the revisions, newest (number 0) first.
func (e *Engine) History() []RevisionInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	revs := e.o.Store.Revisions()
	p := e.o.Store.Pending()
	out := make([]RevisionInfo, 0, len(revs))
	for i := len(revs) - 1; i >= 0; i-- {
		r := revs[i]
		out = append(out, RevisionInfo{
			Number: len(out), Seq: r.Seq, Time: r.Time, User: r.User, Comment: r.Comment,
			Confirmed: p == nil || r.Seq < p.First,
		})
	}
	return out
}

// Revision returns revision n (0 = active configuration).
func (e *Engine) Revision(n int) (*config.Tree, error) {
	e.mu.Lock()
	revs := e.o.Store.Revisions()
	e.mu.Unlock()
	if n < 0 || n >= len(revs) {
		return nil, ErrNoRevision
	}
	return e.tree(revs[len(revs)-1-n])
}

// tree decodes a stored revision, upgrading it first.
func (e *Engine) tree(r *Revision) (*config.Tree, error) {
	t, err := e.parse(r.Config, nil)
	if err != nil {
		return nil, fmt.Errorf("revision %d: %w", r.Seq, err)
	}
	return t, nil
}

// parse decodes a stored configuration (nil raw: none).
func (e *Engine) parse(raw json.RawMessage, err error) (*config.Tree, error) {
	if err != nil || raw == nil {
		return nil, err
	}
	if e.o.Upgrade != nil {
		raw = e.o.Upgrade(raw)
	}
	return config.FromJSON(raw)
}

// SessionInfo describes a configuration session for "status".
type SessionInfo struct {
	User    string
	Mode    Mode
	Since   time.Time
	Changed bool // private candidates only
}

// Status lists the users editing the configuration.
func (e *Engine) Status() []SessionInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	ss := make([]*Session, 0, len(e.sessions))
	for s := range e.sessions {
		ss = append(ss, s)
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].id < ss[j].id })
	var out []SessionInfo
	for _, s := range ss {
		si := SessionInfo{User: s.User, Mode: s.Mode, Since: s.since}
		if s.Mode == Private {
			si.Changed = !config.Equal(s.private, s.baseTree)
		}
		out = append(out, si)
	}
	return out
}

// Session is one user's configuration mode.
type Session struct {
	e     *Engine
	User  string
	Class Class
	Mode  Mode
	since time.Time
	id    uint64

	// Private mode: the private candidate and the commit it is based on.
	private  *config.Tree
	base     uint64
	baseTree *config.Tree
	closed   bool
}

// Configure enters configuration mode. The returned messages are shown to
// the user (other editors, uncommitted changes).
func (e *Engine) Configure(user string, class Class, mode Mode) (*Session, []string, error) {
	if class == ReadOnly {
		return nil, nil, ErrPermission
	}
	if err := e.o.Writable(); err != nil {
		return nil, nil, fmt.Errorf("configuration unavailable: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	sharedChanged := !config.Equal(e.shared, e.active)
	if e.lock != nil && mode != Private {
		return nil, nil, &LockedError{User: e.lock.User}
	}
	switch mode {
	case Private:
		if sharedChanged {
			return nil, nil, errors.New("shared configuration database modified; 'configure private' is not possible until those changes are committed or discarded")
		}
	case Exclusive:
		if sharedChanged {
			return nil, nil, errors.New("shared configuration database modified; 'configure exclusive' would discard those changes")
		}
	}
	var msgs []string
	var others []string
	for o := range e.sessions {
		others = append(others, fmt.Sprintf("%s (%s)", o.User, o.Mode))
	}
	if len(others) > 0 {
		sort.Strings(others)
		msgs = append(msgs, "users currently editing the configuration: "+strings.Join(others, ", "))
	}
	if mode == Shared && sharedChanged {
		msgs = append(msgs, "the configuration has been changed but not committed")
	}
	e.nextID++
	s := &Session{e: e, User: user, Class: class, Mode: mode, since: e.o.Clock.Now(), id: e.nextID}
	switch mode {
	case Private:
		s.private = e.active.Clone()
		s.base, s.baseTree = e.activeSeq, e.active.Clone()
	case Exclusive:
		e.lock = s
	}
	e.sessions[s] = struct{}{}
	return s, msgs, nil
}

// candidate returns the session's candidate tree. Caller holds e.mu.
func (s *Session) candidate() *config.Tree {
	if s.Mode == Private {
		return s.private
	}
	return s.e.shared
}

// Closed reports whether the session ended (it was closed, or the engine
// ended it because mastership moved).
func (s *Session) Closed() bool {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	return s.closed
}

// Candidate returns a copy of the session's candidate configuration.
func (s *Session) Candidate() *config.Tree {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	return s.candidate().Clone()
}

// Modify changes the candidate atomically: fn works on a copy, which
// replaces the candidate only if fn succeeds.
func (s *Session) Modify(fn func(t *config.Tree) error) error {
	e := s.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.Mode != Private && e.lock != nil && e.lock != s {
		return &LockedError{User: e.lock.User}
	}
	work := s.candidate().Clone()
	if err := fn(work); err != nil {
		return err
	}
	if s.Mode == Private {
		s.private = work
	} else {
		e.shared = work
		e.persistShared()
	}
	return nil
}

// Changed reports whether the candidate differs from the active
// configuration.
func (s *Session) Changed() bool {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	return !config.Equal(s.candidate(), s.e.active)
}

// Compare shows the difference between revision n and the candidate.
func (s *Session) Compare(n int) (string, error) {
	old, err := s.e.Revision(n)
	if err != nil {
		return "", err
	}
	return config.Diff(old, s.Candidate()), nil
}

// Rollback replaces the candidate with revision n. It still has to be
// committed.
func (s *Session) Rollback(n int) error {
	t, err := s.e.Revision(n)
	if err != nil {
		return err
	}
	return s.Modify(func(c *config.Tree) error {
		c.Root = t.Root
		return nil
	})
}

// Update rebases a private candidate onto the latest commit: the changes
// made in the private candidate are replayed on the active configuration.
func (s *Session) Update() error {
	if s.Mode != Private {
		return errors.New("'update' is only available in 'configure private'")
	}
	e := s.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	work := e.active.Clone()
	if err := config.ApplySetLines(work, config.Patch(s.baseTree, s.private)); err != nil {
		return fmt.Errorf("update failed, private candidate unchanged: %w", err)
	}
	s.private = work
	s.base, s.baseTree = e.activeSeq, e.active.Clone()
	return nil
}

// Close leaves configuration mode. Private and exclusive candidates are
// discarded. It reports whether uncommitted changes were left behind (in
// the shared candidate) or discarded.
func (s *Session) Close() (uncommitted bool) {
	e := s.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.closed {
		return false
	}
	s.closed = true
	delete(e.sessions, s)
	uncommitted = !config.Equal(s.candidate(), e.active)
	switch s.Mode {
	case Exclusive:
		e.lock = nil
		e.shared = e.active.Clone()
		e.persistShared()
	case Private:
		s.private = nil
	}
	return uncommitted
}

// forbidden lists the hierarchies an operator may not change (4.3).
var forbidden = [][]string{{"system", "login"}, {"system", "services"}, {"virtual-chassis"}}

// check validates cand for this session: permissions and model rules.
// Caller must not hold e.mu.
func (s *Session) check(cand, active *config.Tree) (*model.Config, model.Issues) {
	cfg, issues := model.Build(cand, s.e.o.Inventory)
	for _, chk := range s.e.o.Checks {
		issues = append(issues, chk(cfg)...)
	}
	if s.e.o.StackCheck != nil {
		seen := map[model.Issue]bool{}
		for _, i := range issues {
			seen[i] = true
		}
		for _, i := range s.e.o.StackCheck(cand) {
			if !seen[i] {
				seen[i] = true
				issues = append(issues, i)
			}
		}
	}
	if s.Class == Operator {
		for _, p := range forbidden {
			if !config.NodeEqual(active.Root.Get(p...), cand.Root.Get(p...)) {
				issues = append(model.Issues{{Severity: model.Error, Path: strings.Join(p, " "),
					Msg: "permission denied: class operator may not change this hierarchy"}}, issues...)
			}
		}
	}
	return cfg, issues
}

// Check runs commit check on the candidate without changing anything.
func (s *Session) Check() model.Issues {
	s.e.mu.Lock()
	cand, active := s.candidate().Clone(), s.e.active.Clone()
	s.e.mu.Unlock()
	_, issues := s.check(cand, active)
	return issues
}

// CommitOptions are the variants of the commit command.
type CommitOptions struct {
	Comment   string
	Confirmed bool // commit confirmed
	Minutes   int  // commit confirmed <minutes>; 0 = configured timeout
}

// Result describes the outcome of a commit.
type Result struct {
	Seq       uint64
	Issues    model.Issues // warnings (or errors when the check failed)
	Members   []MemberResult
	Reverted  []MemberResult // results of restoring the previous configuration
	NoChanges bool
	// Confirmed is set when the commit confirmed pending commits.
	Confirmed bool
	// Deadline is set when the commit must be confirmed.
	Deadline time.Time
}

// Commit validates the candidate, applies it on all members and stores it
// as a new revision (reference 4.1 and 4.2).
func (s *Session) Commit(ctx context.Context, opts CommitOptions) (*Result, error) {
	e := s.e
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.o.Writable(); err != nil {
		return nil, fmt.Errorf("commit not possible: %w", err)
	}
	// The shared candidate as of this commit is stored before the commit
	// returns (writes are ordered, the newest value wins).
	defer e.writeShared()

	e.mu.Lock()
	if s.closed {
		e.mu.Unlock()
		return nil, ErrClosed
	}
	if e.lock != nil && e.lock != s {
		e.mu.Unlock()
		return nil, &LockedError{User: e.lock.User}
	}
	if s.Mode == Private && s.base != e.activeSeq {
		e.mu.Unlock()
		return nil, OutOfDateError{}
	}
	cand, old, oldSeq := s.candidate().Clone(), e.active.Clone(), e.activeSeq
	pending := e.o.Store.Pending()
	e.mu.Unlock()

	if config.Equal(cand, old) {
		res := &Result{Seq: oldSeq, NoChanges: true}
		if pending != nil {
			if err := e.confirm(ctx, s.User); err != nil {
				return nil, err
			}
			res.Confirmed = true
		}
		return res, nil
	}

	newCfg, issues := s.check(cand, old)
	res := &Result{Issues: issues}
	if issues.HasErrors() {
		return res, ErrCheckFailed
	}
	oldCfg, _ := model.Build(old, nil)

	// The stricter of the old and new policy applies (4.2 step 7), and a
	// commit during a pending confirmation restarts the timer (step 3).
	needConfirm := opts.Confirmed || pending != nil ||
		oldCfg.System.Commit.ConfirmRequired || newCfg.System.Commit.ConfirmRequired
	minutes := opts.Minutes
	if minutes <= 0 {
		minutes = newCfg.System.Commit.TimeoutMinutes
	}

	res.Members = e.o.Applier.Apply(ctx, old, cand)
	if failed(res.Members) {
		res.Reverted = e.o.Applier.Apply(ctx, cand, old)
		e.logResults("commit failed, revert", res.Reverted)
		return res, ErrApplyFailed
	}

	seq := oldSeq + 1
	now := e.o.Clock.Now()
	rev, err := newRevision(seq, now, s.User, opts.Comment, cand)
	if err == nil && needConfirm {
		np := &Pending{Deadline: now.Add(time.Duration(minutes) * time.Minute), Target: oldSeq, First: seq}
		if pending != nil {
			np.Target, np.First = pending.Target, pending.First
		}
		// The pending state is written before the revision it covers.
		if err = e.o.Store.SetPending(np); err == nil {
			res.Deadline = np.Deadline
		}
	}
	var keep uint64
	if p := e.o.Store.Pending(); p != nil {
		keep = p.Target
	}
	if err == nil {
		err = e.o.Store.Put(rev, keep)
	}
	if err != nil {
		// Nothing was stored as active: restore the previous state.
		_ = e.o.Store.SetPending(pending)
		res.Reverted = e.o.Applier.Apply(ctx, cand, old)
		return res, fmt.Errorf("commit failed: cannot store revision: %w", err)
	}

	e.mu.Lock()
	e.active, e.activeSeq = cand.Clone(), seq
	if s.Mode == Private {
		s.base, s.baseTree = seq, cand.Clone()
	}
	// Candidates without uncommitted edits follow the commit. Edits made to
	// the shared candidate by others (or during the apply) are kept.
	if config.Equal(e.shared, cand) || config.Equal(e.shared, old) {
		e.shared = cand.Clone()
	}
	for o := range e.sessions {
		if o != s && o.Mode == Private && config.Equal(o.private, o.baseTree) {
			o.private, o.base, o.baseTree = cand.Clone(), seq, cand.Clone()
		}
	}
	if needConfirm {
		e.armTimer(res.Deadline)
	}
	e.persistShared()
	e.mu.Unlock()

	res.Seq = seq
	note := fmt.Sprintf("%s: commit complete (revision %d)", s.User, seq)
	if !res.Deadline.IsZero() {
		note += fmt.Sprintf(", must be confirmed within %d minutes", minutes)
	}
	if opts.Comment != "" {
		note += fmt.Sprintf(": %q", opts.Comment)
	}
	e.o.Notify(ctx, note)
	e.o.Log.Info("commit", "facility", "change-log", "revision", seq, "user", s.User, "comment", opts.Comment,
		"confirm_by", res.Deadline, "diff", config.Diff(old, cand))
	return res, nil
}

func failed(rs []MemberResult) bool {
	for _, r := range rs {
		if r.Err != nil {
			return true
		}
	}
	return false
}

func (e *Engine) logResults(what string, rs []MemberResult) {
	for _, r := range rs {
		if r.Err != nil {
			e.o.Log.Error(what, "member", r.Member, "err", r.Err)
		}
	}
}

// Confirm makes all pending commits permanent.
func (e *Engine) Confirm(ctx context.Context, user string) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.o.Writable(); err != nil {
		return fmt.Errorf("confirm not possible: %w", err)
	}
	return e.confirm(ctx, user)
}

// confirm requires opMu.
func (e *Engine) confirm(ctx context.Context, user string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.o.Store.Pending()
	if p == nil {
		return ErrNothingToConf
	}
	if err := e.o.Store.SetPending(nil); err != nil {
		return err
	}
	e.gen++
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	revs := fmt.Sprintf("%d–%d", p.First, e.activeSeq)
	if p.First == e.activeSeq {
		revs = fmt.Sprintf("revision %d", p.First)
	} else {
		revs = "revisions " + revs
	}
	e.o.Log.Info("commit confirmed", "facility", "change-log", "user", user, "revisions", revs)
	e.o.Notify(ctx, fmt.Sprintf("%s: commit confirmed (%s)", user, revs))
	return nil
}

// armTimer (re)starts the confirmation timer. Caller holds e.mu.
func (e *Engine) armTimer(deadline time.Time) {
	e.gen++
	gen := e.gen
	if e.timer != nil {
		e.timer.Stop()
	}
	d := deadline.Sub(e.o.Clock.Now())
	if d < 0 {
		d = 0
	}
	e.timer = e.o.Clock.AfterFunc(d, func() { e.expire(gen) })
}

// expire runs when the confirmation timer fires.
func (e *Engine) expire(gen uint64) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	p := e.o.Store.Pending()
	stale := gen != e.gen || p == nil
	if !stale && e.o.Clock.Now().Before(p.Deadline) {
		e.armTimer(p.Deadline) // fired early
		stale = true
	}
	e.mu.Unlock()
	if stale || e.o.Writable() != nil {
		return
	}
	e.rollbackPending(context.Background(), e.Active())
}

// rollbackPending returns to the last confirmed revision and stores that as
// a new revision (4.2 step 4). from is the configuration in effect (nil at
// startup). Requires opMu.
func (e *Engine) rollbackPending(ctx context.Context, from *config.Tree) []MemberResult {
	e.mu.Lock()
	p := e.o.Store.Pending()
	var target *config.Tree
	for _, r := range e.o.Store.Revisions() {
		if r.Seq == p.Target {
			target, _ = e.tree(r)
		}
	}
	oldActive, lastSeq := e.active, e.activeSeq
	e.mu.Unlock()
	if target == nil {
		// The store guarantees the target exists; never leave a pending state
		// that cannot be resolved.
		e.o.Log.Error("rollback target missing, keeping current configuration", "target", p.Target)
		_ = e.o.Store.SetPending(nil)
		return nil
	}

	res := e.o.Applier.Apply(ctx, from, target)
	e.logResults("automatic rollback", res)
	comment := fmt.Sprintf("automatic rollback: revisions %d–%d not confirmed", p.First, lastSeq)
	seq := lastSeq + 1
	rev, err := newRevision(seq, e.o.Clock.Now(), "system", comment, target)
	if err == nil {
		err = e.o.Store.Put(rev, p.Target)
	}
	if err == nil {
		err = e.o.Store.SetPending(nil)
	}
	if err != nil {
		e.o.Log.Error("automatic rollback: cannot store revision", "err", err)
	}

	e.mu.Lock()
	e.active, e.activeSeq = target.Clone(), seq
	if config.Equal(e.shared, oldActive) {
		e.shared = target.Clone()
	}
	for s := range e.sessions {
		if s.Mode == Private && config.Equal(s.private, s.baseTree) {
			s.private = target.Clone()
			s.base, s.baseTree = seq, target.Clone()
		}
	}
	e.gen++
	e.timer = nil
	e.persistShared()
	e.mu.Unlock()

	e.o.Log.Warn(comment, "facility", "change-log", "revision", seq, "target", p.Target)
	e.o.Notify(context.Background(), comment)
	return res
}

// persistShared queues the shared candidate for storing if it has
// uncommitted changes (else the stored one is removed). Caller holds e.mu.
func (e *Engine) persistShared() {
	if _, ok := e.o.Store.(CandidateStore); !ok {
		return
	}
	var t *config.Tree
	if !config.Equal(e.shared, e.active) {
		t = e.shared.Clone()
	}
	e.sharedWant, e.sharedDirty = t, true
	select {
	case e.sharedKick <- struct{}{}:
	default:
	}
}

func (e *Engine) sharedWriter() {
	for {
		select {
		case <-e.sharedKick:
			e.writeShared()
		case <-e.done:
			return
		}
	}
}

// writeShared stores the newest queued shared candidate. Caller must not
// hold e.mu.
func (e *Engine) writeShared() {
	cs, ok := e.o.Store.(CandidateStore)
	if !ok {
		return
	}
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	e.mu.Lock()
	t, dirty := e.sharedWant, e.sharedDirty
	e.sharedWant, e.sharedDirty = nil, false
	e.mu.Unlock()
	if !dirty {
		return
	}
	if err := cs.SetCandidate(t); err != nil {
		e.o.Log.Warn("cannot store the shared candidate", "err", err)
	}
}
