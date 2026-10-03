package control

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/stack/mesh"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// MaxVoters is the largest number of voting members.
const MaxVoters = 7

// maxRPC bounds one control request or reply (a full configuration).
const maxRPC = 64 << 20

// ErrNoMaster: no master is known (no majority, or the election is not
// finished).
var ErrNoMaster = errors.New("no master (the stack has no majority)")

// Node is this member's part of the stack control.
type Node struct {
	Self    int
	SelfKey []byte // this member's public key (for the bootstrap member list)
	Dir     string // Raft log, stable state and snapshots
	// Store is the local configuration store that the replicated state is
	// written to; MetaFile holds the rest of the replicated state.
	Store    *commit.FileStore
	MetaFile string
	Mesh     *mesh.Mesh
	// Founder: this switch created its stack and never joined another one,
	// so it may start a new Raft cluster (bootstrap).
	Founder bool
	// Priority returns a member's mastership-priority (from the active
	// configuration).
	Priority func(member int) int
	Log      *slog.Logger
	// OnChange is called (in its own goroutine, coalesced) after the
	// replicated state changed.
	OnChange func()
	// OnLeader is called when this member becomes master (true) or stops
	// being master (false), in order.
	OnLeader func(bool)

	fsm       *fsm
	raft      *raft.Raft
	trans     *raft.NetworkTransport
	bolt      *raftboltdb.BoltStore
	ctlL      net.Listener
	cancel    context.CancelFunc
	changeC   chan struct{}
	leader    bool
	leaderMu  sync.Mutex
	handlers  map[string]Handler
	handlerMu sync.Mutex
	wg        sync.WaitGroup
	tune      func(*raft.Config) // tests: shorter timeouts
}

// Handler serves one control RPC operation for other members.
type Handler func(from int, req json.RawMessage) (any, error)

func serverID(member int) raft.ServerID { return raft.ServerID("member-" + strconv.Itoa(member)) }

func parseServer(s string) (int, bool) {
	n, ok := strings.CutPrefix(s, "member-")
	id, err := strconv.Atoi(n)
	return id, ok && err == nil && id >= 1 && id <= 16
}

// Start opens the Raft state and joins the stack control. A founder without
// Raft state bootstraps a one-member cluster.
func (n *Node) Start() error {
	if n.Log == nil {
		n.Log = slog.New(slog.DiscardHandler)
	}
	if n.Priority == nil {
		n.Priority = func(int) int { return 128 }
	}
	var err error
	n.changeC = make(chan struct{}, 1)
	n.fsm, err = openFSM(n.Store, n.MetaFile, func() {
		select {
		case n.changeC <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return err
	}
	if err := hwio.MkdirAll(n.Dir, 0o700); err != nil {
		return err
	}
	n.bolt, err = raftboltdb.NewBoltStore(filepath.Join(n.Dir, "raft.db"))
	if err != nil {
		return fmt.Errorf("raft store: %w", err)
	}
	hl := hclog.FromStandardLogger(slog.NewLogLogger(n.Log.Handler(), slog.LevelInfo),
		&hclog.LoggerOptions{Name: "raft", Level: hclog.Warn})
	snaps, err := raft.NewFileSnapshotStoreWithLogger(n.Dir, 2, hl)
	if err != nil {
		n.bolt.Close()
		return fmt.Errorf("raft snapshots: %w", err)
	}
	n.trans = raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream: &streamLayer{m: n.Mesh, l: n.Mesh.Listen("raft")}, MaxPool: 3, Timeout: 3 * time.Second, Logger: hl})
	conf := raft.DefaultConfig()
	conf.LocalID = serverID(n.Self)
	conf.Logger = hl
	// The local store is written by the state machine and is always at
	// least as new as any local snapshot.
	conf.NoSnapshotRestoreOnStart = true
	conf.SnapshotThreshold = 1024
	conf.TrailingLogs = 1024
	if n.tune != nil {
		n.tune(conf)
	}
	existing, err := raft.HasExistingState(n.bolt, n.bolt, snaps)
	if err != nil {
		n.closeStores()
		return err
	}
	if existing && fileExists(n.forceFile()) {
		// request virtual-chassis force-master: this member alone forms the
		// Raft configuration; the others come back as non-voters.
		n.Log.Warn("stack control: force-master, this member continues alone", "facility", "change-log")
		rc := raft.Configuration{Servers: []raft.Server{
			{ID: serverID(n.Self), Address: raft.ServerAddress(serverID(n.Self)), Suffrage: raft.Voter}}}
		if err := raft.RecoverCluster(conf, n.fsm, n.bolt, n.bolt, snaps, n.trans, rc); err != nil {
			n.closeStores()
			return fmt.Errorf("force-master: %w", err)
		}
		hwio.Remove(n.forceFile())
	}
	n.raft, err = raft.NewRaft(conf, n.fsm, n.bolt, n.bolt, snaps, n.trans)
	if err != nil {
		n.closeStores()
		return fmt.Errorf("raft: %w", err)
	}
	if !existing && n.Founder {
		n.Log.Info("stack control: new cluster (this switch created the stack)")
		f := n.raft.BootstrapCluster(raft.Configuration{Servers: []raft.Server{
			{ID: serverID(n.Self), Address: raft.ServerAddress(serverID(n.Self)), Suffrage: raft.Voter}}})
		if err := f.Error(); err != nil {
			n.Log.Error("stack control: bootstrap", "err", err)
		}
	}
	n.handlers = map[string]Handler{}
	n.Handle("write", func(from int, req json.RawMessage) (any, error) {
		if !n.IsMaster() {
			return nil, errors.New("not master")
		}
		return nil, n.applyLocal(req)
	})
	n.ctlL = n.Mesh.Listen("ctl")
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.wg.Add(3)
	go func() { defer n.wg.Done(); n.serveCtl() }()
	go func() { defer n.wg.Done(); n.notifyLoop(ctx) }()
	go func() { defer n.wg.Done(); n.leaderLoop(ctx) }()
	return nil
}

func fileExists(p string) bool { _, err := hwio.Stat(p); return err == nil }

// forceFile marks a forced recovery for the next start.
func (n *Node) forceFile() string { return filepath.Join(n.Dir, "force-master") }

// ForceMaster prepares the split-brain-prone override for a stack that has
// lost its majority (a two-member stack with one member down): at the next
// start (the caller restarts switchd) this member forms the Raft
// configuration alone and becomes master; the other members rejoin as
// non-voters and take over this member's state when they return. An error
// if a master is known (nothing to override).
func (n *Node) ForceMaster() error {
	if n.raft == nil {
		return errors.New("stack control is not running")
	}
	if n.Master() != 0 {
		return fmt.Errorf("the stack has a master (member %d); nothing to override", n.Master())
	}
	return hwio.WriteFile(n.forceFile(), []byte("force\n"), 0o600)
}

func (n *Node) closeStores() {
	if n.bolt != nil {
		n.bolt.Close()
	}
}

// Close leaves the stack control (the Raft state stays on disk).
func (n *Node) Close() {
	if n.cancel != nil {
		n.cancel()
	}
	if n.ctlL != nil {
		n.ctlL.Close()
	}
	if n.raft != nil {
		n.raft.Shutdown().Error()
	}
	if n.trans != nil {
		n.trans.Close()
	}
	n.wg.Wait()
	n.closeStores()
}

func (n *Node) notifyLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.changeC:
			if n.OnChange != nil {
				n.OnChange()
			}
		}
	}
}

// IsMaster reports whether this member is the master.
func (n *Node) IsMaster() bool {
	return n.raft != nil && n.raft.State() == raft.Leader
}

// MasterReady reports whether this member is master and has taken over
// (the initial state is loaded and OnLeader(true) ran).
func (n *Node) MasterReady() bool {
	n.leaderMu.Lock()
	defer n.leaderMu.Unlock()
	return n.leader && n.IsMaster()
}

// settleTime is how long the replicated state must be unchanged before
// it counts as current (a member catching up applies many entries).
const settleTime = 500 * time.Millisecond

// Current reports whether this member's replicated state is up to date: a
// master is known, everything committed has been applied here, and the
// state has not changed for a moment. A member that replays the log after
// joining or a restart passes through old states; they must not be acted
// upon (applied to the data plane, or taken as a removal).
func (n *Node) Current() bool {
	return n.CaughtUp() && time.Since(n.fsm.lastChange()) >= settleTime
}

// CaughtUp reports whether a master is known and everything committed has
// been applied here as far as this member knows: while it replays the log,
// the master reports the commit index batch by batch, so only a state that
// stays unchanged (Current) is reliable.
func (n *Node) CaughtUp() bool {
	if n.raft == nil || n.Master() == 0 {
		return false
	}
	st := n.raft.Stats()
	ci, _ := strconv.ParseUint(st["commit_index"], 10, 64)
	ai, _ := strconv.ParseUint(st["applied_index"], 10, 64)
	return ci > 0 && ai >= ci && st["fsm_pending"] == "0"
}

// Master returns the master's member id (0: none known).
func (n *Node) Master() int {
	if n.raft == nil {
		return 0
	}
	_, id := n.raft.LeaderWithID()
	m, _ := parseServer(string(id))
	return m
}

// Members returns the member list.
func (n *Node) Members() map[int]MemberInfo { return n.fsm.snapshotMeta().Members }

// Tokens returns the open join tokens.
func (n *Node) Tokens() []Token { return n.fsm.snapshotMeta().Tokens }

// Server is one member in the Raft configuration.
type Server struct {
	Member int
	Voter  bool
}

// Servers returns the members in the Raft configuration.
func (n *Node) Servers() []Server {
	f := n.raft.GetConfiguration()
	if f.Error() != nil {
		return nil
	}
	var out []Server
	for _, s := range f.Configuration().Servers {
		if id, ok := parseServer(string(s.ID)); ok {
			out = append(out, Server{Member: id, Voter: s.Suffrage == raft.Voter})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Member < out[j].Member })
	return out
}

// ---- writes ----

// write applies a command through the master.
func (n *Node) write(c *command) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if n.IsMaster() {
		return n.applyLocal(raw)
	}
	m := n.Master()
	if m == 0 {
		return ErrNoMaster
	}
	_, err = n.Call(m, "write", json.RawMessage(raw), 15*time.Second)
	return err
}

func (n *Node) applyLocal(raw []byte) error {
	f := n.raft.Apply(raw, 10*time.Second)
	if err := f.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
			return ErrNoMaster
		}
		return err
	}
	if err, ok := f.Response().(error); ok {
		return err
	}
	return nil
}

// AddToken stores a join token for member (reference 5.2, member add).
func (n *Node) AddToken(member int, token string, ttl time.Duration) error {
	now := time.Now()
	return n.write(&command{Op: opTokenAdd, Member: member, Token: token, Now: now, Expires: now.Add(ttl)})
}

// Admit uses a join token: the member of the token joins with key.
func (n *Node) Admit(token string, key []byte) error {
	return n.write(&command{Op: opJoin, Token: token, Key: key, Now: time.Now()})
}

// RemoveMember removes a member from the member list and the Raft
// configuration (after moving mastership away from it).
func (n *Node) RemoveMember(member int) error {
	if !n.IsMaster() {
		m := n.Master()
		if m == 0 {
			return ErrNoMaster
		}
		_, err := n.Call(m, "remove-member", member, 30*time.Second)
		return err
	}
	return n.removeMember(member)
}

func (n *Node) removeMember(member int) error {
	if _, ok := n.Members()[member]; !ok {
		return fmt.Errorf("member %d is not in the member list", member)
	}
	if member == n.Self {
		if err := n.Transfer(0); err != nil {
			return fmt.Errorf("moving mastership first: %w", err)
		}
		// The new master finishes the removal.
		m := n.waitMaster(10 * time.Second)
		if m == 0 || m == n.Self {
			return ErrNoMaster
		}
		_, err := n.Call(m, "remove-member", member, 30*time.Second)
		return err
	}
	if err := n.write(&command{Op: opMemberRemove, Member: member}); err != nil {
		return err
	}
	return n.raft.RemoveServer(serverID(member), 0, 10*time.Second).Error()
}

func (n *Node) waitMaster(d time.Duration) int {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if m := n.Master(); m != 0 {
			return m
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

// Transfer hands mastership to member to (0: the reachable voter with the
// highest priority).
func (n *Node) Transfer(to int) error {
	if !n.IsMaster() {
		m := n.Master()
		if m == 0 {
			return ErrNoMaster
		}
		_, err := n.Call(m, "transfer", to, 30*time.Second)
		return err
	}
	if to == n.Self {
		return nil
	}
	if to == 0 {
		to = n.bestVoter(func(int) bool { return true })
		if to == 0 {
			return errors.New("no other voting member is reachable")
		}
	} else if !n.isVoter(to) {
		return fmt.Errorf("member %d is not a voting member", to)
	}
	return n.raft.LeadershipTransferToServer(serverID(to), raft.ServerAddress(serverID(to))).Error()
}

// draining: this member is in maintenance mode.
func (n *Node) draining() bool { return slices.Contains(n.Mesh.Draining(), n.Self) }

func (n *Node) isVoter(member int) bool {
	for _, s := range n.Servers() {
		if s.Member == member {
			return s.Voter
		}
	}
	return false
}

// bestVoter returns the reachable voter (not this member) with the highest
// priority for which ok is true.
func (n *Node) bestVoter(ok func(prio int) bool) int {
	reach := map[int]bool{}
	for _, m := range n.Mesh.Reachable() {
		reach[m] = true
	}
	for _, m := range n.Mesh.Draining() {
		delete(reach, m) // maintenance mode: never master
	}
	best, bestPrio := 0, -1
	for _, s := range n.Servers() {
		if !s.Voter || s.Member == n.Self || !reach[s.Member] {
			continue
		}
		p := n.Priority(s.Member)
		if p <= 0 || !ok(p) {
			continue
		}
		if p > bestPrio || (p == bestPrio && s.Member < best) {
			best, bestPrio = s.Member, p
		}
	}
	return best
}

// ---- leader duties ----

func (n *Node) setLeader(v bool) {
	n.leaderMu.Lock()
	changed := n.leader != v
	n.leader = v
	n.leaderMu.Unlock()
	if changed {
		if v {
			n.Log.Info("stack control: this member is master")
		} else {
			n.Log.Info("stack control: this member is no longer master")
		}
		if n.OnLeader != nil {
			n.OnLeader(v)
		}
	}
}

func (n *Node) leaderLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	defer n.setLeader(false)
	for {
		select {
		case <-ctx.Done():
			return
		case isLeader := <-n.raft.LeaderCh():
			if isLeader {
				n.becameMaster()
			} else {
				n.setLeader(false)
			}
		case <-t.C:
			if n.IsMaster() {
				n.leaderLock()
				n.reconcileServers()
			} else {
				n.setLeader(false)
			}
		}
	}
}

// leaderLock makes sure this master's state machine is current and the
// OnLeader callback ran.
func (n *Node) leaderLock() {
	n.leaderMu.Lock()
	done := n.leader
	n.leaderMu.Unlock()
	if !done {
		n.becameMaster()
	}
}

func (n *Node) becameMaster() {
	if err := n.raft.Barrier(10 * time.Second).Error(); err != nil {
		n.Log.Warn("stack control: barrier after election", "err", err)
		return
	}
	if n.fsm.snapshotMeta().Cluster == "" {
		// First master of a new cluster: the existing configuration history
		// and this member go into Raft.
		st, err := n.fsm.state()
		if err != nil {
			n.Log.Error("stack control: reading the local state", "err", err)
			return
		}
		var id [8]byte
		rand.Read(id[:])
		st.Cluster = hex.EncodeToString(id[:])
		st.Members = map[int]MemberInfo{n.Self: {Key: n.SelfKey}}
		raw, _ := json.Marshal(&command{Op: opLoad, State: st})
		if err := n.applyLocal(raw); err != nil {
			n.Log.Error("stack control: loading the initial state", "err", err)
			return
		}
	}
	// Hand mastership on right after an election if a better member is
	// there (no preemption later on).
	mine := n.Priority(n.Self)
	if n.draining() {
		mine = 0
	}
	if to := n.bestVoter(func(p int) bool { return mine <= 0 || p > mine }); to != 0 {
		n.Log.Info("stack control: handing mastership to the member with the higher priority", "member", to)
		if err := n.raft.LeadershipTransferToServer(serverID(to), raft.ServerAddress(serverID(to))).Error(); err == nil {
			return
		}
	}
	n.setLeader(true)
	n.reconcileServers()
}

// reconcileServers makes the Raft configuration match the member list:
// the MaxVoters members with the highest priority vote, the others don't.
// One change per call.
func (n *Node) reconcileServers() {
	members := n.Members()
	if len(members) == 0 {
		return
	}
	ids := make([]int, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		pi, pj := n.Priority(ids[i]), n.Priority(ids[j])
		if pi != pj {
			return pi > pj
		}
		return ids[i] < ids[j]
	})
	wantVoter := map[int]bool{}
	for i, id := range ids {
		wantVoter[id] = i < MaxVoters
	}
	have := map[int]bool{} // member -> voter
	for _, s := range n.Servers() {
		have[s.Member] = s.Voter
	}
	for m := range have {
		if _, ok := members[m]; !ok && m != n.Self {
			n.Log.Info("stack control: removing a server that is not a member", "member", m)
			n.raft.RemoveServer(serverID(m), 0, 10*time.Second)
			return
		}
	}
	for _, id := range ids {
		voter, in := have[id]
		addr := raft.ServerAddress(serverID(id))
		// A member that cannot be reached must not vote: the majority
		// would then need it (after force-master, or a member added while
		// it is down).
		if wantVoter[id] && id != n.Self && !slices.Contains(n.Mesh.Reachable(), id) {
			if !in {
				n.Log.Info("stack control: adding non-voting member (not reachable yet)", "member", id)
				n.raft.AddNonvoter(serverID(id), addr, 0, 10*time.Second)
				return
			}
			continue
		}
		switch {
		case !in && wantVoter[id]:
			n.Log.Info("stack control: adding voting member", "member", id)
			n.raft.AddVoter(serverID(id), addr, 0, 10*time.Second)
			return
		case !in:
			n.Log.Info("stack control: adding non-voting member", "member", id)
			n.raft.AddNonvoter(serverID(id), addr, 0, 10*time.Second)
			return
		case voter && !wantVoter[id] && id != n.Self:
			n.Log.Info("stack control: member no longer votes", "member", id)
			n.raft.DemoteVoter(serverID(id), 0, 10*time.Second)
			return
		case !voter && wantVoter[id]:
			n.Log.Info("stack control: member votes", "member", id)
			n.raft.AddVoter(serverID(id), addr, 0, 10*time.Second)
			return
		}
	}
	if !wantVoter[n.Self] || n.Priority(n.Self) <= 0 || n.draining() {
		// This master should not vote, or never be master (or is in
		// maintenance mode): hand mastership
		// on as soon as another voter can take it.
		if to := n.bestVoter(func(int) bool { return true }); to != 0 {
			n.Log.Info("stack control: handing mastership on (priority 0 or not voting)", "member", to)
			n.raft.LeadershipTransferToServer(serverID(to), raft.ServerAddress(serverID(to)))
		}
	}
}

// ---- member RPC (mesh service "ctl") ----

// Handle registers an RPC operation.
func (n *Node) Handle(op string, h Handler) {
	n.handlerMu.Lock()
	n.handlers[op] = h
	n.handlerMu.Unlock()
}

type rpcRequest struct {
	Op  string          `json:"op"`
	Req json.RawMessage `json:"req,omitempty"`
}

type rpcReply struct {
	Res json.RawMessage `json:"res,omitempty"`
	Err string          `json:"err,omitempty"`
}

// RemoteError is an error returned by another member's handler.
type RemoteError struct {
	Member int
	Msg    string
}

func (e *RemoteError) Error() string { return e.Msg }

// Call runs op on member and returns its result (JSON).
func (n *Node) Call(member int, op string, req any, timeout time.Duration) (json.RawMessage, error) {
	s, err := n.Mesh.Dial(member, "ctl", timeout)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(timeout))
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(rpcRequest{Op: op, Req: raw})
	if _, err := s.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(io.LimitReader(s, maxRPC)).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("member %d: %w", member, err)
	}
	var rep rpcReply
	if err := json.Unmarshal(line, &rep); err != nil {
		return nil, err
	}
	if rep.Err != "" {
		return nil, &RemoteError{Member: member, Msg: rep.Err}
	}
	return rep.Res, nil
}

func (n *Node) serveCtl() {
	for {
		c, err := n.ctlL.Accept()
		if err != nil {
			return
		}
		go n.serveCall(c)
	}
}

func (n *Node) serveCall(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(c, maxRPC)).ReadBytes('\n')
	if err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	var req rpcRequest
	var rep rpcReply
	if err := json.Unmarshal(line, &req); err != nil {
		rep.Err = "bad request"
	} else {
		n.handlerMu.Lock()
		h := n.handlers[req.Op]
		n.handlerMu.Unlock()
		from := 0
		if s, ok := c.(*mesh.Stream); ok {
			from = s.Member()
		}
		switch {
		case h == nil && req.Op == "transfer":
			var to int
			json.Unmarshal(req.Req, &to)
			err = n.Transfer(to)
		case h == nil && req.Op == "remove-member":
			var m int
			json.Unmarshal(req.Req, &m)
			if !n.IsMaster() {
				err = errors.New("not master")
			} else {
				err = n.removeMember(m)
			}
		case h == nil:
			err = fmt.Errorf("unknown operation %q", req.Op)
		default:
			var res any
			res, err = h(from, req.Req)
			if err == nil && res != nil {
				rep.Res, err = json.Marshal(res)
			}
		}
		if err != nil {
			rep.Err = err.Error()
		}
	}
	b, _ := json.Marshal(rep)
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	c.Write(append(b, '\n'))
}

// ---- Raft transport over the mesh ----

type streamLayer struct {
	m *mesh.Mesh
	l net.Listener
}

func (s *streamLayer) Accept() (net.Conn, error) { return s.l.Accept() }
func (s *streamLayer) Close() error              { return s.l.Close() }
func (s *streamLayer) Addr() net.Addr            { return raftAddr(serverID(s.m.Self)) }

func (s *streamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	id, ok := parseServer(string(address))
	if !ok {
		return nil, fmt.Errorf("bad raft address %q", address)
	}
	return s.m.Dial(id, "raft", timeout)
}

type raftAddr string

func (a raftAddr) Network() string { return "mesh" }
func (a raftAddr) String() string  { return string(a) }
