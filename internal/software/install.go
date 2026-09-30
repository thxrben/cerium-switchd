package software

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Installer installs switchd programs on this member and returns to the
// previous one when a new one does not come up (reference 3.6).
type Installer struct {
	// Program is the installed switchd (/usr/local/sbin/switchd); the
	// previous one is kept next to it as <Program>.prev.
	Program string
	// StateFile records the previous version and a pending update.
	StateFile string
}

// State is what the installer records.
type State struct {
	// Previous is the version of <Program>.prev ("": none).
	Previous string `json:"previous,omitempty"`
	// Pending is an installed version that has not come up healthy yet.
	Pending *Pending `json:"pending,omitempty"`
	// Note reports the last automatic return to the previous version.
	Note string `json:"note,omitempty"`
}

// Pending is an update in progress on this member.
type Pending struct {
	Version  string    `json:"version"`
	Attempts int       `json:"attempts"`
	Since    time.Time `json:"since"`
	// ExitMaintenance: the update put the member into maintenance mode; it
	// leaves it once the new version is healthy.
	ExitMaintenance bool `json:"exit_maintenance,omitempty"`
}

// MaxAttempts: a new version that starts this often without becoming
// healthy is replaced by the previous one.
const MaxAttempts = 3

// Load returns the recorded state.
func (in *Installer) Load() State {
	var st State
	if raw, err := os.ReadFile(in.StateFile); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

func (in *Installer) save(st State) error {
	raw, _ := json.MarshalIndent(st, "", "  ")
	tmp := in.StateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, in.StateFile)
}

func (in *Installer) prev() string { return in.Program + ".prev" }

// Install replaces the program with prog (version) and keeps the running
// one (current) as the previous version. The running process is not
// affected; the new program runs from the next start.
func (in *Installer) Install(prog []byte, version, current string, exitMaintenance bool) error {
	if len(prog) == 0 {
		return errors.New("empty program")
	}
	old, err := os.ReadFile(in.Program)
	if err != nil {
		return fmt.Errorf("reading the installed program: %w", err)
	}
	if err := writeAtomic(in.prev(), old, 0o755); err != nil {
		return fmt.Errorf("keeping the previous program: %w", err)
	}
	st := in.Load()
	st.Previous = current
	st.Pending = &Pending{Version: version, Since: time.Now().UTC(), ExitMaintenance: exitMaintenance}
	if err := in.save(st); err != nil {
		return err
	}
	if err := writeAtomic(in.Program, prog, 0o755); err != nil {
		return fmt.Errorf("installing the program: %w", err)
	}
	return nil
}

// Rollback puts the previous program back (from the next start on); the
// current one becomes the previous one.
func (in *Installer) Rollback(current string, exitMaintenance bool) (string, error) {
	st := in.Load()
	prog, err := os.ReadFile(in.prev())
	if err != nil || st.Previous == "" {
		return "", errors.New("there is no previous version on this member")
	}
	to := st.Previous
	return to, in.Install(prog, to, current, exitMaintenance)
}

// Start runs when switchd starts with version running. It counts the
// attempts of a pending update; after MaxAttempts it puts the previous
// program back and returns true: switchd must exit, systemd starts the
// previous version.
func (in *Installer) Start(running string) (restart bool, err error) {
	st := in.Load()
	p := st.Pending
	if p == nil {
		return false, nil
	}
	if p.Version != running {
		// Not the pending version (e.g. the program was replaced by hand).
		st.Pending = nil
		return false, in.save(st)
	}
	p.Attempts++
	if p.Attempts <= MaxAttempts {
		return false, in.save(st)
	}
	prog, err := os.ReadFile(in.prev())
	if err != nil {
		st.Pending = nil
		st.Note = fmt.Sprintf("%s did not come up and there is no previous program", running)
		return false, errors.Join(in.save(st), err)
	}
	failed := p.Version
	if cur, err := os.ReadFile(in.Program); err == nil {
		_ = writeAtomic(in.prev(), cur, 0o755) // the failed one becomes the previous one
	}
	if err := writeAtomic(in.Program, prog, 0o755); err != nil {
		return false, err
	}
	st.Note = fmt.Sprintf("%s did not come up after %d starts; returned to %s at %s", failed, MaxAttempts, st.Previous,
		time.Now().UTC().Format(time.RFC3339))
	st.Pending = &Pending{Version: st.Previous, Since: time.Now().UTC(), ExitMaintenance: p.ExitMaintenance}
	st.Previous = failed
	return true, in.save(st)
}

// Healthy runs once switchd is working with running: the pending update is
// done. It returns the finished update (nil: none).
func (in *Installer) Healthy(running string) (*Pending, error) {
	st := in.Load()
	p := st.Pending
	if p == nil || p.Version != running {
		return nil, nil
	}
	st.Pending = nil
	return p, in.save(st)
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".new")
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
