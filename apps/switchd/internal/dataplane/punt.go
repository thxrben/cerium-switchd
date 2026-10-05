package dataplane

import (
	"maps"
	"net"
	"slices"

	"github.com/thxrben/cerium-switchd/lib/conf/model"
)

// Routing protocols over irb interfaces in a virtual chassis (reference
// 5.8): the protocols run on the master, but an irb exists on every member
// with the same address and MAC, so a neighbour's unicast protocol packets
// reach whichever member its frame arrives on (an MC-LAG leg, a port of
// another member). Every member that is not the master passes these frames
// to the master as they are: they leave the port's ingress straight into
// the stack tunnel to the master, in their VLAN, and the master's irb
// receives them as if they had arrived there. Source and destination MAC,
// the IP packet and the VLAN stay the client's; an untagged frame gets the
// tag of its port's VLAN, which the tunnel needs (the master's bridge
// removes it again). Only VLANs the port carries and that have an irb are
// passed, so a client cannot reach another VLAN's irb with a foreign tag.
//
// The protocols: OSPF (IP protocol 89), BFD (UDP 3784 single-hop, 4784
// multihop) and BGP (TCP to or from port 179), IPv4 and IPv6. Everything
// else to the gateway MAC (routed traffic, ARP, ping) stays local.

// Punt is the redirection of one member (nil or no ports: none).
type Punt struct {
	// Tunnel is the stack tunnel to the master.
	Tunnel     string
	GatewayMAC net.HardwareAddr
	Ports      []PuntPort
}

// PuntPort is a bridge port whose frames may carry protocol packets for
// an irb.
type PuntPort struct {
	Name string
	// PVID: the VLAN of untagged frames when it has an irb (0: untagged
	// frames are not passed).
	PVID int
	// VLANs: the tagged VLANs of the port that have an irb.
	VLANs []int
}

// ComputePunt returns the redirection for member m when master is the
// master (nothing on the master, or without a master or tunnels).
func ComputePunt(cfg *model.Config, s *State, m, master int, gw net.HardwareAddr) *Punt {
	if master == 0 || master == m || len(gw) != 6 {
		return nil
	}
	tun := TunnelName(master)
	tl := s.Links[tun]
	if tl == nil {
		return nil
	}
	irb := map[int]bool{}
	for _, v := range cfg.VLANs {
		if v.L3 == "" {
			continue
		}
		if u := cfg.L3[v.L3]; u != nil && !u.Disabled {
			irb[v.ID] = true
		}
	}
	p := &Punt{Tunnel: tun, GatewayMAC: gw}
	for _, name := range slices.Sorted(maps.Keys(s.Links)) {
		l := s.Links[name]
		if l.Master != BridgeName || (l.Kind != Physical && l.Kind != Bond) {
			continue
		}
		pp := PuntPort{Name: name}
		for vid, f := range l.VLANs {
			// The tunnel to the master must carry the VLAN.
			if !irb[int(vid)] || !hasVLAN(tl, vid) {
				continue
			}
			if f.PVID && f.Untagged {
				pp.PVID = int(vid)
			}
			if !f.Untagged {
				pp.VLANs = append(pp.VLANs, int(vid))
			}
		}
		slices.Sort(pp.VLANs)
		if pp.PVID != 0 || len(pp.VLANs) > 0 {
			p.Ports = append(p.Ports, pp)
		}
	}
	return p
}

func hasVLAN(l *Link, vid uint16) bool {
	_, ok := l.VLANs[vid]
	return ok
}
