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

	"github.com/thxrben/cerium-switchd/internal/dataplane"
)

// MAC synchronisation between the two members of an MC-LAG pair
// (reference 5.6): what one member's bridge learns on its own ports is
// installed on the peer, on the same MC-LAG bundle or on the peer's stack
// tunnel. Addresses behind a bundle that one member loses are forgotten by
// the other members (third members of the stack), so their traffic floods
// and reaches the device through the remaining leg.

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
	forget    []macKey // addresses behind a bundle removed here: for third members
}

// forgetMsg asks a third member to forget addresses it learned on the
// sender's tunnel.
type forgetMsg struct {
	Keys []macKey `json:"keys"`
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
		m.stack.node.Handle("mac-forget", func(from int, req json.RawMessage) (any, error) {
			var msg forgetMsg
			if err := json.Unmarshal(req, &msg); err != nil {
				return nil, err
			}
			n := 0
			for _, k := range msg.Keys {
				if forgetLearned(dataplane.TunnelName(from), k) == nil {
					n++
				}
			}
			if n > 0 {
				s.log.Debug("mclag: forgot addresses behind a lost leg", "member", from, "count", n)
			}
			return nil, nil
		})
	}
	return s
}

// forgetLearned removes an address the bridge learned on dev (if it is
// there; entries elsewhere or installed from outside stay).
func forgetLearned(dev string, k macKey) error {
	l, err := netlink.LinkByName(dev)
	if err != nil {
		return err
	}
	mac, err := net.ParseMAC(k.MAC)
	if err != nil {
		return err
	}
	return netlink.NeighDel(&netlink.Neigh{LinkIndex: l.Attrs().Index, Family: unix.AF_BRIDGE, Flags: netlink.NTF_MASTER,
		HardwareAddr: mac, Vlan: k.VLAN})
}

// domainView is what MAC sync needs from the MC-LAG controller.
type domainView struct {
	domain     int
	peer       int
	peerTunnel string
	bundles    []string        // MC-LAG bundles with a leg here
	legs       map[string]bool // local legs up
	reach      bool            // peer over the stack (and so its tunnel)
	thirds     []int           // switch members outside the domain
}

func (s *macSync) view() (domainView, bool) {
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.domainLocked()
	if d == nil {
		return domainView{}, false
	}
	v := domainView{domain: d.ID, peer: m.peerOf(d), bundles: m.bundlesLocked(d), legs: map[string]bool{}}
	v.peerTunnel = dataplane.TunnelName(v.peer)
	for b, up := range m.curLegs {
		v.legs[b] = up
	}
	v.reach = m.peerReachable(v.peer)
	for _, id := range m.cfg.SwitchMembers() {
		if !slices.Contains(d.Members[:], id) {
			v.thirds = append(v.thirds, id)
		}
	}
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
		if origin, had := s.local[key]; had {
			delete(s.local, key)
			delete(s.adds, key)
			s.dels[key] = true
			if origin != "" && len(v.thirds) > 0 {
				s.forget = append(s.forget, key)
			}
		}
		delete(s.installed, key) // gone either way
		return
	}
	if dataplane.TunnelMember(name) > 0 || dataplane.VXLANVNI(name) > 0 {
		// Learned from another member: each member learns those itself
		// (the peer's tunnel does not learn). Remote VTEPs' addresses are
		// distributed by vxlanSync.
		return
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
	if !ok || v.peer != from {
		// E.g. this member has not applied the bundles yet: the sender
		// tries again. (The pair id is not compared: older versions sent
		// their configured domain id.)
		return fmt.Errorf("no MC-LAG with member %d", from)
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
	if v.reach {
		return v.peerTunnel
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
	forget := s.forget
	s.forget = nil
	s.mu.Unlock()
	if len(forget) > 0 && s.m.stack != nil {
		for _, id := range v.thirds {
			go func() {
				if _, err := s.m.stack.node.Call(id, "mac-forget", forgetMsg{Keys: forget}, 2*time.Second); err != nil {
					s.log.Debug("mclag: addresses to forget", "member", id, "err", err)
				}
			}()
		}
	}
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
		if name == "" || dataplane.TunnelMember(name) > 0 || dataplane.VXLANVNI(name) > 0 {
			continue // (VXLAN: every member installs those itself, vxlanSync)
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
