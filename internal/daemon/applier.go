package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mclag/internal/inventory"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"

	"mclag/internal/commit"
	"mclag/internal/config"
	"mclag/internal/dataplane"
	"mclag/internal/model"
	"mclag/internal/routing"
)

// addProtoRoutes adds the routing protocols' active routes to the desired
// L3 state. A protocol route replaces the static route of the same prefix
// in the same instance (the RIB chose it by preference); next hops whose
// unit does not exist on this member are dropped.
func addProtoRoutes(desired *dataplane.State, cfg *model.Config, names dataplane.PortNames, rs []routing.FIBRoute) {
	if desired.L3 == nil || len(rs) == 0 {
		return
	}
	type key struct {
		vrf string
		p   netip.Prefix
	}
	proto := map[key]bool{}
	var out []dataplane.Route
	for _, r := range rs {
		dr := dataplane.Route{VRF: r.Instance, Prefix: r.Prefix, Discard: r.Discard, Proto: r.KernelProto}
		for _, h := range r.NextHops {
			dev, ok := dataplane.UnitDevice(cfg, h.Interface, names)
			if !ok {
				continue
			}
			dr.NextHops = append(dr.NextHops, h.Gateway)
			dr.Devs = append(dr.Devs, dev)
		}
		if !dr.Discard && len(dr.NextHops) == 0 {
			continue
		}
		proto[key{r.Instance, r.Prefix}] = true
		out = append(out, dr)
	}
	desired.L3.Routes = slices.DeleteFunc(desired.L3.Routes, func(r dataplane.Route) bool { return proto[key{r.VRF, r.Prefix}] })
	desired.L3.Routes = append(desired.L3.Routes, out...)
}

// kernelApplier makes a configuration effective on this member's kernel.
type kernelApplier struct {
	mu     sync.Mutex
	kernel dataplane.Kernel
	member int
	// cardNoted: card changes already logged.
	cardNoted map[string]bool
	// dryRun plans against a simulated copy of the kernel and only logs.
	dryRun    bool
	stateFile string // links managed by the last apply
	log       *slog.Logger
	inv       model.Inventory
	names     *inventory.Naming
	// last is the configuration applied last; reconciliation restores it.
	last *config.Tree
	// onApplied is called with each successfully applied configuration
	// (services that are not part of the data plane, e.g. syslog).
	onApplied func(*model.Config)
	// afterApply runs after every successful data plane apply, including
	// reconciliations (LACP follows the bundles' ports).
	afterApply func(*model.Config)
	// isMaster reports whether this member is the master (nil: standalone,
	// always master); the management address lives there (reference 1.8).
	isMaster func() bool
	// cmeMAC is the stack-wide MAC address of cme.
	cmeMAC net.HardwareAddr
	// stackPort reports whether a kernel port is a stacking port (the stack
	// manager owns those).
	stackPort func(linux string) bool
	// protoRoutes returns the active routes of the routing protocols
	// (reference 5.8; nil: none). They are installed next to the static
	// routes; a static route of the same prefix wins only by preference,
	// which the RIB already decided, so the two never overlap.
	protoRoutes func() []routing.FIBRoute
}

func (a *kernelApplier) master() bool { return a.isMaster == nil || a.isMaster() }

// unconfigured returns this member's present ports that the desired state
// does not use: switchd owns them too and keeps them down without
// addresses (reference 1.4). Stacking ports are the stack manager's.
func (a *kernelApplier) unconfigured(cfg *model.Config, desired *dataplane.State) []string {
	var out []string
	for _, p := range a.names.Ports() {
		if desired.Links[p.Linux] != nil || (a.stackPort != nil && a.stackPort(p.Linux)) {
			continue
		}
		out = append(out, p.Linux)
	}
	return out
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
		if a.onApplied != nil {
			if cfg, _ := model.Build(to.Active(), a.inv); cfg != nil {
				a.onApplied(cfg)
			}
		}
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
		return
	}
	// Services outside the data plane converge as well (idempotent; e.g.
	// an account removal that had to wait for the user's processes).
	if a.onApplied != nil && reason == "periodic" {
		if cfg, _ := model.Build(a.last.Active(), a.inv); cfg != nil {
			a.onApplied(cfg)
		}
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
	if changed, err := a.names.Refresh(); err != nil {
		a.log.Warn("port numbering", "err", err)
	} else if changed {
		a.log.Info("ports changed", "ports", len(a.names.Ports()))
		for _, c := range a.names.Cards() {
			key := fmt.Sprintf("%s %s %d", c.Key, c.Note, c.MovedFrom)
			if (c.Note == "" && c.MovedFrom < 0) || a.cardNoted[key] {
				continue
			}
			if a.cardNoted == nil {
				a.cardNoted = map[string]bool{}
			}
			a.cardNoted[key] = true
			if c.Note != "" {
				a.log.Warn("card "+c.Note, "card", c.Number, "slot", c.Key)
			}
			if c.MovedFrom >= 0 {
				a.log.Warn("card moved slots: it has the MAC addresses of an absent card", "card", c.Number, "slot", c.Key,
					"old_card", c.MovedFrom, "hint", fmt.Sprintf("request chassis card %d renumber %d", c.Number, c.MovedFrom))
			}
		}
	}
	cfg, issues := model.Build(to.Active(), a.inv)
	if issues.HasErrors() {
		// Validation happened before; this only guards startup with a
		// stored configuration that newer rules reject.
		return fmt.Errorf("configuration is invalid:\n%s", issues)
	}
	desired, notes := dataplane.Compute(cfg, a.member, a.names.Linux)
	for _, n := range notes {
		a.log.Warn("data plane", "note", n)
	}
	owned := a.loadOwned()
	var unconf []string
	if !a.dryRun {
		unconf = a.unconfigured(cfg, desired)
		for _, n := range unconf {
			owned[n] = true // released: down and out of any bridge or bundle
		}
	}
	dataplane.Management(desired, cfg, a.member, a.names.Linux, a.master(), dataplane.Carrier, a.cmeMAC, unconf)
	if a.protoRoutes != nil {
		addProtoRoutes(desired, cfg, a.names.Linux, a.protoRoutes())
	}
	actual, err := a.kernel.Read()
	if err != nil {
		return fmt.Errorf("reading kernel state: %w", err)
	}
	ops := dataplane.Plan(actual, desired, owned)
	if err := a.execute(ops, reason, actual, desired); err != nil {
		return err
	}
	if a.dryRun {
		return nil
	}
	level := slog.LevelInfo
	if reason != "commit" && reason != "routing" && reason != "dhcp lease" {
		level = slog.LevelWarn // something else changed the kernel state
	}
	// Bridge self VLANs: added before the IP interfaces use them, pruned
	// after the IP interfaces that used them are gone.
	if _, err := a.kernel.SyncSelfVLANs(desired.SelfVLANs, false); err != nil {
		return fmt.Errorf("bridge VLANs: %w", err)
	}
	changed, warnings, err := a.kernel.SyncL3(desired.L3)
	if changed {
		var steps []string
		if nk, ok := a.kernel.(*dataplane.Netlink); ok {
			steps = nk.L3Changes
		}
		a.log.Log(context.Background(), level, "routed interfaces updated", "reason", reason, "steps", strings.Join(steps, " "), "err", err)
	}
	for _, w := range warnings {
		if reason == "commit" {
			a.log.Warn("routing", "note", w)
		}
	}
	if err != nil {
		return fmt.Errorf("routed interfaces: %w", err)
	}
	if _, err := a.kernel.SyncSelfVLANs(desired.SelfVLANs, true); err != nil {
		return fmt.Errorf("bridge VLANs: %w", err)
	}
	// Per-VLAN MTU (vlans <v> mtu).
	mtus := map[int]int{}
	for _, v := range cfg.VLANs {
		if v.MTU != 0 {
			mtus[v.ID] = v.MTU
		}
	}
	if changed, err := a.kernel.SyncVLANMTU(mtus); err != nil {
		a.log.Error("VLAN mtu", "err", err)
	} else if changed {
		a.log.Log(context.Background(), level, "VLAN mtu filters updated", "reason", reason)
	}
	// VXLAN: the remote VTEPs of the VXLAN ports (reference 5.7).
	if changed, err := a.kernel.SyncVXLAN(dataplane.VXLANRemotes(cfg)); err != nil {
		a.log.Error("vxlan", "err", err)
	} else if changed {
		a.log.Log(context.Background(), level, "vxlan remote VTEPs updated", "reason", reason)
	}
	// IGMP/MLD snooping (reference 5.5): after the ports and VLANs exist.
	if changed, err := a.kernel.SyncMulticast(dataplane.ComputeMulticast(cfg, desired, a.names.Linux)); err != nil {
		a.log.Error("multicast snooping", "err", err)
	} else if changed {
		a.log.Log(context.Background(), level, "multicast snooping updated", "reason", reason)
	}
	// Port mirroring (forwarding-options analyzer): after the devices exist.
	if changed, err := a.kernel.SyncMirrors(dataplane.ComputeMirrors(cfg, a.member, a.names.Linux)); err != nil {
		a.log.Error("port mirroring", "err", err)
	} else if changed {
		a.log.Log(context.Background(), level, "port mirroring updated", "reason", reason)
	}
	if err := a.saveOwned(desired); err != nil {
		return err
	}
	if a.afterApply != nil {
		a.afterApply(cfg)
	}
	return nil
}

func (a *kernelApplier) execute(ops []dataplane.Op, reason string, actual, desired *dataplane.State) error {
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
	return dataplane.ExecuteLenient(k, ops, warn)
}

func newKernelApplier(kernel dataplane.Kernel, stateDir string, dryRun bool, log *slog.Logger) *kernelApplier {
	return &kernelApplier{
		kernel:    kernel,
		member:    1, // standalone until stacking (Phase 5)
		dryRun:    dryRun,
		stateFile: filepath.Join(stateDir, "dataplane-owned.json"),
		log:       log,
	}
}
