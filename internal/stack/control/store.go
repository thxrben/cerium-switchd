package control

import (
	"encoding/json"

	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/config"
)

// Store is the configuration store of the commit engine in a stack: reads
// are local (always available), writes go through the master
// (commit.Store and commit.CandidateStore).
type Store struct{ n *Node }

// Store returns the engine's view of the replicated configuration store.
func (n *Node) EngineStore() *Store { return &Store{n} }

func (s *Store) Revisions() []*commit.Revision { return s.n.Store.Revisions() }
func (s *Store) Pending() *commit.Pending      { return s.n.Store.Pending() }

func (s *Store) Put(r *commit.Revision, keep uint64) error {
	return s.n.write(&command{Op: opPut, Rev: r, Keep: keep})
}

func (s *Store) SetPending(p *commit.Pending) error {
	return s.n.write(&command{Op: opPending, Pending: p})
}

func (s *Store) Candidate() (json.RawMessage, error) { return s.n.Store.Candidate() }

func (s *Store) SetCandidate(t *config.Tree) error {
	var raw json.RawMessage
	if t != nil {
		var err error
		if raw, err = json.Marshal(config.ToJSON(t.Root)); err != nil {
			return err
		}
	}
	return s.n.write(&command{Op: opCandidate, Candidate: raw})
}
