package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"mclag/internal/cli"
	"mclag/internal/dataplane"
	"mclag/internal/lacp"
	"mclag/internal/model"
)

// mclagCtl runs this member's side of its MC-LAG domain (reference 5.6):
// it exchanges leg states with the peer over the stacking plane, holds
// legs out of their bundles when needed and keeps the split horizon.
type mclagCtl struct {
	member  int
	lacp    *lacp.Runtime
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
	initDone   bool
	legState   string          // local and peer legs as last seen (flush on change)
	curLegs    map[string]bool // local legs, current
	macs       *macSync
	peerFacts  map[string]string    // the peer's bundle facts (nil: not sent)
	differs    map[string]time.Time // bundle -> facts differ since
}

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
}

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

func newMCLAG(member int, rt *lacp.Runtime, stack *stackCtl, log *slog.Logger) *mclagCtl {
	m := &mclagCtl{member: member, lacp: rt, stack: stack, sysRoot: "/sys", log: log, started: time.Now(), holds: map[string]string{}, differs: map[string]time.Time{}}
	m.macs = newMACSync(m, log)
	if stack != nil {
		stack.node.Handle("mclag-legs", func(from int, req json.RawMessage) (any, error) {
			var l legsMsg
			if err := json.Unmarshal(req, &l); err != nil {
				return nil, err
			}
			m.mu.Lock()
			if d := m.domainLocked(); d != nil && d.ID == l.Domain && m.peerOf(d) == from {
				m.peerLegs, m.peerSeen, m.peerKnown = l.Legs, time.Now(), true
				m.peerFacts = l.Facts
			}
			m.mu.Unlock()
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

func (m *mclagCtl) domainLocked() *model.Domain {
	if m.cfg == nil {
		return nil
	}
	for _, d := range m.cfg.Domains {
		if slices.Contains(d.Members, m.member) {
			return d
		}
	}
	return nil
}

func (m *mclagCtl) peerOf(d *model.Domain) int {
	for _, id := range d.Members {
		if id != m.member {
			return id
		}
	}
	return 0
}

// bundlesLocked lists the domain's MC-LAG bundles with a leg on this member.
func (m *mclagCtl) bundlesLocked(d *model.Domain) []string {
	var out []string
	for n, i := range m.cfg.Interfaces {
		if i.AE && i.MCLAG && slices.Contains(i.MemberIDs, m.member) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// primary: the member with the higher mastership-priority, ties: lower id.
func (m *mclagCtl) primaryLocked(d *model.Domain) bool {
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
func (m *mclagCtl) thirdsLocked(d *model.Domain) []string {
	var out []string
	for _, id := range m.cfg.SwitchMembers() {
		if !slices.Contains(d.Members, id) {
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
	all := m.lacp.Legs()
	legs := map[string]bool{}
	for _, b := range bundles {
		legs[b] = all[b]
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
		if !up && d.DelayRestore > 0 {
			m.restoreEnd = now.Add(time.Duration(d.DelayRestore) * time.Second)
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
	if reason == "" && now.Before(m.restoreEnd) {
		reason = fmt.Sprintf("delay-restore (%s left)", m.restoreEnd.Sub(now).Round(time.Second))
	}
	newHolds := map[string]string{}
	facts := map[string]string{}
	for _, b := range bundles {
		facts[b] = bundleFacts(m.cfg, b)
		switch pf, ok := m.peerFacts[b]; {
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

	// Split horizon: while the peer's leg is up (unknown counts as up).
	var split []string
	for _, b := range bundles {
		if up, ok := m.peerLegs[b]; !m.peerKnown || !ok || up {
			split = append(split, b)
		}
	}
	m.split = split

	// Broadcast and multicast from third members: the secondary leaves
	// bundles to the primary while the primary's leg is up.
	var df []string
	if !primary {
		for _, b := range bundles {
			if up, ok := m.peerLegs[b]; m.peerKnown && ok && up && reachable {
				df = append(df, b)
			}
		}
	}
	sh := dataplane.SplitHorizon{Peer: dataplane.TunnelName(peer), Bundles: split, Thirds: m.thirdsLocked(d), DF: df}

	// A leg changed (here or on the peer): addresses learned on the
	// peer's tunnel may be behind a leg that went away; flush them.
	state := fmt.Sprint(legs, m.peerLegs)
	flush := m.legState != "" && state != m.legState
	m.legState = state

	// Tell the peer about our legs: on change and every second.
	send := reachable && m.stack != nil && (!mapsEqualBool(legs, m.lastLegs) || now.Sub(m.lastSent) >= time.Second)
	if send {
		m.lastLegs, m.lastSent = legs, now
	}
	domain := d.ID
	peerTunnel := dataplane.TunnelName(peer)
	m.mu.Unlock()

	for b, h := range changedHold {
		m.lacp.SetHold(b, h)
		if h {
			m.log.Warn("mclag: leg held out of the bundle", "bundle", b, "reason", newHolds[b])
		} else {
			m.log.Info("mclag: leg released", "bundle", b)
		}
	}
	if err := dataplane.SyncSplitHorizon(sh); err != nil {
		m.log.Warn("mclag: split horizon", "err", err)
	}
	if flush {
		if n, err := dataplane.FlushLearned(peerTunnel); err == nil && n > 0 {
			m.log.Info("mclag: leg changed, addresses learned on the peer's tunnel flushed", "count", n)
		}
	}
	if send {
		go func() {
			if _, err := m.stack.node.Call(peer, "mclag-legs", legsMsg{Domain: domain, Legs: legs, Facts: facts}, time.Second); err != nil {
				m.log.Debug("mclag: leg state to the peer", "err", err)
			}
		}()
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

// status is "show mclag".
func (m *mclagCtl) status() (cli.MCLAGStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.domainLocked()
	if d == nil {
		return cli.MCLAGStatus{}, nil
	}
	peer := m.peerOf(d)
	st := cli.MCLAGStatus{Domain: d.ID, Member: m.member, Peer: peer, Primary: m.primaryLocked(d),
		PeerReachable: m.peerReachable(peer), PeerKnown: m.peerKnown, PeerSeen: m.peerSeen}
	st.Reach, st.Members = m.reachLocked()
	legs := m.lacp.Legs()
	for _, b := range m.bundlesLocked(d) {
		pl, ok := m.peerLegs[b]
		st.Bundles = append(st.Bundles, cli.MCLAGBundle{Name: b, LocalUp: legs[b], PeerUp: pl, PeerKnown: m.peerKnown && ok,
			SplitHorizon: slices.Contains(m.split, b), Hold: m.holds[b],
			Facts: bundleFacts(m.cfg, b), PeerFacts: m.peerFacts[b], DiffersSince: m.differs[b]})
	}
	return st, nil
}

// mclagSystem is the shared LACP system id of a domain (reference 5.6):
// the configured system-mac, or one derived from the stack and domain id.
func mclagSystem(d *model.Domain, stackID string) lacp.SystemID {
	sys := lacp.SystemID{Priority: uint16(d.SystemPriority)}
	if sys.Priority == 0 {
		sys.Priority = 32768
	}
	if mac, err := net.ParseMAC(d.SystemMAC); err == nil && len(mac) == 6 {
		copy(sys.MAC[:], mac)
		return sys
	}
	sum := sha256.Sum256([]byte("mclag domain\x00" + stackID + "\x00" + strconv.Itoa(d.ID)))
	copy(sys.MAC[:], sum[:6])
	sys.MAC[0] = sys.MAC[0]&^0x01 | 0x02
	return sys
}
