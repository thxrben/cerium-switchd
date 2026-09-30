package commit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mclag/internal/config"
)

// Revision is one committed configuration.
type Revision struct {
	Seq     uint64    `json:"seq"`
	Time    time.Time `json:"time"`
	User    string    `json:"user"`
	Comment string    `json:"comment,omitempty"`
	// Config is the full configuration including inactive statements.
	Config json.RawMessage `json:"config"`
}

// Tree decodes the revision's configuration.
func (r *Revision) Tree() (*config.Tree, error) {
	t, err := config.FromJSON(r.Config)
	if err != nil {
		return nil, fmt.Errorf("revision %d: %w", r.Seq, err)
	}
	return t, nil
}

// Pending describes commits that are waiting for confirmation.
type Pending struct {
	Deadline time.Time `json:"deadline"`
	// Target is the last confirmed revision, the rollback target.
	Target uint64 `json:"target"`
	// First is the first unconfirmed revision.
	First uint64 `json:"first"`
}

// Store persists revisions and the pending-confirmation state. It must be
// safe for concurrent use.
//
// Crash safety relies on the write order used by the engine: the pending
// state is written before the revision it covers, and removed before
// anything else happens on confirmation. A pending state that refers to a
// revision that was never written is discarded on load, and without a
// pending state every revision is confirmed.
type Store interface {
	// Revisions returns all stored revisions, oldest first.
	Revisions() []*Revision
	// Put stores a new revision and trims old ones, never removing keep.
	Put(r *Revision, keep uint64) error
	Pending() *Pending
	// SetPending stores p, or removes the pending state if p is nil.
	SetPending(p *Pending) error
}

// FileStore keeps one JSON file per revision in a directory.
type FileStore struct {
	mu       sync.Mutex
	dir      string
	max      int
	revs     []*Revision
	pending  *Pending
	fileMode os.FileMode
}

const pendingFile = "pending.json"

// OpenFileStore loads (or creates) a store in dir that keeps max revisions.
func OpenFileStore(dir string, max int) (*FileStore, error) {
	if max < 2 {
		max = 2
	}
	s := &FileStore{dir: dir, max: max, fileMode: 0o600}
	if err := os.MkdirAll(filepath.Join(dir, "rev"), 0o700); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(filepath.Join(dir, "rev"))
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue // leftover temp files
		}
		if _, err := strconv.ParseUint(strings.TrimSuffix(name, ".json"), 10, 64); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "rev", name))
		if err != nil {
			return nil, err
		}
		var r Revision
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		s.revs = append(s.revs, &r)
	}
	sort.Slice(s.revs, func(i, j int) bool { return s.revs[i].Seq < s.revs[j].Seq })

	raw, err := os.ReadFile(filepath.Join(dir, pendingFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var p Pending
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("%s: %w", pendingFile, err)
		}
		// Discard a pending state whose revision was never written, or whose
		// target is gone (then there is nothing to roll back to).
		if last := s.last(); last != nil && last.Seq >= p.First && s.find(p.Target) != nil {
			s.pending = &p
		} else if err := os.Remove(filepath.Join(dir, pendingFile)); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *FileStore) last() *Revision {
	if len(s.revs) == 0 {
		return nil
	}
	return s.revs[len(s.revs)-1]
}

func (s *FileStore) find(seq uint64) *Revision {
	for _, r := range s.revs {
		if r.Seq == seq {
			return r
		}
	}
	return nil
}

func (s *FileStore) Revisions() []*Revision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Revision(nil), s.revs...)
}

func (s *FileStore) Pending() *Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return nil
	}
	c := *s.pending
	return &c
}

func (s *FileStore) revPath(seq uint64) string {
	return filepath.Join(s.dir, "rev", fmt.Sprintf("%012d.json", seq))
}

func (s *FileStore) Put(r *Revision, keep uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if last := s.last(); last != nil && r.Seq <= last.Seq {
		return fmt.Errorf("revision %d is not newer than %d", r.Seq, last.Seq)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := writeAtomic(s.revPath(r.Seq), raw, s.fileMode); err != nil {
		return err
	}
	s.revs = append(s.revs, r)
	for len(s.revs) > s.max {
		i := 0
		if s.revs[0].Seq == keep {
			i = 1
		}
		if err := os.Remove(s.revPath(s.revs[i].Seq)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.revs = append(s.revs[:i], s.revs[i+1:]...)
	}
	return nil
}

func (s *FileStore) SetPending(p *Pending) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, pendingFile)
	if p == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.pending = nil
		return syncDir(s.dir)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := writeAtomic(path, raw, s.fileMode); err != nil {
		return err
	}
	c := *p
	s.pending = &c
	return nil
}

// writeAtomic writes data to path via a synced temporary file and rename.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// CandidateStore is implemented by stores that keep the shared candidate
// across restarts of switchd (reference 3.1).
type CandidateStore interface {
	// Candidate returns the stored shared candidate as JSON (nil: none).
	Candidate() (json.RawMessage, error)
	// SetCandidate stores t, or removes the stored candidate if t is nil.
	SetCandidate(t *config.Tree) error
}

const candidateFile = "candidate.json"

func (s *FileStore) Candidate() (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(s.dir, candidateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

func (s *FileStore) SetCandidate(t *config.Tree) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, candidateFile)
	if t == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	raw, err := json.Marshal(config.ToJSON(t.Root))
	if err != nil {
		return err
	}
	return writeAtomic(path, raw, s.fileMode)
}

// SetCandidateRaw stores a shared candidate given as JSON (nil: remove).
func (s *FileStore) SetCandidateRaw(raw json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, candidateFile)
	if raw == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeAtomic(path, raw, s.fileMode)
}

// Replace makes the store hold exactly revs, p and the candidate (a
// replicated state that arrived as a whole). Revisions that are not in
// revs are removed.
func (s *FileStore) Replace(revs []*Revision, p *Pending, candidate json.RawMessage) error {
	s.mu.Lock()
	keepFiles := map[string]bool{}
	sorted := append([]*Revision(nil), revs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	for _, r := range sorted {
		raw, err := json.Marshal(r)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if err := writeAtomic(s.revPath(r.Seq), raw, s.fileMode); err != nil {
			s.mu.Unlock()
			return err
		}
		keepFiles[filepath.Base(s.revPath(r.Seq))] = true
	}
	ents, _ := os.ReadDir(filepath.Join(s.dir, "rev"))
	for _, e := range ents {
		if !keepFiles[e.Name()] {
			_ = os.Remove(filepath.Join(s.dir, "rev", e.Name()))
		}
	}
	s.revs = sorted
	s.mu.Unlock()
	if err := s.SetPending(p); err != nil {
		return err
	}
	return s.SetCandidateRaw(candidate)
}

// Has reports whether revision seq is stored.
func (s *FileStore) Has(seq uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(seq) != nil
}
