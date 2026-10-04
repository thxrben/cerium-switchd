package model

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// ChangeWarnings are warnings about what a commit of cand would do to the
// running state of active (reference 5.14: "this change resets the
// session"). active may be nil (first commit).
func ChangeWarnings(active, cand *Config) Issues {
	if active == nil || cand == nil {
		return nil
	}
	var out Issues
	old := map[string]*Routing{}
	for _, r := range active.AllRouting() {
		old[r.Instance] = r
	}
	for _, r := range cand.AllRouting() {
		o := old[r.Instance]
		if o == nil || o.BGP == nil || r.BGP == nil || o.BGP.Disabled || r.BGP.Disabled {
			continue
		}
		base := "protocols bgp"
		if r.Instance != "" {
			base = "routing-instances " + r.Instance + " protocols bgp"
		}
		if o.AS != r.AS || active.RouterID(o) != cand.RouterID(r) {
			out = append(out, Issue{Severity: Warning, Path: base,
				Msg: "the autonomous system or router id changes: this change resets every BGP session of the instance"})
			continue
		}
		before := bgpNeighbors(o.BGP)
		for _, g := range sortedKeys(r.BGP.Groups) {
			for _, a := range sortedAddrs(r.BGP.Groups[g].Neighbors) {
				n := r.BGP.Groups[g].Neighbors[a]
				p := before[a]
				if p == nil || p.Disabled || n.Disabled {
					continue
				}
				if what := sessionChange(p, n); what != "" {
					out = append(out, Issue{Severity: Warning, Path: fmt.Sprintf("%s group %s neighbor %s", base, g, a),
						Msg: fmt.Sprintf("bgp neighbor %s: this change resets the session (%s)", a, what)})
				}
			}
		}
	}
	return out
}

func bgpNeighbors(b *BGP) map[netip.Addr]*BGPNeighbor {
	out := map[netip.Addr]*BGPNeighbor{}
	for _, g := range b.Groups {
		for a, n := range g.Neighbors {
			out[a] = n
		}
	}
	return out
}

// sessionChange names the settings whose change needs a new session (the
// ones a BGP OPEN or the TCP connection carries).
func sessionChange(a, b *BGPNeighbor) string {
	var what []string
	add := func(changed bool, name string) {
		if changed {
			what = append(what, name)
		}
	}
	add(a.PeerAS != b.PeerAS, "peer-as")
	add(a.LocalAddress != b.LocalAddress, "local-address")
	add(a.LocalAS != b.LocalAS, "local-as")
	add(a.AuthKey != b.AuthKey, "authentication-key")
	add(a.Internal != b.Internal, "type")
	add(a.IPv4 != b.IPv4 || a.IPv6 != b.IPv6, "family")
	add(a.Multihop != b.Multihop || a.TTL != b.TTL, "multihop")
	add(a.HoldTime != b.HoldTime, "hold-time")
	add(a.Passive != b.Passive, "passive")
	add(a.GracefulRestart != b.GracefulRestart || a.RestartTime != b.RestartTime, "graceful-restart")
	if len(what) == 0 {
		return ""
	}
	slices.Sort(what)
	return strings.Join(what, ", ")
}
