package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"mclag/internal/commit"
	"mclag/internal/config"
	"mclag/internal/dataplane"
	"mclag/internal/model"
)

// kernelApplier makes a configuration effective on this member's kernel.
type kernelApplier struct {
	mu     sync.Mutex
	kernel dataplane.Kernel
	member int
	// dryRun plans against a simulated copy of the kernel and only logs.
	dryRun    bool
	stateFile string // links managed by the last apply
	log       *slog.Logger
}

func memberName(id int) string { return fmt.Sprintf("member%d", id) }

func (a *kernelApplier) loadOwned() map[string]bool {
	owned := map[string]bool{}
	raw, err := os.ReadFile(a.stateFile)
	if err != nil {
		return owned
	}
	var names []string
	if json.Unmarshal(raw, &names) == nil {
		for _, n := range names {
			owned[n] = true
		}
	}
	return owned
}

func (a *kernelApplier) saveOwned(s *dataplane.State) error {
	names := make([]string, 0, len(s.Links))
	for n := range s.Links {
		names = append(names, n)
	}
	sort.Strings(names)
	raw, _ := json.Marshal(names)
	tmp := a.stateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.stateFile)
}

func (a *kernelApplier) Apply(_ context.Context, _, to *config.Tree) []commit.MemberResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	res := commit.MemberResult{Member: memberName(a.member)}
	res.Err = a.apply(to)
	return []commit.MemberResult{res}
}

func (a *kernelApplier) apply(to *config.Tree) error {
	cfg, issues := model.Build(to.Active(), nil)
	if issues.HasErrors() {
		// Validation happened before; this only guards startup with a
		// stored configuration that newer rules reject.
		return fmt.Errorf("configuration is invalid:\n%s", issues)
	}
	desired, notes := dataplane.Compute(cfg, a.member)
	for _, n := range notes {
		a.log.Warn("data plane", "note", n)
	}
	actual, err := a.kernel.Read()
	if err != nil {
		return fmt.Errorf("reading kernel state: %w", err)
	}
	ops := dataplane.Plan(actual, desired, a.loadOwned())
	if len(ops) == 0 {
		a.log.Info("data plane: nothing to change")
		return nil
	}
	a.log.Info("data plane: plan", "ops", len(ops), "dry_run", a.dryRun, "plan", dataplane.FormatPlan(ops))
	k := a.kernel
	if a.dryRun {
		k = dataplane.NewFake(actual)
	}
	warn := func(op dataplane.Op, err error) { a.log.Warn("data plane: skipped", "op", op.String(), "err", err) }
	if err := dataplane.ExecuteLenient(k, ops, warn); err != nil {
		return err
	}
	if a.dryRun {
		return nil
	}
	return a.saveOwned(desired)
}

func newKernelApplier(stateDir string, dryRun bool, log *slog.Logger) *kernelApplier {
	return &kernelApplier{
		kernel:    &dataplane.Netlink{},
		member:    1, // standalone until stacking (Phase 5)
		dryRun:    dryRun,
		stateFile: filepath.Join(stateDir, "dataplane-owned.json"),
		log:       log,
	}
}
