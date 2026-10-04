package daemon

import (
	"github.com/thxrben/cerium-switchd/internal/memslots"
	"net/netip"
	"slices"

	"github.com/thxrben/cerium-switchd/internal/bgpd"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/pkg/bgp"
)

// bgpConfig is cer-bgpd's configuration (reference 5.14): every BGP
// instance with its neighbours' effective settings.
func bgpConfig(cfg *model.Config, capacity func(memslots.Purpose) int) bgpd.Config {
	c := bgpd.Config{Policies: cfg.Policies}
	if capacity != nil {
		c.Limits = bgpd.Limits{IPv4: capacity(memslots.BGPv4), IPv6: capacity(memslots.BGPv6), Paths: capacity(memslots.BGPPaths)}
	}
	for _, r := range cfg.AllRouting() {
		if r.BGP == nil || r.BGP.Disabled || r.AS == 0 {
			continue
		}
		in := bgpd.Instance{Name: r.Instance, VRF: r.Instance, AS: r.AS, RouterID: cfg.RouterID(r)}
		for _, g := range sortedKeysOf(r.BGP.Groups) {
			grp := r.BGP.Groups[g]
			addrs := make([]netip.Addr, 0, len(grp.Neighbors))
			for a := range grp.Neighbors {
				addrs = append(addrs, a)
			}
			slices.SortFunc(addrs, netip.Addr.Compare)
			for _, a := range addrs {
				in.Neighbors = append(in.Neighbors, bgpNeighbor(cfg, r.Instance, grp.Neighbors[a]))
			}
		}
		c.Instances = append(c.Instances, in)
	}
	return c
}

func bgpNeighbor(cfg *model.Config, instance string, m *model.BGPNeighbor) bgpd.Neighbor {
	n := bgp.Neighbor{Addr: m.Addr, Group: m.Group, PeerAS: m.PeerAS, LocalAS: m.LocalAS, Internal: m.Internal,
		HoldTime: m.HoldTime, Passive: m.Passive, Cluster: m.Cluster, RemovePrivate: m.RemovePrivate,
		GracefulRestart: m.GracefulRestart, RestartTime: m.RestartTime, StaleTime: m.StaleTime,
		Multipath: m.Multipath, MultipleAS: m.MultipleAS, Disabled: m.Disabled,
		LocalAddress: m.LocalAddress, AuthKey: m.AuthKey}
	if n.HoldTime < 0 {
		n.HoldTime = 90
	}
	if m.IPv4 {
		n.Families = append(n.Families, bgp.IPv4Unicast)
	}
	if m.IPv6 {
		n.Families = append(n.Families, bgp.IPv6Unicast)
	}
	switch {
	case m.Multihop && m.TTL > 0:
		n.TTL = m.TTL
	case m.Multihop:
		n.TTL = 64
	}
	var connected bool
	n.NextHop4, n.NextHop6, connected = selfAddrs(cfg, instance, m.Addr, m.LocalAddress)
	out := bgpd.Neighbor{Neighbor: n, Import: m.Import, Export: m.Export, Owner: neighborOwner(cfg, instance, m.Addr)}
	if b := m.BFD; b != nil {
		bc := &bgpd.BFDConfig{IntervalMs: b.IntervalMs, Multiplier: b.Multiplier, AuthType: b.AuthAlg, AuthKeyID: b.AuthKeyID,
			AuthKey: b.AuthKey, Multihop: m.Multihop || !connected}
		if bc.Multihop {
			bc.Local = m.LocalAddress
			if !bc.Local.IsValid() {
				bc.Local = n.NextHop4
				if m.Addr.Is6() {
					bc.Local = n.NextHop6
				}
			}
		}
		out.BFDCfg = bc
	}
	return out
}

// selfAddrs are this switch's IPv4 and IPv6 addresses towards a
// neighbour, for next hop self in the other family than the session's:
// those of the unit whose subnet has the neighbour (or that has the local
// address), stack-wide addresses only. connected: a unit's subnet has the
// neighbour.
func selfAddrs(cfg *model.Config, instance string, peer, local netip.Addr) (v4, v6 netip.Addr, connected bool) {
	for _, name := range sortedKeysOf(cfg.L3) {
		u := cfg.L3[name]
		if u.Disabled || u.Instance != instance {
			continue
		}
		on := false
		for _, p := range u.Addrs {
			if p.Contains(peer) {
				on, connected = true, true
			}
			if p.Addr() == local {
				on = true
			}
		}
		if !on {
			continue
		}
		for _, p := range u.Addrs {
			if _, perMember := u.AddrMember[p]; perMember || p.Addr().IsLinkLocalUnicast() {
				continue
			}
			if p.Addr().Is4() && !v4.IsValid() {
				v4 = p.Addr()
			}
			if p.Addr().Is6() && !v6.IsValid() {
				v6 = p.Addr()
			}
		}
		return v4, v6, connected
	}
	return v4, v6, connected
}

// neighborOwner is the member whose routed port (or single-member bundle)
// has the neighbour's subnet; 0 for an irb, an MC-LAG bundle or a
// neighbour that is not directly connected (the master reaches it).
func neighborOwner(cfg *model.Config, instance string, peer netip.Addr) int {
	for _, name := range sortedKeysOf(cfg.L3) {
		u := cfg.L3[name]
		if u.Disabled || u.Instance != instance || u.IRB() {
			continue
		}
		for _, p := range u.Addrs {
			if p.Contains(peer) {
				if owners, _ := unitOwners(cfg, u); len(owners) == 1 {
					return owners[0]
				}
				return 0
			}
		}
	}
	return 0
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
