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
	"time"

	"github.com/vishvananda/netlink"

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
	inv       model.Inventory
	// last is the configuration applied last; reconciliation restores it.
	last *config.Tree
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
	res.Err = a.apply(to, "commit")
	if res.Err == nil {
		a.last = to.Clone()
	}
	return []commit.MemberResult{res}
}

// reconcile re-applies the last configuration, e.g. after a NIC appeared or
// something else changed a managed link.
func (a *kernelApplier) reconcile(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == nil {
		return
	}
	if err := a.apply(a.last, reason); err != nil {
		a.log.Error("data plane: reconcile failed", "reason", reason, "err", err)
	}
}

// watch reconciles on link events (debounced) and periodically, until ctx
// is done.
func (a *kernelApplier) watch(ctx context.Context) {
	if a.dryRun {
		return // nothing is applied, so nothing drifts
	}
	updates := make(chan netlink.LinkUpdate, 256)
	done := make(chan struct{})
	defer close(done)
	subscribed := netlink.LinkSubscribeWithOptions(updates, done, netlink.LinkSubscribeOptions{
		ErrorCallback: func(err error) { a.log.Warn("data plane: link events", "err", err) },
	}) == nil
	if !subscribed {
		a.log.Warn("data plane: no link events; relying on periodic reconciliation")
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-updates:
			if !ok {
				updates = nil // subscription ended; periodic only
				continue
			}
			debounce.Reset(300 * time.Millisecond)
		case <-debounce.C:
			a.reconcile("link event")
		case <-tick.C:
			a.reconcile("periodic")
		}
	}
}

func (a *kernelApplier) apply(to *config.Tree, reason string) error {
	cfg, issues := model.Build(to.Active(), a.inv)
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
		if reason == "commit" {
			a.log.Info("data plane: nothing to change")
		}
		return nil
	}
	if reason == "commit" {
		a.log.Info("data plane: plan", "ops", len(ops), "dry_run", a.dryRun, "plan", dataplane.FormatPlan(ops))
	} else {
		// Something outside switchd changed a managed link, or a port
		// appeared: bring it back to the configuration.
		a.log.Warn("data plane: correcting kernel state", "reason", reason, "ops", len(ops), "dry_run", a.dryRun, "plan", dataplane.FormatPlan(ops))
	}
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
