// Package control is the stack's replicated control state
// (docs/stack-protocol.md, "Stack control (Raft)"): configuration
// revisions, the pending confirmation, the shared candidate, the member list
// and join tokens, replicated with Raft over the mesh. The Raft leader is the
// stack master.
package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"mclag/internal/commit"
)

// MemberInfo is one entry of the member list.
type MemberInfo struct {
	Key []byte `json:"key"` // Ed25519 public key
}

// Token is an open join token.
type Token struct {
	Member  int       `json:"member"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// meta is the replicated state besides the configuration store, kept in
// its own file together with the last applied Raft index.
type meta struct {
	Index   uint64             `json:"index"`
	Cluster string             `json:"cluster,omitempty"`
	Members map[int]MemberInfo `json:"members,omitempty"`
	Tokens  []Token            `json:"tokens,omitempty"`
}

func (m meta) clone() meta {
	c := m
	c.Members = map[int]MemberInfo{}
	for id, mi := range m.Members {
		c.Members[id] = mi
	}
	c.Tokens = slices.Clone(m.Tokens)
	return c
}

// State is the whole replicated state (snapshots and the load entry).
type State struct {
	Index     uint64             `json:"index"`
	Cluster   string             `json:"cluster"`
	Members   map[int]MemberInfo `json:"members"`
	Tokens    []Token            `json:"tokens,omitempty"`
	Revisions []*commit.Revision `json:"revisions"`
	Pending   *commit.Pending    `json:"pending,omitempty"`
	Candidate json.RawMessage    `json:"candidate,omitempty"`
}

// Command operations.
const (
	opLoad         = "load"
	opPut          = "put"
	opPending      = "pending"
	opCandidate    = "candidate"
	opTokenAdd     = "token-add"
	opJoin         = "join"
	opMemberRemove = "member-remove"
)

// command is one Raft log entry.
type command struct {
	Op        string           `json:"op"`
	Rev       *commit.Revision `json:"rev,omitempty"`
	Keep      uint64           `json:"keep,omitempty"`
	Pending   *commit.Pending  `json:"pending,omitempty"`
	Candidate json.RawMessage  `json:"candidate,omitempty"`
	Member    int              `json:"member,omitempty"`
	Key       []byte           `json:"key,omitempty"`
	Token     string           `json:"token,omitempty"`
	Expires   time.Time        `json:"expires,omitzero"`
	Now       time.Time        `json:"now,omitzero"`
	State     *State           `json:"state,omitempty"`
}

// fsm applies commands to the local configuration store and the meta file.
type fsm struct {
	store    *commit.FileStore
	metaFile string
	onChange func() // after every change; must not block

	mu   sync.Mutex
	meta meta
}

func openFSM(store *commit.FileStore, metaFile string, onChange func()) (*fsm, error) {
	f := &fsm{store: store, metaFile: metaFile, onChange: onChange, meta: meta{Members: map[int]MemberInfo{}}}
	raw, err := os.ReadFile(metaFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &f.meta); err != nil {
			return nil, fmt.Errorf("%s: %w", metaFile, err)
		}
		if f.meta.Members == nil {
			f.meta.Members = map[int]MemberInfo{}
		}
	}
	return f, nil
}

func (f *fsm) saveLocked() error {
	raw, err := json.Marshal(f.meta)
	if err != nil {
		return err
	}
	tmp := f.metaFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.metaFile)
}

func (f *fsm) snapshotMeta() meta {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.meta.clone()
}

func (f *fsm) changed() {
	if f.onChange != nil {
		f.onChange()
	}
}

// Apply implements raft.FSM. Entries at or below the recorded index were
// applied before a restart and are skipped.
func (f *fsm) Apply(l *raft.Log) any {
	if l.Type != raft.LogCommand {
		return nil
	}
	f.mu.Lock()
	if l.Index <= f.meta.Index {
		f.mu.Unlock()
		return nil
	}
	var c command
	if err := json.Unmarshal(l.Data, &c); err != nil {
		f.meta.Index = l.Index
		_ = f.saveLocked()
		f.mu.Unlock()
		return fmt.Errorf("bad control entry: %w", err)
	}
	res := f.applyLocked(&c)
	f.meta.Index = l.Index
	if err := f.saveLocked(); err != nil && res == nil {
		res = err
	}
	f.mu.Unlock()
	f.changed()
	if res == nil {
		return nil
	}
	return res
}

func (f *fsm) applyLocked(c *command) error {
	switch c.Op {
	case opLoad:
		if c.State == nil {
			return errors.New("load without state")
		}
		return f.loadLocked(c.State)
	case opPut:
		if c.Rev == nil {
			return errors.New("put without revision")
		}
		if f.store.Has(c.Rev.Seq) {
			return nil // replayed
		}
		return f.store.Put(c.Rev, c.Keep)
	case opPending:
		return f.store.SetPending(c.Pending)
	case opCandidate:
		return f.store.SetCandidateRaw(c.Candidate)
	case opTokenAdd:
		f.meta.Tokens = slices.DeleteFunc(f.meta.Tokens, func(t Token) bool {
			return !c.Now.Before(t.Expires) || t.Token == c.Token || t.Member == c.Member
		})
		f.meta.Tokens = append(f.meta.Tokens, Token{Member: c.Member, Token: c.Token, Expires: c.Expires})
		return nil
	case opJoin:
		i := slices.IndexFunc(f.meta.Tokens, func(t Token) bool { return t.Token == c.Token })
		if i < 0 || !c.Now.Before(f.meta.Tokens[i].Expires) {
			return errors.New("join token unknown, used or expired")
		}
		t := f.meta.Tokens[i]
		f.meta.Tokens = slices.Delete(f.meta.Tokens, i, i+1)
		f.meta.Members[t.Member] = MemberInfo{Key: c.Key}
		return nil
	case opMemberRemove:
		delete(f.meta.Members, c.Member)
		return nil
	}
	return fmt.Errorf("unknown control operation %q", c.Op)
}

func (f *fsm) loadLocked(s *State) error {
	if err := f.store.Replace(s.Revisions, s.Pending, s.Candidate); err != nil {
		return err
	}
	f.meta.Cluster = s.Cluster
	f.meta.Members = map[int]MemberInfo{}
	for id, m := range s.Members {
		f.meta.Members[id] = m
	}
	f.meta.Tokens = slices.Clone(s.Tokens)
	return nil
}

// state returns the whole replicated state.
func (f *fsm) state() (*State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateLocked()
}

func (f *fsm) stateLocked() (*State, error) {
	cand, err := f.store.Candidate()
	if err != nil {
		return nil, err
	}
	m := f.meta.clone()
	return &State{Index: m.Index, Cluster: m.Cluster, Members: m.Members, Tokens: m.Tokens,
		Revisions: f.store.Revisions(), Pending: f.store.Pending(), Candidate: cand}, nil
}

// Snapshot implements raft.FSM. Revisions are immutable, so the copy is
// cheap; it is encoded in Persist.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	s, err := f.state()
	if err != nil {
		return nil, err
	}
	return snapshot{s}, nil
}

// Restore implements raft.FSM. A snapshot older than the local state (the
// local store is ahead after a restart) is ignored.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var s State
	if err := json.NewDecoder(rc).Decode(&s); err != nil {
		return err
	}
	f.mu.Lock()
	if s.Index <= f.meta.Index && f.meta.Cluster == s.Cluster {
		f.mu.Unlock()
		return nil
	}
	err := f.loadLocked(&s)
	if err == nil {
		f.meta.Index = s.Index
		err = f.saveLocked()
	}
	f.mu.Unlock()
	f.changed()
	return err
}

type snapshot struct{ s *State }

func (s snapshot) Persist(sink raft.SnapshotSink) error {
	if err := json.NewEncoder(sink).Encode(s.s); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (snapshot) Release() {}
