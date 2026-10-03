package dataplane

import (
	"maps"
	"slices"

	"github.com/thxrben/cerium-switchd/internal/model"
)

// Multicast is the desired IGMP/MLD snooping of a member's bridge
// (reference 5.5). The Linux bridge snoops IGMP and MLD together, per VLAN.
type Multicast struct {
	// On: snooping runs in at least one VLAN (the bridge's switch).
	On    bool
	VLANs map[uint16]McastVLAN
	// Router: bridge ports that always get all group traffic (stack
	// tunnels, VXLAN ports, multicast-router-interface).
	Router map[string]bool
	// FastLeave: ports with immediate-leave.
	FastLeave map[string]bool
}

// McastVLAN is the snooping of one VLAN.
type McastVLAN struct {
	Snooping, Querier bool
	IGMPVersion       int // 2|3
	MLDVersion        int // 1|2
}

// Querier reports whether this member sends queries in any VLAN.
func (m *Multicast) Querier() bool {
	for _, v := range m.VLANs {
		if v.Snooping && v.Querier {
			return true
		}
	}
	return false
}

// ComputeMulticast returns the snooping of member m for its desired state s
// (the bridge's VLANs and ports are those of s).
func ComputeMulticast(cfg *model.Config, s *State, names PortNames) *Multicast {
	mc := &Multicast{VLANs: map[uint16]McastVLAN{}, Router: map[string]bool{}, FastLeave: map[string]bool{}}
	if s.Bridge == nil || cfg.IGMP == nil || cfg.MLD == nil {
		return mc
	}
	vids := map[uint16]bool{}
	for _, l := range s.Links {
		if l.Master != BridgeName {
			continue
		}
		for vid := range l.VLANs {
			vids[vid] = true
		}
		if l.Kind == Tunnel || VXLANVNI(l.Name) > 0 {
			mc.Router[l.Name] = true
		}
	}
	for _, v := range s.SelfVLANs {
		vids[uint16(v)] = true
	}
	for _, vid := range slices.Sorted(maps.Keys(vids)) {
		ig, on := cfg.IGMP.VLAN(int(vid))
		ml, _ := cfg.MLD.VLAN(int(vid))
		mc.VLANs[vid] = McastVLAN{Snooping: on, Querier: ig.Querier || ml.Querier, IGMPVersion: ig.Version, MLDVersion: ml.Version}
		mc.On = mc.On || on
	}
	kernel := func(n string) (string, bool) {
		if cfg.Interfaces[n] != nil && cfg.Interfaces[n].AE {
			return n, true
		}
		return names(n)
	}
	for _, sn := range []*model.Snooping{cfg.IGMP, cfg.MLD} {
		for n, p := range sn.Ports {
			k, ok := kernel(n)
			if !ok || s.Links[k] == nil || s.Links[k].Master != BridgeName {
				continue // not a switch port of this member
			}
			if p.Router {
				mc.Router[k] = true
			}
			if p.ImmediateLeave {
				mc.FastLeave[k] = true
			}
		}
	}
	return mc
}
