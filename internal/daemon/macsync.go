package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"mclag/internal/dataplane"
)

// MAC synchronisation between the two members of an MC-LAG domain
// (reference 5.6): what one member's bridge learns on its own ports is
// installed on the peer, on the same MC-LAG bundle or on the peer-link.

type macKey struct {
	MAC  string `json:"mac"` // lower case, colon separated
	VLAN int    `json:"vlan"`
}

// macMsg carries changes (or, with Full, the whole table) to the peer.
type macMsg struct {
	Domain int      `json:"domain"`
	Full   bool     `json:"full,omitempty"`
	Adds   []macAdd `json:"adds,omitempty"`
	Dels   []macKey `json:"dels,omitempty"`
}

type macAdd struct {
	macKey
	Bundle string `json:"bundle,omitempty"` // MC-LAG bundle ("": a single-homed port)
}

const (
	macFullSyncEvery = 30 * time.Second
	macRemoteMaxAge  = 5 * time.Minute // peer gone this long: its addresses are dropped
)

type macSync struct {
	m   *mclagCtl
	log *slog.Logger

	mu        sync.Mutex
	local     map[macKey]string // learned here: key -> origin ("" single-homed, else the bundle)
	remote    map[macKey]string // from the peer: key -> origin
	installed map[macKey]string // installed here: key -> device
	adds      map[macKey]string // pending changes for the peer
	dels      map[macKey]bool
	lastFull  time.Time
	wasReach  bool
	lostSince time.Time
}

func newMACSync(m *mclagCtl, log *slog.Logger) *macSync {
	s := &macSync{m: m, log: log, local: map[macKey]string{}, remote: map[macKey]string{}, installed: map[macKey]string{},
		adds: map[macKey]string{}, dels: map[macKey]bool{}}
	if m.stack != nil {
		m.stack.node.Handle("macsync", func(from int, req json.RawMessage) (any, error) {
			var msg macMsg
			if err := json.Unmarshal(req, &msg); err != nil {
				return nil, err
			}
			return nil, s.receive(from, msg)
		})
	}
	return s
}

// domainView is what MAC sync needs from the MC-LAG controller.
type domainView struct {
	domain   int
	peer     int
	peerLink string
	bundles  []string        // MC-LAG bundles with a leg here
	legs     map[string]bool // local legs up
	linkUp   bool            // peer-link
	reach    bool            // peer over the stacking plane
}

func (s *macSync) view() (domainView, bool) {
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.domainLocked()
	if d == nil {
		return domainView{}, false
	}
	v := domainView{domain: d.ID, peer: m.peerOf(d), peerLink: d.PeerLink, bundles: m.bundlesLocked(d), legs: map[string]bool{}}
	for b, up := range m.curLegs {
		v.legs[b] = up
	}
	v.linkUp = m.carrier(d.PeerLink)
	v.reach = m.peerReachable(v.peer)
	return v, true
}

func macOf(n *netlink.Neigh) string { return strings.ToLower(n.HardwareAddr.String()) }

// run watches the bridge's address table and keeps both sides in sync.
func (s *macSync) run(ctx context.Context) {
	var updates chan netlink.NeighUpdate
	var done chan struct{}
	subscribe := func() {
		if done != nil {
			close(done)
		}
		updates, done = make(chan netlink.NeighUpdate, 4096), make(chan struct{})
		if err := netlink.NeighSubscribeWithOptions(updates, done, netlink.NeighSubscribeOptions{
			ListExisting:  true,
			ErrorCallback: func(err error) { s.log.Debug("mclag: address table events", "err", err) },
		}); err != nil {
			s.log.Warn("mclag: address table events", "err", err)
			updates = nil
		}
	}
	subscribe()
	defer func() { close(done) }()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case u, ok := <-updates:
			if !ok {
				// The subscription ended (e.g. the socket overflowed under a
				// burst of changes): events were lost, so start again and
				// resend the whole table.
				subscribe()
				s.mu.Lock()
				s.lastFull = time.Time{}
				s.mu.Unlock()
				continue
			}
			s.event(u)
		case <-t.C:
			if updates == nil {
				subscribe()
			}
			s.tick(time.Now())
		}
	}
}

// event handles an address table change of the bridge.
func (s *macSync) event(u netlink.NeighUpdate) {
	n := u.Neigh
	if n.Family != unix.AF_BRIDGE || n.Vlan == 0 || n.MasterIndex == 0 || len(n.HardwareAddr) != 6 {
		return
	}
	if br, err := netlink.LinkByIndex(n.MasterIndex); err != nil || br.Attrs().Name != dataplane.BridgeName {
		return
	}
	if n.State&(unix.NUD_PERMANENT|unix.NUD_NOARP) != 0 {
		return // local and static entries
	}
	v, ok := s.view()
	if !ok {
		return
	}
	port, err := netlink.LinkByIndex(n.LinkIndex)
	if err != nil {
		return
	}
	name := port.Attrs().Name
	key := macKey{MAC: macOf(&n), VLAN: n.Vlan}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n.Flags&netlink.NTF_EXT_LEARNED != 0 {
		return // ours (installed from the peer)
	}
	if u.Type == unix.RTM_DELNEIGH {
		if _, had := s.local[key]; had {
			delete(s.local, key)
			delete(s.adds, key)
			s.dels[key] = true
		}
		delete(s.installed, key) // gone either way
		return
	}
	if name == v.peerLink {
		return // (learning is off there)
	}
	origin := ""
	if slices.Contains(v.bundles, name) {
		origin = name
	}
	// The bridge learned it here: it replaced anything we installed.
	delete(s.installed, key)
	if cur, had := s.local[key]; !had || cur != origin {
		s.local[key] = origin
		s.adds[key] = origin
		delete(s.dels, key)
	}
}

// receive applies the peer's changes.
func (s *macSync) receive(from int, msg macMsg) error {
	v, ok := s.view()
	if !ok || v.domain != msg.Domain || v.peer != from {
		// E.g. this member has not applied the domain yet: the sender
		// tries again.
		return fmt.Errorf("not in MC-LAG domain %d with member %d", msg.Domain, from)
	}
	s.mu.Lock()
	if msg.Full {
		keep := map[macKey]bool{}
		for _, a := range msg.Adds {
			keep[a.macKey] = true
		}
		for k := range s.remote {
			if !keep[k] {
				delete(s.remote, k)
			}
		}
	}
	for _, a := range msg.Adds {
		s.remote[a.macKey] = a.Bundle
	}
	for _, k := range msg.Dels {
		delete(s.remote, k)
	}
	s.mu.Unlock()
	s.reconcile(v)
	return nil
}

// target is where a peer's address belongs here ("": nowhere).
func (s *macSync) target(v domainView, origin string) string {
	if origin != "" && v.legs[origin] {
		return origin
	}
	if v.linkUp {
		return v.peerLink
	}
	return ""
}

// reconcile installs and removes the peer's addresses.
func (s *macSync) reconcile(v domainView) {
	s.mu.Lock()
	type change struct {
		key      macKey
		old, new string
	}
	var changes []change
	for k, origin := range s.remote {
		want := ""
		if _, learned := s.local[k]; !learned {
			want = s.target(v, origin)
		}
		if have := s.installed[k]; have != want {
			changes = append(changes, change{k, have, want})
		}
	}
	for k, have := range s.installed {
		if _, ok := s.remote[k]; !ok {
			changes = append(changes, change{k, have, ""})
		}
	}
	s.mu.Unlock()
	for _, c := range changes {
		var err error
		if c.new == "" {
			err = fdbDel(c.old, c.key)
		} else {
			err = fdbSet(c.new, c.key)
		}
		s.mu.Lock()
		if err == nil {
			if c.new == "" {
				delete(s.installed, c.key)
			} else {
				s.installed[c.key] = c.new
			}
		}
		s.mu.Unlock()
		if err != nil {
			s.log.Debug("mclag: address from the peer", "mac", c.key.MAC, "vlan", c.key.VLAN, "dev", c.new, "err", err)
		}
	}
}

func (s *macSync) tick(now time.Time) {
	v, ok := s.view()
	if !ok {
		s.clear()
		return
	}
	s.mu.Lock()
	full := v.reach && (!s.wasReach || now.Sub(s.lastFull) >= macFullSyncEvery)
	s.mu.Unlock()
	if full {
		// Addresses learned before the domain existed (or while events were
		// lost) produce no new events: take the table as it is.
		s.rescan(v)
	}
	s.mu.Lock()
	if !v.reach && s.wasReach {
		s.lostSince = now
	}
	if !v.reach && !s.lostSince.IsZero() && now.Sub(s.lostSince) > macRemoteMaxAge {
		s.remote = map[macKey]string{} // the peer is gone for good
		s.lostSince = time.Time{}
	}
	s.wasReach = v.reach
	var msg macMsg
	if full {
		s.lastFull = now
		msg = macMsg{Domain: v.domain, Full: true}
		for k, o := range s.local {
			msg.Adds = append(msg.Adds, macAdd{k, o})
		}
		s.adds, s.dels = map[macKey]string{}, map[macKey]bool{}
	} else if v.reach && (len(s.adds) > 0 || len(s.dels) > 0) {
		msg = macMsg{Domain: v.domain}
		for k, o := range s.adds {
			msg.Adds = append(msg.Adds, macAdd{k, o})
		}
		for k := range s.dels {
			msg.Dels = append(msg.Dels, k)
		}
		s.adds, s.dels = map[macKey]string{}, map[macKey]bool{}
	}
	s.mu.Unlock()
	if msg.Domain != 0 {
		go func() {
			if _, err := s.m.stack.node.Call(v.peer, "macsync", msg, 2*time.Second); err != nil {
				s.log.Debug("mclag: addresses to the peer", "err", err)
				// Resend everything once the peer accepts it again.
				s.mu.Lock()
				s.lastFull = time.Time{}
				s.mu.Unlock()
			}
		}()
	}
	s.reconcile(v)
}

// clear removes everything installed (not in a domain any more).
func (s *macSync) clear() {
	s.mu.Lock()
	inst := s.installed
	s.local, s.remote, s.installed = map[macKey]string{}, map[macKey]string{}, map[macKey]string{}
	s.adds, s.dels = map[macKey]string{}, map[macKey]bool{}
	// A domain configured again starts with a full exchange.
	s.wasReach, s.lastFull, s.lostSince = false, time.Time{}, time.Time{}
	s.mu.Unlock()
	for k, dev := range inst {
		fdbDel(dev, k)
	}
}

func fdbNeigh(dev string, k macKey) (*netlink.Neigh, error) {
	l, err := netlink.LinkByName(dev)
	if err != nil {
		return nil, err
	}
	mac, err := net.ParseMAC(k.MAC)
	if err != nil {
		return nil, err
	}
	return &netlink.Neigh{LinkIndex: l.Attrs().Index, Family: unix.AF_BRIDGE, State: netlink.NUD_REACHABLE,
		Flags: netlink.NTF_MASTER | netlink.NTF_EXT_LEARNED, HardwareAddr: mac, Vlan: k.VLAN}, nil
}

// fdbSet installs (or moves) an externally learned address: it does not
// age; the bridge replaces it if it learns the address itself.
func fdbSet(dev string, k macKey) error {
	n, err := fdbNeigh(dev, k)
	if err != nil {
		return err
	}
	return netlink.NeighSet(n)
}

func fdbDel(dev string, k macKey) error {
	n, err := fdbNeigh(dev, k)
	if err != nil {
		return err
	}
	return netlink.NeighDel(n)
}

// rescan rebuilds the table of locally learned addresses from the bridge.
func (s *macSync) rescan(v domainView) {
	br, err := netlink.LinkByName(dataplane.BridgeName)
	if err != nil {
		return
	}
	neighs, err := netlink.NeighList(0, unix.AF_BRIDGE)
	if err != nil {
		return
	}
	names := map[int]string{}
	local := map[macKey]string{}
	for _, n := range neighs {
		if n.MasterIndex != br.Attrs().Index || n.Vlan == 0 || len(n.HardwareAddr) != 6 ||
			n.State&(unix.NUD_PERMANENT|unix.NUD_NOARP) != 0 || n.Flags&netlink.NTF_EXT_LEARNED != 0 {
			continue
		}
		name, ok := names[n.LinkIndex]
		if !ok {
			if l, err := netlink.LinkByIndex(n.LinkIndex); err == nil {
				name = l.Attrs().Name
			}
			names[n.LinkIndex] = name
		}
		if name == "" || name == v.peerLink {
			continue
		}
		origin := ""
		if slices.Contains(v.bundles, name) {
			origin = name
		}
		local[macKey{MAC: macOf(&n), VLAN: n.Vlan}] = origin
	}
	s.mu.Lock()
	s.local = local
	for k := range local {
		delete(s.installed, k) // the bridge's own entry replaced ours
	}
	s.mu.Unlock()
}
