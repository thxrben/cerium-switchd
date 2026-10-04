package bgp

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"time"
)

// Transport makes the connections to neighbours (TCP MD5, TTL, the
// instance's VRF and source address are its business).
type Transport interface {
	Dial(ctx context.Context, n Neighbor) (net.Conn, error)
}

// Policy are the program's import and export policies. Import may change
// the path (local preference, communities, ...) and returns false to
// reject it (the path stays in the Adj-RIB-In as hidden). Export sees a
// copy of the path to announce to n and may change it the same way;
// for originated paths it decides whether they are announced at all.
type Policy struct {
	Import func(n *Neighbor, p *Path) bool
	Export func(n *Neighbor, p *Path) bool
}

// Speaker is the BGP speaker of one routing instance. Everything runs on
// its event loop (Run).
type Speaker struct {
	Transport Transport
	Log       *slog.Logger
	// OnRoutes receives the whole BGP table after changes (at most every
	// RoutesDelay): every usable path, ranked; converged as Converged. It
	// runs on the event loop and must not block.
	OnRoutes    func(rs []Route, converged bool)
	RoutesDelay time.Duration

	events chan func()
	pol    Policy

	// Owned by the event loop.
	cfg           Config
	peers         map[netip.Addr]*peer
	local         map[netip.Prefix]*Path // originated (exports from the routing table)
	inactive      map[netip.Prefix]bool  // BGP's route is not the active one there
	best          map[netip.Prefix]*dest
	dirty         map[netip.Prefix]bool
	started       time.Time
	routesAt      time.Time
	routesDirty   bool
	lastConverged bool
	clusterIDs    map[netip.Addr]bool
}

// Route is one usable path of a prefix: Rank 0 for the best path and the
// paths used with it (multipath), then 1, 2, ... for the others.
type Route struct {
	Path
	Rank int
}

// dest is the decision result of a prefix.
type dest struct {
	paths []*Path // usable, best first
	used  int     // paths[:used] are installed (multipath)
}

// New returns a speaker; Configure gives it its configuration, Run runs
// it.
func New(t Transport, pol Policy, log *slog.Logger) *Speaker {
	if log == nil {
		log = slog.Default()
	}
	return &Speaker{Transport: t, Log: log, pol: pol, events: make(chan func(), 4096), peers: map[netip.Addr]*peer{},
		local: map[netip.Prefix]*Path{}, inactive: map[netip.Prefix]bool{}, best: map[netip.Prefix]*dest{},
		dirty: map[netip.Prefix]bool{}, started: time.Now(), RoutesDelay: 100 * time.Millisecond, clusterIDs: map[netip.Addr]bool{}}
}

func (s *Speaker) do(f func()) { s.events <- f }

// call runs f on the loop and waits for it.
func (s *Speaker) call(f func()) {
	done := make(chan struct{})
	s.do(func() { f(); close(done) })
	<-done
}

// Run serves events until ctx ends; every session ends with a Cease.
func (s *Speaker) Run(ctx context.Context) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			for _, p := range s.peers {
				p.stop(6, 2) // Cease, administrative shutdown
			}
			return
		case f := <-s.events:
			f()
			// Drain what is queued before deciding (batches of updates).
			for n := 0; n < 256; n++ {
				select {
				case f := <-s.events:
					f()
					continue
				default:
				}
				break
			}
		case now := <-tick.C:
			for _, a := range s.sortedPeers() {
				s.peers[a].tick(now)
			}
		}
		s.settle(time.Now())
	}
}

func (s *Speaker) sortedPeers() []netip.Addr {
	out := make([]netip.Addr, 0, len(s.peers))
	for a := range s.peers {
		out = append(out, a)
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return out
}

// Configure applies a configuration: only neighbours whose session
// settings changed are reset; the others keep their sessions.
func (s *Speaker) Configure(c Config) {
	s.call(func() { s.configure(c) })
}

func (s *Speaker) configure(c Config) {
	resetAll := c.AS != s.cfg.AS || c.RouterID != s.cfg.RouterID
	s.cfg = c
	s.clusterIDs = map[netip.Addr]bool{}
	want := map[netip.Addr]Neighbor{}
	for _, n := range c.Neighbors {
		want[n.Addr] = n
		if n.Cluster.IsValid() {
			s.clusterIDs[n.Cluster] = true
		}
	}
	for a, p := range s.peers {
		if _, ok := want[a]; !ok {
			p.stop(6, 3) // Cease, peer de-configured
			p.dropPaths()
			delete(s.peers, a)
		}
	}
	for _, a := range sortedAddrs(want) {
		n := want[a]
		p := s.peers[a]
		if p == nil {
			p = newPeer(s, n)
			s.peers[a] = p
			continue
		}
		old := p.n
		p.n = n
		if !n.BFD && (p.bfdHold || p.bfdWasUp) {
			p.bfdWasUp, p.bfdHold = false, false // BFD no longer configured
		}
		switch {
		case n.Disabled:
			p.stop(6, 2)
			p.dropPaths()
		case resetAll || old.resetKey() != n.resetKey() || old.Disabled:
			p.stop(6, 6) // Cease, other configuration change
			p.dropPaths()
			p.retryAt = time.Time{}
		default:
			// Policies, multipath, cluster id: re-evaluated in place.
			p.reimport()
			p.readvertise()
		}
	}
	s.markAll()
}

// SetPolicy replaces the policies: imports are evaluated again from the
// Adj-RIBs-In (no route refresh needed) and exports sent where they
// change.
func (s *Speaker) SetPolicy(pol Policy) {
	s.call(func() {
		s.pol = pol
		for _, a := range s.sortedPeers() {
			s.peers[a].reimport()
		}
		s.markAll()
		for _, a := range s.sortedPeers() {
			s.peers[a].readvertise()
		}
	})
}

// SetLocal replaces the originated paths (routes of other protocols that
// an export policy may announce) and the prefixes where BGP's own route
// is not active in the routing table (it is then not announced).
func (s *Speaker) SetLocal(paths []Path, inactive []netip.Prefix) {
	s.call(func() {
		want := map[netip.Prefix]*Path{}
		for i := range paths {
			p := paths[i]
			p.Prefix = p.Prefix.Masked()
			p.Peer = netip.Addr{}
			if p.Since.IsZero() {
				p.Since = time.Now()
			}
			want[p.Prefix] = &p
		}
		for pf, old := range s.local {
			if n, ok := want[pf]; !ok || !samePath(old, n) {
				s.dirty[pf] = true
			}
		}
		for pf, n := range want {
			if old, ok := s.local[pf]; !ok || !samePath(old, n) {
				s.dirty[pf] = true
			} else {
				want[pf] = old // keep its Since
			}
		}
		s.local = want
		in := map[netip.Prefix]bool{}
		for _, pf := range inactive {
			in[pf.Masked()] = true
		}
		for pf := range s.inactive {
			if !in[pf] {
				s.dirty[pf] = true
			}
		}
		for pf := range in {
			if !s.inactive[pf] {
				s.dirty[pf] = true
			}
		}
		s.inactive = in
	})
}

// SetBFD reports the state of the neighbour's BFD session: going down
// after it was up ends the BGP session at once (Cease, BFD down, RFC 9384)
// and no new one starts until BFD is up again. A BFD session that never
// came up changes nothing (the neighbour may not run BFD).
func (s *Speaker) SetBFD(nbr netip.Addr, up bool) {
	s.do(func() {
		p := s.peers[nbr]
		if p == nil {
			return
		}
		switch {
		case up:
			p.bfdWasUp, p.bfdHold = true, false
			p.retryAt = time.Time{}
		case p.bfdWasUp && !p.bfdHold:
			p.bfdHold = true
			p.lastError = "BFD session down"
			p.stop(6, 10) // Cease, BFD down
			p.dropPaths()
			p.lastError = "BFD session down"
		}
	})
}

// Accept takes an incoming connection; it is closed when no enabled
// neighbour has its address.
func (s *Speaker) Accept(c net.Conn) {
	s.do(func() {
		var ra netip.Addr
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
			ra = addrOf(a.IP)
		}
		p := s.peers[ra.WithZone("")]
		if p == nil || p.n.Disabled || p.bfdHold {
			c.Close()
			return
		}
		p.attach(c, true)
	})
}

// Converged reports whether every established neighbour has sent its
// End-of-RIB (or the routes count as complete for another reason): the
// routing table may then remove what BGP did not send.
func (s *Speaker) Converged() bool {
	var ok bool
	s.call(func() { ok = s.converged() })
	return ok
}

func (s *Speaker) converged() bool {
	if time.Since(s.started) > 3*time.Minute {
		return true
	}
	for _, p := range s.peers {
		if p.n.Disabled {
			continue
		}
		if p.established() && !p.allEOR() {
			return false
		}
		if !p.established() && time.Since(s.started) < 30*time.Second && !p.n.Passive {
			return false // give the sessions time to come up
		}
	}
	return true
}

func (s *Speaker) markAll() {
	for pf := range s.best {
		s.dirty[pf] = true
	}
	for pf := range s.local {
		s.dirty[pf] = true
	}
	for _, p := range s.peers {
		for pf := range p.in {
			s.dirty[pf] = true
		}
	}
}

// settle runs the decision process for changed prefixes and sends the
// resulting updates.
func (s *Speaker) settle(now time.Time) {
	if len(s.dirty) > 0 {
		changed := s.decide()
		for _, a := range s.sortedPeers() {
			s.peers[a].advertise(changed)
		}
		s.routesDirty = true
	}
	if c := s.converged(); c != s.lastConverged {
		s.lastConverged, s.routesDirty = c, true
	}
	if s.routesDirty && s.OnRoutes != nil && now.Sub(s.routesAt) >= s.RoutesDelay {
		s.routesDirty, s.routesAt = false, now
		s.OnRoutes(s.table(), s.lastConverged)
	}
}

// table is every usable path, ranked.
func (s *Speaker) table() []Route {
	var out []Route
	for _, pf := range sortedPrefixes(s.best) {
		d := s.best[pf]
		for i, p := range d.paths {
			r := Route{Path: *p}
			if i >= d.used {
				r.Rank = i - d.used + 1
			}
			out = append(out, r)
		}
	}
	return out
}

// decide recomputes the dirty prefixes; it returns them.
func (s *Speaker) decide() []netip.Prefix {
	changed := sortedPrefixes(s.dirty)
	clear(s.dirty)
	for _, pf := range changed {
		var cands []*Path
		for _, a := range s.sortedPeers() {
			p := s.peers[a]
			if ip := p.in[pf]; ip != nil && ip.accepted != nil && p.usable() && s.nextHopOK(ip.accepted) {
				cands = append(cands, ip.accepted)
			}
		}
		if len(cands) == 0 {
			delete(s.best, pf)
			continue
		}
		slices.SortStableFunc(cands, s.compare)
		used := 1
		if bp := s.peers[cands[0].Peer]; bp != nil && bp.n.Multipath {
			for used < len(cands) && used < 16 && s.multipathEqual(cands[0], cands[used], bp.n.MultipleAS) {
				used++
			}
		}
		s.best[pf] = &dest{paths: cands, used: used}
	}
	return changed
}

// nextHopOK: a path whose next hop is this switch is not usable.
func (s *Speaker) nextHopOK(p *Path) bool {
	return p.NextHop.IsValid() && p.NextHop != s.cfg.RouterID
}

// compare orders paths, best first (RFC 4271 §9.1.2.2 with the Junos
// order).
func (s *Speaker) compare(a, b *Path) int {
	if a.localPref() != b.localPref() {
		return cmpDesc(a.localPref(), b.localPref())
	}
	if la, lb := a.ASPathLen(), b.ASPathLen(); la != lb {
		return cmpAsc(la, lb)
	}
	if a.Origin != b.Origin {
		return cmpAsc(a.Origin, b.Origin)
	}
	if a.FirstAS() == b.FirstAS() && a.med() != b.med() {
		return cmpAsc(a.med(), b.med())
	}
	if a.EBGP != b.EBGP {
		if a.EBGP {
			return -1
		}
		return 1
	}
	ida, idb := a.PeerID, b.PeerID
	if a.OriginatorID.IsValid() {
		ida = a.OriginatorID
	}
	if b.OriginatorID.IsValid() {
		idb = b.OriginatorID
	}
	if c := ida.Compare(idb); c != 0 {
		return c
	}
	if len(a.ClusterList) != len(b.ClusterList) {
		return cmpAsc(len(a.ClusterList), len(b.ClusterList))
	}
	return a.Peer.Compare(b.Peer)
}

// multipathEqual: b may be used together with the best path a.
func (s *Speaker) multipathEqual(a, b *Path, multipleAS bool) bool {
	return a.localPref() == b.localPref() && a.ASPathLen() == b.ASPathLen() && a.Origin == b.Origin &&
		a.med() == b.med() && a.EBGP == b.EBGP && (multipleAS || a.FirstAS() == b.FirstAS())
}

// advertised is the path the speaker announces for a prefix (nil: none):
// an originated path, else BGP's best path while it is the active route.
func (s *Speaker) advertised(pf netip.Prefix) *Path {
	if l := s.local[pf]; l != nil {
		return l
	}
	if d := s.best[pf]; d != nil && !s.inactive[pf] {
		return d.paths[0]
	}
	return nil
}

func cmpAsc[T int | uint8 | uint32](a, b T) int {
	if a < b {
		return -1
	}
	return 1
}

func cmpDesc[T int | uint8 | uint32](a, b T) int { return -cmpAsc(a, b) }

func samePath(a, b *Path) bool {
	return a.Source == b.Source && attrKey(&a.Attrs) == attrKey(&b.Attrs)
}

func sortedAddrs[V any](m map[netip.Addr]V) []netip.Addr {
	out := make([]netip.Addr, 0, len(m))
	for a := range m {
		out = append(out, a)
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return out
}

func sortedPrefixes[V any](m map[netip.Prefix]V) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	return out
}
