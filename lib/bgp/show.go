package bgp

import (
	"net/netip"
	"time"
)

// FamilyCount are a neighbour's prefixes of one family.
type FamilyCount struct {
	Family     Family `json:"family"`
	Received   int    `json:"received"`
	Accepted   int    `json:"accepted"`
	Active     int    `json:"active"`
	Advertised int    `json:"advertised"`
}

// NeighborStatus is a neighbour for show bgp summary|neighbor.
type NeighborStatus struct {
	Addr      netip.Addr    `json:"addr"`
	Group     string        `json:"group"`
	PeerAS    uint32        `json:"peer_as"`
	LocalAS   uint32        `json:"local_as"`
	Internal  bool          `json:"internal,omitempty"`
	State     string        `json:"state"`
	Since     time.Duration `json:"since"` // in the state
	RouterID  netip.Addr    `json:"router_id"`
	HoldTime  int           `json:"hold_time"`  // negotiated (s)
	LocalHold int           `json:"local_hold"` // configured (s)
	Local     netip.Addr    `json:"local,omitempty"`
	LastError string        `json:"last_error,omitempty"`
	Families  []Family      `json:"families,omitempty"` // negotiated
	// The neighbour's capabilities.
	AS4, RouteRefresh, GR bool
	GRTime                int           `json:"gr_time,omitempty"`
	Stale                 bool          `json:"stale,omitempty"` // its routes are kept while it restarts
	Counts                []FamilyCount `json:"counts,omitempty"`
	Stats                 Stats         `json:"stats"`
	Disabled              bool          `json:"disabled,omitempty"`
	Passive               bool          `json:"passive,omitempty"`
	Client                bool          `json:"client,omitempty"` // route reflector client
	// The policy chains (filled by the program).
	Import []string `json:"import,omitempty"`
	Export []string `json:"export,omitempty"`
}

// Status lists the neighbours.
func (s *Speaker) Status() []NeighborStatus {
	var out []NeighborStatus
	s.call(func() {
		now := time.Now()
		for _, a := range s.sortedPeers() {
			p := s.peers[a]
			st := NeighborStatus{Addr: a, Group: p.n.Group, PeerAS: p.n.PeerAS, LocalAS: p.n.LocalAS, Internal: p.n.Internal,
				State: p.state().String(), Since: now.Sub(p.since), LocalHold: p.n.HoldTime, LastError: p.lastError,
				Stats: p.stats, Disabled: p.n.Disabled, Passive: p.n.Passive, Client: p.n.Cluster.IsValid(), Stale: p.stale}
			if p.established() {
				st.RouterID, st.HoldTime, st.Families = p.open.RouterID, int(p.hold/time.Second), p.families
				st.AS4, st.RouteRefresh, st.GR, st.GRTime = p.open.AS4, p.open.RouteRefresh, p.open.GR, int(p.open.GRTime)
				st.Local = p.est.local()
			}
			for _, f := range p.n.Families {
				c := FamilyCount{Family: f}
				for pf, ip := range p.in {
					if FamilyOf(pf) != f {
						continue
					}
					c.Received++
					if ip.accepted != nil {
						c.Accepted++
						if d := s.best[pf]; d != nil {
							for _, u := range d.paths[:d.used] {
								if u == ip.accepted {
									c.Active++
								}
							}
						}
					}
				}
				for pf := range p.out {
					if FamilyOf(pf) == f {
						c.Advertised++
					}
				}
				st.Counts = append(st.Counts, c)
			}
			out = append(out, st)
		}
	})
	return out
}

// InPath is a received path; Hidden: rejected by the import policy or a
// loop check.
type InPath struct {
	Path
	Hidden bool
}

// AdjIn is what a neighbour sent (show route receive-protocol).
func (s *Speaker) AdjIn(nbr netip.Addr) []InPath {
	var out []InPath
	s.call(func() {
		p := s.peers[nbr]
		if p == nil {
			return
		}
		for _, pf := range sortedPrefixes(p.in) {
			ip := p.in[pf]
			if ip.accepted != nil {
				out = append(out, InPath{Path: *ip.accepted})
			} else {
				out = append(out, InPath{Path: *ip.raw, Hidden: true})
			}
		}
	})
	return out
}

// AdjOut is what was announced to a neighbour (show route
// advertising-protocol).
func (s *Speaker) AdjOut(nbr netip.Addr) []Path {
	var out []Path
	s.call(func() {
		p := s.peers[nbr]
		if p == nil {
			return
		}
		for _, pf := range sortedPrefixes(p.out) {
			out = append(out, *p.out[pf].path)
		}
	})
	return out
}

// Clear modes.
const (
	ClearHard        = ""             // reset the session
	ClearSoft        = "soft"         // send everything again, ask the neighbour for its routes again
	ClearSoftInbound = "soft-inbound" // evaluate the import policy again
)

// Clear resets (or refreshes) the neighbour nbr, every neighbour when nbr
// is invalid; it returns how many it touched.
func (s *Speaker) Clear(nbr netip.Addr, mode string) int {
	n := 0
	s.call(func() {
		for _, a := range s.sortedPeers() {
			p := s.peers[a]
			if nbr.IsValid() && a != nbr {
				continue
			}
			n++
			switch mode {
			case ClearSoft:
				if p.established() {
					for _, f := range p.families {
						p.resend(f)
						if p.open.RouteRefresh {
							p.est.send(routeRefreshMsg(f))
						}
					}
				}
			case ClearSoftInbound:
				p.reimport()
			default:
				p.stop(6, 4) // Cease, administrative reset
				p.dropPaths()
				p.retryAt = time.Time{}
			}
		}
	})
	return n
}
