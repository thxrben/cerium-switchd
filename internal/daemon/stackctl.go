package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/stack"
	"github.com/thxrben/cerium-switchd/internal/stack/control"
	"github.com/thxrben/cerium-switchd/internal/version"
	"github.com/thxrben/cerium-switchd/pkg/sdnotify"
)

// Stack apply timeouts (docs/stack-protocol.md, "Commits in a stack").
const (
	memberApplyTimeout = 120 * time.Second
	catchUpGrace       = 60 * time.Second
	catchUpInterval    = 10 * time.Second
)

// stackCtl connects the commit engine to the stack control: the master
// applies commits on every member, the others catch up from the
// replicated state.
type stackCtl struct {
	node   *control.Node
	vc     *stack.Manager
	local  *kernelApplier
	member int
	live   *sdnotify.Liveness
	log    *slog.Logger
	// stateDir holds the configuration store (rewritten when this member
	// leaves the stack).
	stateDir string

	// restart ends switchd so that systemd starts it again.
	restart func()
	// onLeader runs when this member becomes or stops being master.
	onLeader func(isMaster bool)
	// inv and checks are this member's commit checks (hardware, OS).
	inv    model.Inventory
	checks []func(*model.Config) model.Issues

	mu        sync.Mutex
	engine    *commit.Engine
	lastApply time.Time // last apply sent by the master
	listed    bool      // this member was seen in the member list
	left      bool
	settle    *time.Timer
	ports     portCache
}

func (s *stackCtl) eng() *commit.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine
}

// startControl starts the stack control on the configuration store. It
// returns nil (and logs) if it cannot run; switchd then works standalone.
func startControl(stateDir string, store *commit.FileStore, vc *stack.Manager, local *kernelApplier, member int, log *slog.Logger, restart func()) *stackCtl {
	s := &stackCtl{stateDir: stateDir, vc: vc, local: local, member: member, log: log, restart: restart}
	n := &control.Node{Self: member, SelfKey: vc.PublicKey(), Dir: filepath.Join(stateDir, "stack", "raft"), Store: store,
		MetaFile: filepath.Join(stateDir, "stack", "control.json"), Mesh: vc.Mesh(), Founder: vc.Founder(), Log: log,
		Priority: func(id int) int {
			if e := s.eng(); e != nil {
				return mastershipPriority(e.Active(), id)
			}
			return 128
		},
		OnChange: s.changed,
		OnLeader: s.leader,
	}
	if err := n.Start(); err != nil {
		log.Error("stack control cannot start; this switch works standalone", "err", err)
		return nil
	}
	s.node = n
	vc.Control = n
	_, s.listed = n.Members()[member]
	n.Handle("apply", s.applyFromMaster)
	n.Handle("check", s.checkForMaster)
	if len(n.Members()) <= 1 && vc.Founder() {
		// A new or one-member stack: wait briefly so the initial
		// configuration can be stored and commits work right away.
		// (With other members, the election needs the stacking sessions,
		// which start later.)
		deadline := time.Now().Add(5 * time.Second)
		for !n.MasterReady() && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	return s
}

func mastershipPriority(t *config.Tree, id int) int {
	if t.Active().Root.Leaf("virtual-chassis", "member", strconv.Itoa(id), "role") == "witness" {
		return 0 // never master
	}
	v := t.Active().Root.Leaf("virtual-chassis", "member", strconv.Itoa(id), "mastership-priority")
	if p, err := strconv.Atoi(v); err == nil {
		return p
	}
	return 128
}

func (s *stackCtl) setEngine(e *commit.Engine) {
	s.mu.Lock()
	s.engine = e
	s.mu.Unlock()
}

// writable is the engine's Writable: only the master changes the
// configuration.
func (s *stackCtl) writable() error {
	if s.node.MasterReady() {
		return nil
	}
	if m := s.node.Master(); m != 0 && m != s.member {
		return fmt.Errorf("member %d is master", m)
	}
	return control.ErrNoMaster
}

// changed runs after the replicated state changed. It is acted upon (shown,
// applied, a removal) once it is current (see control.Node.Current): a
// member replaying the log after a restart or join must not pass through
// old revisions.
func (s *stackCtl) changed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settle == nil {
		s.settle = time.AfterFunc(settleDelay, s.settled)
	} else {
		s.settle.Reset(settleDelay)
	}
}

const settleDelay = 600 * time.Millisecond

func (s *stackCtl) settled() {
	if !s.node.Current() {
		d := settleDelay
		if s.node.Master() == 0 {
			d = 2 * time.Second
		}
		s.mu.Lock()
		s.settle.Reset(d)
		s.mu.Unlock()
		return
	}
	s.reload()
	s.checkRemoved()
	s.catchUp(false)
}

// reload shows the replicated configuration (members that are not master).
func (s *stackCtl) reload() {
	if e := s.eng(); e != nil && !s.node.IsMaster() {
		if err := e.Reload(); err != nil {
			s.log.Error("stack: replicated configuration unreadable", "err", err)
		}
	}
}

// checkRemoved makes this switch a stack of its own once it sees itself
// removed from the member list (docs/stack-protocol.md, removal).
func (s *stackCtl) checkRemoved() {
	members := s.node.Members()
	_, in := members[s.member]
	s.mu.Lock()
	defer s.mu.Unlock()
	if in {
		s.listed = true
		return
	}
	if !s.listed || len(members) == 0 || s.left {
		return
	}
	s.left = true
	// This member becomes member 1 of a stack of its own: its configuration
	// is rewritten first (ports renumbered, stacking and MC-LAG removed),
	// then the stack keys are replaced, so a failure in between leaves a
	// member that still finds itself removed on the next start.
	if e := s.engine; e != nil { // s.mu is held
		var m map[string]any
		raw, _ := json.Marshal(config.ToJSON(e.Active().Root))
		if json.Unmarshal(raw, &m) == nil {
			(&standalone{from: strconv.Itoa(s.member), log: s.log}).Rewrite(m)
			raw, _ = json.Marshal(m)
			if err := replaceConfigAs(s.stateDir, "config.pre-leave-",
				fmt.Sprintf("left the virtual chassis (was member %d, now member 1)", s.member), raw); err != nil {
				s.log.Error("stack: rewriting the configuration after removal failed", "err", err)
				return
			}
		}
	}
	if err := s.vc.Leave(); err != nil {
		s.log.Error("stack: leaving after removal failed", "err", err)
		return
	}
	go func() {
		time.Sleep(2 * time.Second) // let the removal finish on the others
		s.restart()
	}()
}

// catchUp applies the replicated active configuration if this member runs
// a different one and no commit is in progress (periodic: only after the
// grace period since the master's last apply).
func (s *stackCtl) catchUp(periodic bool) {
	e := s.eng()
	if e == nil || s.node.IsMaster() {
		return
	}
	s.mu.Lock()
	recent := time.Since(s.lastApply) < catchUpGrace
	s.mu.Unlock()
	if recent {
		return
	}
	want := e.Active()
	s.local.mu.Lock()
	same := s.local.last != nil && config.Equal(s.local.last, want)
	s.local.mu.Unlock()
	if same {
		return
	}
	s.log.Info("stack: applying the replicated configuration (this member missed a commit)", "periodic", periodic)
	for _, r := range s.local.Apply(context.Background(), nil, want) {
		if r.Err != nil {
			s.log.Error("stack: catch-up apply failed", "err", r.Err)
		}
	}
}

func (s *stackCtl) run(ctx context.Context) {
	t := time.NewTicker(catchUpInterval)
	defer t.Stop()
	defer s.live.Forget("stack control")
	s.live.Beat("stack control")
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.node.Current() {
				s.checkRemoved()
				s.catchUp(true)
			}
			s.live.Beat("stack control")
		}
	}
}

// leader runs when this member becomes or stops being master.
func (s *stackCtl) leader(isMaster bool) {
	if s.onLeader != nil {
		s.onLeader(isMaster)
	}
	e := s.eng()
	if e == nil {
		return
	}
	if isMaster {
		e.Resume(context.Background())
		return
	}
	e.Demote("mastership moved to another member; configuration mode ended (the shared candidate is kept)")
}

type applyRequest struct {
	Config json.RawMessage `json:"config"`
}

// applyFromMaster applies a configuration sent by the master.
func (s *stackCtl) applyFromMaster(from int, req json.RawMessage) (any, error) {
	// A member that lost track of the master for a moment (a stacking
	// cable failed) still accepts it; a stale master without majority
	// cannot store its commit anyway and reverts it.
	if m := s.node.Master(); m != 0 && from != m {
		return nil, fmt.Errorf("apply from member %d, but the master is member %d", from, m)
	}
	var r applyRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return nil, err
	}
	t, err := config.FromJSON(newUpgrader(s.local.names, s.member, s.log).Upgrade(r.Config))
	if err != nil {
		return nil, fmt.Errorf("configuration from the master: %w", err)
	}
	s.mu.Lock()
	s.lastApply = time.Now()
	s.mu.Unlock()
	res := s.local.Apply(context.Background(), nil, t)
	s.mu.Lock()
	s.lastApply = time.Now()
	s.mu.Unlock()
	for _, x := range res {
		if x.Err != nil {
			return nil, x.Err
		}
	}
	return nil, nil
}

// Apply implements commit.Applier: this member, and every other member
// when this member is master.
func (s *stackCtl) Apply(ctx context.Context, from, to *config.Tree) []commit.MemberResult {
	res := s.local.Apply(ctx, from, to)
	if !s.node.MasterReady() {
		return res
	}
	raw, err := json.Marshal(config.ToJSON(to.Root))
	if err != nil {
		return append(res, commit.MemberResult{Member: "stack", Err: err})
	}
	reach := map[int]bool{}
	for _, id := range s.node.Mesh.Reachable() {
		reach[id] = true
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for id := range s.node.Members() {
		if id == s.member {
			continue
		}
		if !reach[id] {
			res = append(res, commit.MemberResult{Member: memberName(id), Pending: true})
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.node.Call(id, "apply", applyRequest{Config: raw}, memberApplyTimeout)
			var re *control.RemoteError
			if err != nil && !errors.As(err, &re) {
				err = fmt.Errorf("not reachable: %w", err)
			}
			mu.Lock()
			res = append(res, commit.MemberResult{Member: memberName(id), Err: err})
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(res, func(i, j int) bool { return naturalMember(res[i].Member) < naturalMember(res[j].Member) })
	return res
}

func naturalMember(n string) int {
	var id int
	fmt.Sscanf(n, "member%d", &id)
	return id
}

// checkForMaster runs this member's checks on a candidate.
func (s *stackCtl) checkForMaster(from int, req json.RawMessage) (any, error) {
	var r applyRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return nil, err
	}
	unknown := UnknownStatements(r.Config)
	t, err := config.FromJSON(newUpgrader(s.local.names, s.member, nil).Upgrade(r.Config))
	if err != nil {
		return nil, err
	}
	cfg, issues := model.Build(t, s.inv)
	for _, u := range unknown {
		if strings.HasPrefix(u, "switch-options vxlan") || strings.Contains(u, " vxlan") {
			// VXLAN moves the stack tunnels to another port: a member that
			// does not know it would keep the old one (reference 5.7).
			issues = append(issues, model.Issue{Severity: model.Error, Path: u,
				Msg: fmt.Sprintf("member %d (%s) does not support VXLAN; update it first", s.member, version.Version)})
			continue
		}
		issues = append(issues, model.Issue{Severity: model.Warning, Path: u,
			Msg: fmt.Sprintf("not supported by the version of member %d (%s); ignored there until it is updated", s.member, version.Version)})
	}
	for _, chk := range s.checks {
		issues = append(issues, chk(cfg)...)
	}
	if issues == nil {
		issues = model.Issues{}
	}
	return issues, nil
}

// stackCheck asks every other reachable member to check a candidate
// (commit.Options.StackCheck). Issues that only concern one member name it.
func (s *stackCtl) stackCheck(cand *config.Tree) model.Issues {
	if !s.node.MasterReady() {
		return nil
	}
	raw, err := json.Marshal(config.ToJSON(cand.Root))
	if err != nil {
		return nil
	}
	reach := map[int]bool{}
	for _, id := range s.node.Mesh.Reachable() {
		reach[id] = true
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	byMember := map[int]model.Issues{}
	for id := range s.node.Members() {
		if id == s.member || !reach[id] {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.node.Call(id, "check", applyRequest{Config: raw}, 30*time.Second)
			var is model.Issues
			if err != nil {
				is = model.Issues{{Severity: model.Warning, Msg: fmt.Sprintf("the checks of this member could not run: %v", err)}}
			} else if err := json.Unmarshal(res, &is); err != nil {
				return
			}
			mu.Lock()
			byMember[id] = is
			mu.Unlock()
		}()
	}
	wg.Wait()
	// The same issue on every member is the configuration's, not a member's.
	count := map[model.Issue]int{}
	for _, is := range byMember {
		for _, i := range is {
			count[i]++
		}
	}
	var out model.Issues
	ids := make([]int, 0, len(byMember))
	for id := range byMember {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	shared := map[model.Issue]bool{}
	for _, id := range ids {
		for _, i := range byMember[id] {
			if count[i] == len(byMember) && len(byMember) > 1 {
				if !shared[i] {
					shared[i] = true
					out = append(out, i)
				}
				continue
			}
			if prefix := fmt.Sprintf("member %d ", id); !strings.HasPrefix(i.Msg, prefix) {
				i.Msg = fmt.Sprintf("member %d: %s", id, i.Msg)
			}
			out = append(out, i)
		}
	}
	return out
}

// role is this member's role for the CLI banner ("" in a one-member stack).
func (s *stackCtl) role() string {
	members := s.node.Members()
	if len(members) <= 1 {
		return ""
	}
	master := s.node.Master()
	switch {
	case master == s.member:
		return fmt.Sprintf("master:%d", s.member)
	case master == 0:
		return fmt.Sprintf("no-master:%d", s.member)
	}
	// backup: the voter with the highest priority after the master.
	e := s.eng()
	backup, bp := 0, -1
	for _, sv := range s.node.Servers() {
		if !sv.Voter || sv.Member == master {
			continue
		}
		p := 128
		if e != nil {
			p = mastershipPriority(e.Active(), sv.Member)
		}
		if p > bp || (p == bp && sv.Member < backup) {
			backup, bp = sv.Member, p
		}
	}
	if backup == s.member {
		return fmt.Sprintf("backup:%d", s.member)
	}
	return fmt.Sprintf("linecard:%d", s.member)
}

// ---- operational commands on other members ----

type execRequest struct {
	User      string `json:"user"`
	Class     string `json:"class"`
	Line      string `json:"line"`
	Confirmed bool   `json:"confirmed,omitempty"`
}

// serveExec runs operational commands for users of other members
// (reference 5.2, targets). env builds the CLI environment of a user.
func (s *stackCtl) serveExec(env func(user string, class commit.Class) cli.Env) {
	s.node.Handle("exec", func(from int, req json.RawMessage) (any, error) {
		var r execRequest
		if err := json.Unmarshal(req, &r); err != nil {
			return nil, err
		}
		e := env(r.User, commit.ParseClass(r.Class))
		e.Stack, e.Role = nil, nil // no further hops
		sh := cli.New(e)
		defer sh.Close()
		sh.SetPlainErrors() // the section on the other member shows no command line
		s.log.Info("cli command", "facility", "interactive-commands", "user", r.User, "command", r.Line, "from_member", from)
		ctx, cancel := context.WithTimeout(context.Background(), memberExecTimeout)
		defer cancel()
		rep := sh.Execute(ctx, r.Line, remoteTerm{r.Confirmed})
		return rep.Output, nil
	})
}

const memberExecTimeout = 60 * time.Second

// remoteTerm is the terminal of a command run for another member: it can
// only answer the command's confirmation (asked on that member already).
type remoteTerm struct{ confirmed bool }

var errNoTerminal = errors.New("not available when run on another member")

func (t remoteTerm) Ask(string, bool) (string, error) {
	if t.confirmed {
		return "yes", nil
	}
	return "", errNoTerminal
}
func (remoteTerm) ReadText(string) (string, error) { return "", errNoTerminal }
func (remoteTerm) ReadFile(string) ([]byte, error) { return nil, errNoTerminal }
func (remoteTerm) WriteFile(string, []byte) error  { return errNoTerminal }

// sessionStack is cli.Stack for one CLI session.
type sessionStack struct {
	s     *stackCtl
	user  string
	class commit.Class
}

func (ss sessionStack) Self() int { return ss.s.member }

func (ss sessionStack) Members() []int {
	var out []int
	for id := range ss.s.node.Members() {
		out = append(out, id)
	}
	if !slices.Contains(out, ss.s.member) {
		out = append(out, ss.s.member)
	}
	sort.Ints(out)
	return out
}

func (ss sessionStack) Exec(ctx context.Context, member int, line string, confirmed bool) (string, error) {
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := ss.s.node.Call(member, "exec", execRequest{User: ss.user, Class: ss.class.String(), Line: line, Confirmed: confirmed},
			memberExecTimeout+5*time.Second)
		var out string
		if err == nil {
			err = json.Unmarshal(raw, &out)
		} else if !errors.As(err, new(*control.RemoteError)) {
			err = fmt.Errorf("member %d is not reachable (%v)", member, err)
		}
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// synced waits up to 2 s until this member stored revision rev (a commit
// made through the master) and shows it.
func (s *stackCtl) synced(rev uint64) {
	e := s.eng()
	if e == nil || s.node.IsMaster() {
		return
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if s.node.Store.Has(rev) {
			s.reload()
			return
		}
		if e.ActiveSeq() >= rev {
			return
		}
	}
}

// Port names of the other members, for completion and help in the CLI
// (every member's ports are offered, reference 3.3). Fetched in the
// background and cached, so that ? and Tab never wait for the stack.
const portsRefresh = 30 * time.Second

type portCache struct {
	mu      sync.Mutex
	names   map[int][]string
	fetched time.Time
	running bool
}

// servePorts answers other members' "ports" requests with this member's
// port names.
func (s *stackCtl) servePorts(local func() []string) {
	s.node.Handle("ports", func(int, json.RawMessage) (any, error) { return local(), nil })
}

// remotePorts returns the cached port names of the other members and
// starts a refresh when the cache is old.
func (s *stackCtl) remotePorts() []string {
	c := &s.ports
	c.mu.Lock()
	var out []string
	for id, names := range c.names {
		if id != s.member {
			out = append(out, names...)
		}
	}
	stale := time.Since(c.fetched) > portsRefresh && !c.running
	if stale {
		c.running = true
	}
	c.mu.Unlock()
	if stale {
		go s.refreshPorts()
	}
	return out
}

func (s *stackCtl) refreshPorts() {
	got := map[int][]string{}
	for id := range s.node.Members() {
		if id == s.member {
			continue
		}
		raw, err := s.node.Call(id, "ports", nil, 3*time.Second)
		if err != nil {
			continue
		}
		var names []string
		if json.Unmarshal(raw, &names) == nil {
			got[id] = names
		}
	}
	c := &s.ports
	c.mu.Lock()
	defer c.mu.Unlock()
	// Keep what an unreachable member reported last.
	if c.names == nil {
		c.names = map[int][]string{}
	}
	for id := range c.names {
		if _, member := s.node.Members()[id]; !member {
			delete(c.names, id)
		}
	}
	for id, n := range got {
		c.names[id] = n
	}
	c.fetched, c.running = time.Now(), false
}
