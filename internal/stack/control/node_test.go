package control

import (
	"encoding/json"
	"fmt"
	"mclag/internal/schema"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"mclag/internal/commit"
	"mclag/internal/stack/mesh"
)

type testMember struct {
	id    int
	dir   string
	mesh  *mesh.Mesh
	store *commit.FileStore
	node  *Node
	prio  map[int]int
}

func fast(c *raft.Config) {
	c.HeartbeatTimeout = 200 * time.Millisecond
	c.ElectionTimeout = 200 * time.Millisecond
	c.LeaderLeaseTimeout = 100 * time.Millisecond
	c.CommitTimeout = 10 * time.Millisecond
}

func newMember(t *testing.T, id int, dir string, founder bool, prio map[int]int) *testMember {
	t.Helper()
	m := &testMember{id: id, dir: dir, prio: prio}
	if m.mesh == nil {
		m.mesh = mesh.New(id, nil)
	}
	m.start(t, founder)
	return m
}

func (m *testMember) start(t *testing.T, founder bool) {
	t.Helper()
	var err error
	m.store, err = commit.OpenFileStore(filepath.Join(m.dir, "config"), 50)
	if err != nil {
		t.Fatal(err)
	}
	m.node = &Node{Self: m.id, SelfKey: []byte(fmt.Sprintf("key-%d", m.id)), Dir: filepath.Join(m.dir, "raft"),
		Store: m.store, MetaFile: filepath.Join(m.dir, "control.json"), Mesh: m.mesh, Founder: founder,
		Priority: func(id int) int {
			if p, ok := m.prio[id]; ok {
				return p
			}
			return 128
		}, tune: fast}
	if err := m.node.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.node.Close)
}

func connect(a, b *testMember) func() {
	ca, cb := net.Pipe()
	go a.mesh.AddPeer(b.id, ca)
	go b.mesh.AddPeer(a.id, cb)
	return func() { ca.Close(); cb.Close() }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func rev(seq uint64, cfg string) *commit.Revision {
	return &commit.Revision{Seq: seq, Time: time.Now().UTC(), User: "test", Config: json.RawMessage(cfg)}
}

func lastSeq(s *commit.FileStore) uint64 {
	r := s.Revisions()
	if len(r) == 0 {
		return 0
	}
	return r[len(r)-1].Seq
}

func master(ms ...*testMember) int {
	for _, m := range ms {
		if m.node.IsMaster() {
			return m.id
		}
	}
	return 0
}

func TestStackControl(t *testing.T) {
	base := t.TempDir()
	// Member 1 has a configuration history from before Raft.
	pre, err := commit.OpenFileStore(filepath.Join(base, "1", "config"), 50)
	if err != nil {
		t.Fatal(err)
	}
	pre.Put(rev(1, `{}`), 0)
	pre.Put(rev(2, `{"system":{"host-name":"a"}}`), 0)

	prio := map[int]int{}
	m1 := newMember(t, 1, filepath.Join(base, "1"), true, prio)
	waitFor(t, "1 master", func() bool { return m1.node.IsMaster() })
	waitFor(t, "member list", func() bool { _, ok := m1.node.Members()[1]; return ok })
	st := m1.node.EngineStore()
	if err := st.Put(rev(3, `{"system":{"host-name":"b"}}`), 0); err != nil {
		t.Fatal(err)
	}
	if lastSeq(m1.store) != 3 {
		t.Fatalf("master store at %d", lastSeq(m1.store))
	}

	// Members 2 and 3 join with tokens (as the join exchange does).
	m2 := newMember(t, 2, filepath.Join(base, "2"), false, prio)
	m3 := newMember(t, 3, filepath.Join(base, "3"), false, prio)
	connect(m1, m2)
	connect(m2, m3)
	cut31 := connect(m3, m1)
	for _, id := range []int{2, 3} {
		tok := fmt.Sprintf("TOKEN%d", id)
		if err := m1.node.AddToken(id, tok, time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := m1.node.Admit(tok, []byte(fmt.Sprintf("key-%d", id))); err != nil {
			t.Fatal(err)
		}
		if err := m1.node.Admit(tok, nil); err == nil {
			t.Fatal("token used twice")
		}
	}
	waitFor(t, "3 voters", func() bool {
		v := 0
		for _, s := range m1.node.Servers() {
			if s.Voter {
				v++
			}
		}
		return v == 3
	})
	waitFor(t, "replicated to 2 and 3", func() bool { return lastSeq(m2.store) == 3 && lastSeq(m3.store) == 3 })
	waitFor(t, "3 current", m3.node.Current)
	if len(m3.store.Revisions()) != 3 {
		t.Errorf("member 3 has %d revisions, want the whole history", len(m3.store.Revisions()))
	}
	if !slices.Equal(keys(m3.node.Members()), []int{1, 2, 3}) {
		t.Errorf("member list on 3: %v", keys(m3.node.Members()))
	}

	// A write on a non-master goes through the master.
	if err := m3.node.EngineStore().Put(rev(4, `{}`), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "forwarded write", func() bool { return lastSeq(m1.store) == 4 && lastSeq(m2.store) == 4 })
	if err := m3.node.EngineStore().SetCandidate(nil); err != nil {
		t.Fatal(err)
	}

	// Explicit mastership switch.
	if err := m3.node.Transfer(2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "2 master", func() bool { return master(m1, m2, m3) == 2 })

	// The master fails: one of the others takes over and has everything.
	m2.node.Close()
	waitFor(t, "new master", func() bool { m := master(m1, m3); return m == 1 || m == 3 })
	nm := m1
	if master(m1, m3) == 3 {
		nm = m3
	}
	if err := nm.node.EngineStore().Put(rev(5, `{}`), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "write without 2", func() bool { return lastSeq(m1.store) == 5 && lastSeq(m3.store) == 5 })

	// Member 2 comes back (restart with its state) and catches up.
	m2.start(t, false)
	waitFor(t, "2 caught up", func() bool { return lastSeq(m2.store) == 5 })

	// Removing a member (the master itself: mastership moves first).
	mid := master(m1, m2, m3)
	var mm *testMember
	for _, m := range []*testMember{m1, m2, m3} {
		if m.id == mid {
			mm = m
		}
	}
	if err := mm.node.RemoveMember(mid); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "removed", func() bool {
		for _, m := range []*testMember{m1, m2, m3} {
			if m.id != mid && m.node.IsMaster() {
				_, in := m.node.Members()[mid]
				return !in && len(m.node.Servers()) == 2
			}
		}
		return false
	})
	_ = cut31
}

func keys(m map[int]MemberInfo) []int {
	var out []int
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// A master with priority 0 hands mastership on after the election.
func TestPriorityZeroHandsOn(t *testing.T) {
	base := t.TempDir()
	prio := map[int]int{1: 0}
	m1 := newMember(t, 1, filepath.Join(base, "1"), true, prio)
	// Alone, member 1 is master despite priority 0 (nobody else).
	waitFor(t, "1 master", func() bool { return m1.node.IsMaster() })
	m2 := newMember(t, 2, filepath.Join(base, "2"), false, prio)
	connect(m1, m2)
	if err := m1.node.AddToken(2, "T", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := m1.node.Admit("T", []byte("k")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "2 votes", func() bool { return len(m1.node.Servers()) == 2 })
	// As soon as member 2 votes, member 1 hands mastership on.
	waitFor(t, "2 master", func() bool { return m2.node.IsMaster() })
}

// `show system limits` reads the voter limit from the schema package.
func TestMaxVotersMatchesSchema(t *testing.T) {
	if MaxVoters != schema.MaxVoters {
		t.Fatalf("control.MaxVoters %d, schema.MaxVoters %d", MaxVoters, schema.MaxVoters)
	}
}

// A two-member stack that lost a member has no majority; force-master lets
// the remaining member continue alone, and the other one rejoins later.
func TestForceMaster(t *testing.T) {
	base := t.TempDir()
	prio := map[int]int{}
	m1 := newMember(t, 1, filepath.Join(base, "1"), true, prio)
	waitFor(t, "1 master", func() bool { return m1.node.IsMaster() })
	m2 := newMember(t, 2, filepath.Join(base, "2"), false, prio)
	cut := connect(m1, m2)
	if err := m1.node.AddToken(2, "T", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := m1.node.Admit("T", []byte("key-2")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "2 votes", func() bool { return len(m1.node.Servers()) == 2 })
	if err := m1.node.EngineStore().Put(rev(1, `{"system":{"host-name":"a"}}`), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "replicated", func() bool { return lastSeq(m2.store) == 1 })
	if err := m1.node.ForceMaster(); err == nil {
		t.Fatal("force-master accepted while the stack has a master")
	}

	// Whoever is master, the other one fails: the survivor has no majority.
	surv, dead := m1, m2
	if m2.node.IsMaster() {
		surv, dead = m2, m1
	}
	dead.node.Close()
	cut() // as when the switch is off: its stacking link is down
	waitFor(t, "no master", func() bool { return surv.node.Master() == 0 })
	if err := surv.node.EngineStore().Put(rev(2, `{}`), 0); err == nil {
		t.Fatal("commit without a majority succeeded")
	}
	if err := surv.node.ForceMaster(); err != nil {
		t.Fatal(err)
	}
	surv.node.Close()
	surv.start(t, false)
	waitFor(t, "master alone", func() bool { return surv.node.IsMaster() })
	if err := surv.node.EngineStore().Put(rev(2, `{"system":{"host-name":"b"}}`), 0); err != nil {
		t.Fatalf("commit after force-master: %v", err)
	}
	if lastSeq(surv.store) != 2 {
		t.Fatalf("survivor at %d", lastSeq(surv.store))
	}
	if _, err := os.Stat(surv.node.forceFile()); err == nil {
		t.Error("force marker left behind")
	}
	// The other member is still in the member list, not voting while away.
	if _, ok := surv.node.Members()[dead.id]; !ok {
		t.Error("member list lost the failed member")
	}

	// It returns with its old state, takes over the survivor's history and
	// votes again.
	dead.start(t, false)
	connect(surv, dead)
	waitFor(t, "returned member caught up", func() bool { return lastSeq(dead.store) == 2 })
	waitFor(t, "both vote", func() bool {
		v := 0
		for _, s := range surv.node.Servers() {
			if s.Voter {
				v++
			}
		}
		return v == 2
	})
	if err := surv.node.EngineStore().Put(rev(3, `{}`), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "3 on both", func() bool { return lastSeq(dead.store) == 3 })
}
