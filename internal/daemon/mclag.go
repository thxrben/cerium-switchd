package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
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
	wasHeld    bool      // held for the peer-link: delay-restore when it returns
	holds      map[string]string
	split      []string
	lastSent   time.Time
	lastLegs   map[string]bool
	initDone   bool
	legState   string          // local and peer legs as last seen (flush on change)
	curLegs    map[string]bool // local legs, current
	macs       *macSync
	bfd        *peerBFD
	linux      func(string) (string, bool) // port name -> kernel name
	bfdNames   map[string]string           // kernel name -> port name (peer-link ports)
}

// legsMsg is the leg state a member sends its peer.
type legsMsg struct {
	Domain int             `json:"domain"`
	Legs   map[string]bool `json:"legs"`
}

func newMCLAG(member int, rt *lacp.Runtime, stack *stackCtl, linux func(string) (string, bool), log *slog.Logger) *mclagCtl {
	m := &mclagCtl{member: member, lacp: rt, stack: stack, sysRoot: "/sys", log: log, started: time.Now(), holds: map[string]string{},
		bfd: newPeerBFD(), linux: linux, bfdNames: map[string]string{}}
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
		if i.AE && i.MCLAG && slices.Contains(i.MemberIDs, m.member) && n != d.PeerLink {
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

func (m *mclagCtl) carrier(dev string) bool {
	b, err := os.ReadFile(filepath.Join(m.sysRoot, "class", "net", dev, "carrier"))
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

func (m *mclagCtl) peerReachable(peer int) bool {
	return m.stack != nil && slices.Contains(m.stack.node.Mesh.Reachable(), peer)
}

func (m *mclagCtl) run(ctx context.Context) {
	go m.macs.run(ctx)
	go m.bfd.run(ctx)
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
		m.bfd.configure(nil, 0, 0, 0, 0, 0)
		for b := range held {
			m.lacp.SetHold(b, false)
		}
		if err := dataplane.SyncSplitHorizon("", nil); err != nil {
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
	// Micro-BFD on the peer-link's ports of this member.
	var bfdPorts []string
	m.bfdNames = map[string]string{}
	for n, i := range m.cfg.Interfaces {
		if i.Parent == d.PeerLink && i.Member == m.member {
			if l, ok := m.linux(n); ok {
				bfdPorts = append(bfdPorts, l)
				m.bfdNames[l] = n
			}
		}
	}
	m.bfd.configure(bfdPorts, d.ID, m.member, peer, d.PeerLinkBFD.IntervalMS, d.PeerLinkBFD.Multiplier)
	bfdUp, _ := m.bfd.state(now)
	reachable := m.peerReachable(peer)
	peerLinkUp := m.carrier(d.PeerLink) && bfdUp
	primary := m.primaryLocked(d)

	// Holds.
	reason := ""
	switch {
	case !peerLinkUp && reachable && !primary:
		reason = "peer-link down, peer alive (secondary)"
		m.wasHeld = true
	case m.wasHeld && peerLinkUp:
		// Cut off from the peer, now back: delay-restore first.
		m.wasHeld = false
		if d.DelayRestore > 0 {
			m.restoreEnd = now.Add(time.Duration(d.DelayRestore) * time.Second)
		}
	}
	if reason == "" && now.Before(m.restoreEnd) {
		reason = fmt.Sprintf("delay-restore (%s left)", m.restoreEnd.Sub(now).Round(time.Second))
	}
	newHolds := map[string]string{}
	for _, b := range bundles {
		if reason != "" {
			newHolds[b] = reason
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

	// A leg changed (here or on the peer): addresses learned on the
	// peer-link may be behind a leg that went away; flush them.
	state := fmt.Sprint(legs, m.peerLegs)
	flush := m.legState != "" && state != m.legState
	m.legState = state

	// Tell the peer about our legs: on change and every second.
	send := reachable && m.stack != nil && (!mapsEqualBool(legs, m.lastLegs) || now.Sub(m.lastSent) >= time.Second)
	if send {
		m.lastLegs, m.lastSent = legs, now
	}
	domain := d.ID
	peerLink := d.PeerLink
	m.mu.Unlock()

	for b, h := range changedHold {
		m.lacp.SetHold(b, h)
		if h {
			m.log.Warn("mclag: leg held out of the bundle", "bundle", b, "reason", newHolds[b])
		} else {
			m.log.Info("mclag: leg released", "bundle", b)
		}
	}
	if err := dataplane.SyncSplitHorizon(peerLink, split); err != nil {
		m.log.Warn("mclag: split horizon", "err", err)
	}
	if flush {
		if n, err := dataplane.FlushLearned(peerLink); err != nil {
			m.log.Warn("mclag: flushing the peer-link's addresses", "err", err)
		} else if n > 0 {
			m.log.Info("mclag: leg changed, addresses learned on the peer-link flushed", "count", n)
		}
	}
	if send {
		go func() {
			if _, err := m.stack.node.Call(peer, "mclag-legs", legsMsg{Domain: domain, Legs: legs}, time.Second); err != nil {
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
		PeerReachable: m.peerReachable(peer), PeerLink: d.PeerLink,
		PeerKnown: m.peerKnown, PeerSeen: m.peerSeen}
	bfdUp, ports := m.bfd.state(time.Now())
	st.PeerLinkUp = m.carrier(d.PeerLink) && bfdUp
	for _, l := range sortedBool(ports) {
		name := m.bfdNames[l]
		if name == "" {
			name = l
		}
		st.PeerLinkPorts = append(st.PeerLinkPorts, cli.MCLAGPort{Name: name, Up: ports[l]})
	}
	legs := m.lacp.Legs()
	for _, b := range m.bundlesLocked(d) {
		pl, ok := m.peerLegs[b]
		st.Bundles = append(st.Bundles, cli.MCLAGBundle{Name: b, LocalUp: legs[b], PeerUp: pl, PeerKnown: m.peerKnown && ok,
			SplitHorizon: slices.Contains(m.split, b), Hold: m.holds[b]})
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
