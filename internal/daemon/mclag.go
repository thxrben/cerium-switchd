package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/thxrben/cerium-switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/internal/dataplane"
	"github.com/thxrben/cerium-switchd/internal/model"
)

// mclagCtl runs this member's side of its MC-LAG domain (reference 5.6):
// it exchanges leg states with the peer over the stacking plane, holds
// legs out of their bundles when needed and keeps the split horizon.
type mclagCtl struct {
	member  int
	lacp    lacpControl
	stack   *stackCtl // nil: no stack control (no peer communication)
	sysRoot string
	log     *slog.Logger

	mu         sync.Mutex
	cfg        *model.Config
	peerLegs   map[string]bool
	peerSeen   time.Time
	peerKnown  bool
	started    time.Time
	restoreEnd time.Time // delay-restore: legs held until then
	minority   bool      // legs held: this member is in the minority part of the stack
	backSince  time.Time // the peer is reachable again since (minority hold)
	holds      map[string]string
	split      []string
	lastSent   time.Time
	lastLegs   map[string]bool
	lastReady  map[string]int
	initDone   bool
	legState   string          // local and peer legs as last seen (flush on change)
	curLegs    map[string]bool // local legs, current
	macs       *macSync
	peerFacts  map[string]string // the peer's bundle facts (nil: not sent)
	peerReady  map[string]int    // the peer's LACP-ready ports per bundle (minimum-links)
	peerGroups []mcastKey        // the peer's multicast groups on MC-LAG bundles
	// peerJoining: the peer announced that its leg is about to forward
	// (before its LACP enables the first port); filtered as up meanwhile.
	peerJoining map[string]time.Time
	// peerDraining: the peer's legs that leave for maintenance mode.
	peerDraining map[string]bool
	// shMu serialises computing and installing the split horizon (never
	// held while the LACP runtime is called).
	shMu   sync.Mutex
	groups struct {
		applied   map[mcastKey]bool
		refreshed time.Time
		busy      atomic.Bool
	}
	differs   map[string]time.Time // bundle -> facts differ since
	maint     bool                 // maintenance mode: legs held
	maintEnd  time.Time            // maintenance mode ended: legs held until then
	drainFrom map[string]time.Time // maintenance: leg reported down to the peer since
	leaveTo   sync.Map             // bundle -> peer tunnel for beforeLeave (read under the LACP lock)
	// moved: addresses beforeLeave pointed at the peer's tunnel (they do
	// not age); removed when the leg is back or the peer is gone, unless
	// MAC synchronisation has taken them over.
	movedMu sync.Mutex
	moved   map[macKey]string // -> tunnel
}

// mclagDrainNotice: in maintenance mode a leg is reported down to the peer
// this long before it leaves its bundle (the peer lets traffic from the
// tunnel out on its own leg by then).
const mclagDrainNotice = 300 * time.Millisecond

// mclagJoinGrace: a leg the peer announced as joining counts as up this
// long (its regular leg state reports it within a second).
const mclagJoinGrace = 3 * time.Second

// mclagJoinTimeout bounds the announcement of a joining leg: the peer
// filters first, then the leg forwards (or after this, when the peer does
// not answer).
const mclagJoinTimeout = 300 * time.Millisecond

// mclagInconsistentAfter: how long a bundle may differ from the peer's
// before the secondary holds it.
const mclagInconsistentAfter = 10 * time.Second

// mclagRejoinAfter: how long legs held for the minority rule wait after the
// peer is reachable again (the MAC tables are exchanged at once).
const mclagRejoinAfter = 2 * time.Second

// legsMsg is the leg state a member sends its peer.
type legsMsg struct {
	Domain int             `json:"domain"`
	Legs   map[string]bool `json:"legs"`
	// Facts of each MC-LAG bundle as this member applies them (consistency
	// check).
	Facts map[string]string `json:"facts,omitempty"`
	// Ready: ports LACP has ready per bundle (minimum-links counts both
	// members' ports).
	Ready map[string]int `json:"ready,omitempty"`
	// Groups: the multicast groups this member learned on its MC-LAG legs
	// (installed on the peer's legs too, reference 5.5).
	Groups []mcastKey `json:"groups,omitempty"`
	// Draining: legs that leave for maintenance mode (reported down, but
	// still receiving until the partner stops sending on them).
	Draining []string `json:"draining,omitempty"`
}

// joiningMsg announces that a leg is about to forward.
type joiningMsg struct {
	Bundle string `json:"bundle"`
}

// mcastKey is a group membership of an MC-LAG bundle.
type mcastKey struct {
	Bundle string `json:"bundle"`
	VID    int    `json:"vid"`
	Group  string `json:"group"`
}

// mclagGroupRefresh: the peer's groups are installed again this often
// (learned entries expire after the membership interval, 260 s).
const mclagGroupRefresh = 60 * time.Second

// bundleFacts is what both members must agree on for an MC-LAG bundle
// (reference 5.6, consistency checks).
func bundleFacts(cfg *model.Config, name string) string {
	i := cfg.Interfaces[name]
	if i == nil {
		return ""
	}
	vlans := slices.Clone(i.VLANs)
	slices.Sort(vlans)
	mode := "static"
	if i.LACP != nil {
		mode = "passive"
		if i.LACP.Active {
			mode = "active"
		}
		if i.LACP.Fast {
			mode += ",fast"
		} else {
			mode += ",slow"
		}
	}
	sw := "no switching"
	switch {
	case i.Switching && i.Mode == "trunk":
		sw = fmt.Sprintf("trunk vlans %v native %d", vlans, i.NativeVLAN)
	case i.Switching:
		sw = fmt.Sprintf("access vlan %d", i.AccessVLAN)
	}
	return fmt.Sprintf("%s, mtu %d, lacp %s", sw, i.MTU, mode)
}

func newMCLAG(member int, rt lacpControl, stack *stackCtl, log *slog.Logger) *mclagCtl {
	m := &mclagCtl{member: member, lacp: rt, stack: stack, sysRoot: "/sys", log: log, started: time.Now(), holds: map[string]string{},
		differs: map[string]time.Time{}, drainFrom: map[string]time.Time{}}
	m.macs = newMACSync(m, log)
	if stack != nil {
		stack.node.Handle("mclag-legs", func(from int, req json.RawMessage) (any, error) {
			var l legsMsg
			if err := json.Unmarshal(req, &l); err != nil {
				return nil, err
			}
			m.mu.Lock()
			// Only the peer's messages count (the pair id is not compared:
			// older versions sent their configured domain id).
			if d := m.domainLocked(); d != nil && m.peerOf(d) == from {
				m.peerLegs, m.peerSeen, m.peerKnown = l.Legs, time.Now(), true
				m.peerFacts, m.peerReady, m.peerGroups = l.Facts, l.Ready, l.Groups
				m.peerDraining = map[string]bool{}
				for _, b := range l.Draining {
					m.peerDraining[b] = true
				}
			}
			m.mu.Unlock()
			return nil, nil
		})
		stack.node.Handle("mclag-leg-joining", func(from int, req json.RawMessage) (any, error) {
			var j joiningMsg
			if err := json.Unmarshal(req, &j); err != nil {
				return nil, err
			}
			m.mu.Lock()
			d := m.domainLocked()
			ok := d != nil && m.peerOf(d) == from
			if ok {
				if m.peerJoining == nil {
					m.peerJoining = map[string]time.Time{}
				}
				m.peerJoining[j.Bundle] = time.Now()
			}
			m.mu.Unlock()
			if ok {
				// Filter before answering: the peer's leg forwards after the
				// answer.
				m.installSplit(time.Now())
			}
			return nil, nil
		})
	}
	return m
}

// setConfig takes the applied configuration.
func (m *mclagCtl) setConfig(cfg *model.Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// domainLocked returns this member's MC-LAG pair (nil: no MC-LAG bundle).
func (m *mclagCtl) domainLocked() *model.Pair {
	if m.cfg == nil {
		return nil
	}
	return m.cfg.PairOf(m.member)
}

func (m *mclagCtl) peerOf(d *model.Pair) int { return d.Peer(m.member) }

// bundlesLocked lists the pair's MC-LAG bundles.
func (m *mclagCtl) bundlesLocked(d *model.Pair) []string {
	out := slices.Clone(d.Bundles)
	sort.Strings(out)
	return out
}

// primary: the member with the higher mastership-priority, ties: lower id.
func (m *mclagCtl) primaryLocked(d *model.Pair) bool {
	peer := m.peerOf(d)
	prio := func(id int) int {
		if mem := m.cfg.Members[id]; mem != nil {
			return mem.Priority
		}
		return 128
	}
	a, b := prio(m.member), prio(peer)
	return a > b || (a == b && m.member < peer)
}

// peerReachable: the peer is reachable over the stack (then its stack
// tunnel works too: both run over the same stacking links).
func (m *mclagCtl) peerReachable(peer int) bool {
	return m.stack != nil && slices.Contains(m.stack.node.Mesh.Reachable(), peer)
}

// reachLocked counts the switch members this member reaches (itself
// included) and the switch members of the stack.
func (m *mclagCtl) reachLocked() (reach, total int) {
	members := m.cfg.SwitchMembers()
	var up []int
	if m.stack != nil {
		up = m.stack.node.Mesh.Reachable()
	}
	for _, id := range members {
		if id == m.member || slices.Contains(up, id) {
			reach++
		}
	}
	return reach, len(members)
}

// thirdsLocked returns the stack tunnels to the switch members outside the
// domain.
func (m *mclagCtl) thirdsLocked(d *model.Pair) []string {
	var out []string
	for _, id := range m.cfg.SwitchMembers() {
		if !slices.Contains(d.Members[:], id) {
			out = append(out, dataplane.TunnelName(id))
		}
	}
	return out
}

func (m *mclagCtl) run(ctx context.Context) {
	go m.macs.run(ctx)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.step(time.Now())
		}
	}
}

func (m *mclagCtl) step(now time.Time) {
	m.mu.Lock()
	d := m.domainLocked()
	if d == nil {
		// Not in a domain: nothing held, no split horizon.
		held := m.holds
		m.holds, m.split = map[string]string{}, nil
		m.mu.Unlock()
		for b := range held {
			m.lacp.SetHold(b, false)
		}
		if err := dataplane.SyncSplitHorizon(dataplane.SplitHorizon{}); err != nil {
			m.log.Warn("mclag: split horizon", "err", err)
		}
		return
	}
	peer := m.peerOf(d)
	bundles := m.bundlesLocked(d)
	all, allReady := m.lacp.Legs(), m.lacp.Ready()
	legs, ready := map[string]bool{}, map[string]int{}
	for _, b := range bundles {
		legs[b], ready[b] = all[b], allReady[b]
	}
	m.curLegs = legs
	if !m.initDone {
		m.initDone = true
		// After a boot (no leg up yet), legs wait for delay-restore; a
		// restart of switchd with the legs up changes nothing.
		up := false
		for _, v := range legs {
			up = up || v
		}
		if delay := m.cfg.MCLAG.DelayRestore; !up && delay > 0 {
			m.restoreEnd = now.Add(time.Duration(delay) * time.Second)
		}
	}
	reachable := m.peerReachable(peer)
	primary := m.primaryLocked(d)

	// Holds. The peer unreachable: a two-member stack forwards at all
	// costs; with more members, the minority part leaves the bundles to the
	// majority (reference 5.6, failure handling).
	reason := ""
	reach, total := m.reachLocked()
	switch {
	case !reachable && total >= 3 && 2*reach < total:
		reason = fmt.Sprintf("minority part of the stack (reaches %d of %d members)", reach, total)
		m.minority, m.backSince = true, time.Time{}
	case m.minority && reachable:
		if m.backSince.IsZero() {
			m.backSince = now
		}
		if now.Sub(m.backSince) < mclagRejoinAfter {
			reason = "rejoining (MAC tables are exchanged)"
		} else {
			m.minority = false
		}
	default:
		m.minority = false // e.g. the peer is gone, but this part is the majority
	}
	switch {
	case reason != "":
	case m.maint:
		reason = "maintenance mode"
	case now.Before(m.maintEnd):
		reason = "rejoining after maintenance mode (MAC tables are exchanged)"
	}
	if reason == "" && now.Before(m.restoreEnd) {
		reason = fmt.Sprintf("delay-restore (%s left)", m.restoreEnd.Sub(now).Round(time.Second))
	}
	newHolds := map[string]string{}
	facts := map[string]string{}
	for _, b := range bundles {
		facts[b] = bundleFacts(m.cfg, b)
		switch pf, ok := m.peerFacts[b]; {
		case reason == "maintenance mode" && legs[b] && reachable && m.peerLegs[b]:
			// Planned: the peer hears first that this leg goes away, then
			// it leaves the bundle.
			if m.drainFrom[b].IsZero() {
				m.drainFrom[b] = now
				m.leaveTo.Store(b, dataplane.TunnelName(peer))
			}
			if now.Sub(m.drainFrom[b]) >= mclagDrainNotice || m.holds[b] != "" {
				newHolds[b] = reason
			}
		case reason != "":
			newHolds[b] = reason
		case ok && m.peerKnown && pf != facts[b]:
			// Both members apply the same configuration, so a difference is
			// a commit still on its way (ignored) or one that stays, e.g. the
			// peer runs another software version: then the secondary's leg
			// stays out rather than forwarding differently.
			if m.differs[b].IsZero() {
				m.differs[b] = now
				m.log.Info("mclag: bundle differs from the peer", "bundle", b, "here", facts[b], "peer", pf)
			}
			if !primary && now.Sub(m.differs[b]) >= mclagInconsistentAfter {
				newHolds[b] = fmt.Sprintf("inconsistent with the peer: here %s; peer %s", facts[b], pf)
			}
			continue
		}
		delete(m.differs, b)
	}
	for b := range m.differs {
		if !slices.Contains(bundles, b) {
			delete(m.differs, b)
		}
	}
	changedHold := map[string]bool{}
	for _, b := range bundles {
		if (m.holds[b] != "") != (newHolds[b] != "") {
			changedHold[b] = newHolds[b] != ""
		}
	}
	for b := range m.holds {
		if !slices.Contains(bundles, b) {
			changedHold[b] = false
		}
	}
	m.holds = newHolds

	// A leg changed (here or on the peer): addresses learned on the
	// peer's tunnel may be behind a leg that went away; flush them.
	state := fmt.Sprint(legs, m.peerLegs)
	flush := m.legState != "" && state != m.legState
	m.legState = state

	if !m.maint {
		for b := range m.drainFrom {
			m.leaveTo.Delete(b)
		}
		m.drainFrom = map[string]time.Time{}
	}
	var draining []string
	for b := range m.drainFrom {
		legs[b] = false // reported down while it drains
		draining = append(draining, b)
	}
	slices.Sort(draining)

	// Tell the peer about our legs: on change and every second.
	send := reachable && m.stack != nil && (!mapsEqualBool(legs, m.lastLegs) || !maps.Equal(ready, m.lastReady) ||
		now.Sub(m.lastSent) >= time.Second)
	if send {
		m.lastLegs, m.lastReady, m.lastSent = legs, ready, now
	}
	// minimum-links counts the peer's ready ports while it is reachable.
	peerReady := map[string]int{}
	for _, b := range bundles {
		if reachable {
			peerReady[b] = m.peerReady[b]
		}
	}
	domain := d.ID
	peerTunnel := dataplane.TunnelName(peer)
	// Moved addresses stay while a leg drains or is held for maintenance
	// and the peer is there to carry them.
	maintHeld := (m.maint || now.Before(m.maintEnd) || len(m.drainFrom) > 0) && reachable
	m.mu.Unlock()

	for b, n := range peerReady {
		m.lacp.SetPeerReady(b, n)
	}
	for b, h := range changedHold {
		if h {
			// Maintenance: this member's traffic towards the partner takes
			// the peer's leg before the partner stops collecting here.
			m.beforeLeave(b)
		}
		m.lacp.SetHold(b, h)
		if h {
			m.log.Warn("mclag: leg held out of the bundle", "bundle", b, "reason", newHolds[b])
		} else {
			m.log.Info("mclag: leg released", "bundle", b)
		}
	}
	m.installSplit(now)
	m.releaseMoved(maintHeld)
	if flush {
		if n, err := dataplane.FlushLearned(peerTunnel); err == nil && n > 0 {
			m.log.Info("mclag: leg changed, addresses learned on the peer's tunnel flushed", "count", n)
		}
	}
	var installGroups []mcastKey
	if reachable && !m.groups.busy.Load() {
		if changed := !groupsEqual(m.peerGroups, m.groups.applied); changed || now.Sub(m.groups.refreshed) >= mclagGroupRefresh {
			installGroups = slices.Clone(m.peerGroups)
			m.groups.refreshed = now
		}
	}
	if installGroups != nil {
		m.groups.busy.Store(true)
		go m.installGroups(installGroups)
	}
	if send {
		go func() {
			groups := localGroups(bundles)
			if _, err := m.stack.node.Call(peer, "mclag-legs", legsMsg{Domain: domain, Legs: legs, Facts: facts, Ready: ready, Groups: groups, Draining: draining}, time.Second); err != nil {
				m.log.Debug("mclag: leg state to the peer", "err", err)
			}
		}()
	}
}

// installSplit computes the split horizon from the current state and
// installs it. Computing and installing happen under shMu, so the last
// installation always reflects the latest state (a joining announcement
// cannot be overwritten by an older computation).
func (m *mclagCtl) installSplit(now time.Time) {
	m.shMu.Lock()
	defer m.shMu.Unlock()
	m.mu.Lock()
	d := m.domainLocked()
	if d == nil {
		m.mu.Unlock()
		return
	}
	peer := m.peerOf(d)
	bundles := m.bundlesLocked(d)
	in := splitInput{Bundles: bundles, PeerKnown: m.peerKnown, PeerLegs: m.peerLegs, Joining: m.peerJoining,
		Draining: m.peerDraining, Primary: m.primaryLocked(d), Reachable: m.peerReachable(peer)}
	split, drain, df := in.compute(now)
	m.split = split
	sh := dataplane.SplitHorizon{Peer: dataplane.TunnelName(peer), Bundles: split, Draining: drain, Thirds: m.thirdsLocked(d), DF: df}
	m.mu.Unlock()
	if err := dataplane.SyncSplitHorizon(sh); err != nil {
		m.log.Warn("mclag: split horizon", "err", err)
	}
}

// splitInput is what the split horizon depends on (reference 5.6).
type splitInput struct {
	Bundles   []string
	PeerKnown bool
	PeerLegs  map[string]bool
	Joining   map[string]time.Time
	Draining  map[string]bool
	Primary   bool
	Reachable bool
}

// compute returns the bundles whose traffic from the peer's tunnel is
// dropped (split), those where only its broadcast and multicast is dropped
// (drain: the peer's leg leaves for maintenance), and the bundles whose
// broadcast and multicast from third members the primary delivers (df).
//
// The peer's leg counts as up while unknown, while it is up, and while the
// peer announced it as joining: a leg must never forward while this member
// lets the peer's traffic out on its own leg, or the partner's flooded
// frames (BPDUs among them) come back to it through the stack.
func (in splitInput) compute(now time.Time) (split, drain, df []string) {
	for _, b := range in.Bundles {
		up, ok := in.PeerLegs[b]
		joining := !in.Joining[b].IsZero() && now.Sub(in.Joining[b]) < mclagJoinGrace
		switch {
		case !in.PeerKnown || !ok || up || joining:
			split = append(split, b)
		case in.Draining[b]:
			drain = append(drain, b)
		}
		// Broadcast and multicast from third members: the secondary leaves
		// bundles to the primary while the primary's leg is up.
		if !in.Primary && in.PeerKnown && ok && up && in.Reachable {
			df = append(df, b)
		}
	}
	return split, drain, df
}

// beforeJoin announces to the peer that a leg is about to forward
// (lacp.Runtime.BeforeJoin) and waits until the peer filters, at most
// mclagJoinTimeout. Peers of earlier versions do not know the message;
// they learn the leg state with the next regular message, as before.
func (m *mclagCtl) beforeJoin(bundle string) {
	m.mu.Lock()
	d := m.domainLocked()
	if d == nil || !slices.Contains(d.Bundles, bundle) || m.stack == nil {
		m.mu.Unlock()
		return
	}
	peer := m.peerOf(d)
	reachable := m.peerReachable(peer)
	m.mu.Unlock()
	if !reachable {
		return
	}
	if _, err := m.stack.node.Call(peer, "mclag-leg-joining", joiningMsg{Bundle: bundle}, mclagJoinTimeout); err != nil {
		m.log.Debug("mclag: joining leg announced without answer", "bundle", bundle, "err", err)
	}
}

func mapsEqualBool(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// beforeLeave moves what this member's bridge learned on a bundle to the
// peer's tunnel before the bundle's last port leaves in maintenance mode
// (lacp.Runtime.BeforeLeave): traffic to those devices goes through the
// peer's leg at once instead of being lost until the addresses are learned
// again.
func (m *mclagCtl) beforeLeave(bundle string) {
	v, ok := m.leaveTo.Load(bundle)
	if !ok {
		return
	}
	tunnel := v.(string)
	l, err := netlink.LinkByName(bundle)
	if err != nil {
		return
	}
	neighs, err := netlink.NeighList(l.Attrs().Index, unix.AF_BRIDGE)
	if err != nil {
		return
	}
	moved := 0
	for _, n := range neighs {
		if n.Vlan == 0 || n.State&(unix.NUD_PERMANENT|unix.NUD_NOARP) != 0 || len(n.HardwareAddr) != 6 {
			continue
		}
		k := macKey{MAC: macOf(&n), VLAN: n.Vlan}
		if fdbSet(tunnel, k) == nil {
			moved++
			m.movedMu.Lock()
			if m.moved == nil {
				m.moved = map[macKey]string{}
			}
			m.moved[k] = tunnel
			m.movedMu.Unlock()
		}
	}
	if moved > 0 {
		m.log.Info("mclag: addresses behind the leg moved to the peer before it leaves", "bundle", bundle, "count", moved)
	}
}

// releaseMoved removes the addresses beforeLeave moved, once no leg of
// this member drains and the peer's tunnel is no longer the way (the leg is
// back, or the peer is gone): the bridge learns them again or floods.
func (m *mclagCtl) releaseMoved(draining bool) {
	if draining {
		return
	}
	m.movedMu.Lock()
	moved := m.moved
	m.moved = nil
	m.movedMu.Unlock()
	if len(moved) == 0 {
		return
	}
	m.macs.mu.Lock()
	installed := maps.Clone(m.macs.installed)
	m.macs.mu.Unlock()
	n := 0
	for k, tunnel := range moved {
		if installed[k] == tunnel {
			continue // MAC synchronisation installed it there
		}
		if fdbDel(tunnel, k) == nil {
			n++
		}
	}
	if n > 0 {
		m.log.Info("mclag: addresses moved to the peer for maintenance mode released", "count", n)
	}
}

// setMaintenance holds (on) or, after mclagRejoinAfter, releases the legs.
func (m *mclagCtl) setMaintenance(on bool, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maint && !on {
		m.maintEnd = now.Add(mclagRejoinAfter)
	}
	m.maint = on
}

// legsUp lists this member's MC-LAG legs that carry traffic.
func (m *mclagCtl) legsUp() []string {
	m.mu.Lock()
	d := m.domainLocked()
	var bundles []string
	if d != nil {
		bundles = m.bundlesLocked(d)
	}
	m.mu.Unlock()
	all := m.lacp.Legs()
	var out []string
	for _, b := range bundles {
		if all[b] {
			out = append(out, b)
		}
	}
	return out
}

// drainBlockers explains why holding this member's legs would cut traffic:
// a bundle whose leg on the peer is down, or a peer in maintenance mode.
func (m *mclagCtl) drainBlockers(draining []int) []string {
	up := m.legsUp()
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.domainLocked()
	if d == nil || len(up) == 0 {
		return nil
	}
	peer := m.peerOf(d)
	if slices.Contains(draining, peer) {
		return []string{fmt.Sprintf("the MC-LAG peer (member %d) is in maintenance mode", peer)}
	}
	if !m.peerReachable(peer) {
		return []string{fmt.Sprintf("the MC-LAG peer (member %d) is not reachable, draining would cut %s", peer, strings.Join(up, ", "))}
	}
	var out []string
	for _, b := range up {
		if pl, ok := m.peerLegs[b]; !m.peerKnown || !ok || !pl {
			out = append(out, b+": the peer's leg is down, draining would cut the bundle")
		}
	}
	return out
}

// status is "show mclag".
func (m *mclagCtl) status() ([]cli.MCLAGStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.domainLocked()
	if d == nil {
		return nil, nil
	}
	peer := m.peerOf(d)
	st := cli.MCLAGStatus{Pair: d.ID, Member: m.member, Peer: peer, Primary: m.primaryLocked(d),
		PeerReachable: m.peerReachable(peer), PeerKnown: m.peerKnown, PeerSeen: m.peerSeen}
	st.Reach, st.Members = m.reachLocked()
	legs := m.lacp.Legs()
	for _, b := range m.bundlesLocked(d) {
		pl, ok := m.peerLegs[b]
		st.Bundles = append(st.Bundles, cli.MCLAGBundle{Name: b, LocalUp: legs[b], PeerUp: pl, PeerKnown: m.peerKnown && ok,
			SplitHorizon: slices.Contains(m.split, b), Hold: m.holds[b],
			Facts: bundleFacts(m.cfg, b), PeerFacts: m.peerFacts[b], DiffersSince: m.differs[b]})
	}
	return []cli.MCLAGStatus{st}, nil
}

func groupsEqual(list []mcastKey, set map[mcastKey]bool) bool {
	if len(list) != len(set) {
		return false
	}
	for _, k := range list {
		if !set[k] {
			return false
		}
	}
	return true
}

// localGroups lists the groups this member learned on its MC-LAG bundles
// (not those it installed for the peer: those are refreshed from the peer).
func localGroups(bundles []string) []mcastKey {
	es, _, err := dataplane.McastGroups()
	if err != nil {
		return nil
	}
	var out []mcastKey
	for _, e := range es {
		if !e.Permanent && slices.Contains(bundles, e.Port) {
			out = append(out, mcastKey{Bundle: e.Port, VID: e.VID, Group: e.Group})
		}
	}
	slices.SortFunc(out, func(a, b mcastKey) int {
		return strings.Compare(fmt.Sprint(a.Bundle, a.VID, a.Group), fmt.Sprint(b.Bundle, b.VID, b.Group))
	})
	return out
}

// installGroups installs (refreshes) the peer's groups on this member's
// legs (reference 5.5). Groups the peer no longer reports are left to
// expire.
func (m *mclagCtl) installGroups(keys []mcastKey) {
	defer m.groups.busy.Store(false)
	applied := map[mcastKey]bool{}
	for _, k := range keys {
		if err := dataplane.McastRefresh(k.Bundle, k.VID, k.Group); err != nil {
			m.log.Debug("mclag: multicast group of the peer", "bundle", k.Bundle, "group", k.Group, "err", err)
		}
		applied[k] = true // (a failure is retried with the next refresh)
	}
	m.mu.Lock()
	m.groups.applied = applied
	m.mu.Unlock()
}
