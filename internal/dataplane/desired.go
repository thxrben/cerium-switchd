package dataplane

import (
	"fmt"
	"slices"

	"mclag/internal/model"
)

// Compute returns the desired state of member m for cfg, plus notes about
// configuration that is accepted but not (yet) effective on this member.
func Compute(cfg *model.Config, m int) (*State, []string) {
	s := &State{
		Bridge: &BridgeOpts{AgeingSeconds: cfg.Switch.MACAging},
		Links:  map[string]*Link{},
	}
	if s.Bridge.AgeingSeconds == 0 {
		s.Bridge.AgeingSeconds = 300
	}
	var notes []string

	// Bundles present on this member: those with a member port here.
	for _, i := range sortedIfs(cfg) {
		if !i.AE || !slices.Contains(i.MemberIDs, m) {
			continue
		}
		l := &Link{
			Name:  i.Name,
			Kind:  Bond,
			Up:    !i.Disabled,
			MTU:   model.LinuxMTU(i.MTU),
			Alias: i.Description,
			Bond: &BondOpts{
				Mode:       "balance-xor",
				HashPolicy: orDefault(i.HashPolicy, "layer3+4"),
				MinLinks:   i.MinLinks,
				MIIMon:     100,
			},
			MaxLearned: i.MACLimit,
		}
		if i.MCLAG {
			// minimum-links counts ports on both members; enforced by the
			// MC-LAG logic, not by the local bond.
			l.Bond.MinLinks = 0
		}
		if i.LACP != nil {
			// LACP runs in userspace (Phase 6). Until then a LACP bundle stays
			// down rather than forwarding without negotiation (loop risk).
			l.Up = false
			notes = append(notes, fmt.Sprintf("%s: LACP is not implemented yet; the bundle is kept down", i.Name))
		}
		if i.Switching {
			l.Master = BridgeName
			l.VLANs, l.DropTagged = portVLANs(i)
		}
		s.Links[l.Name] = l
	}

	for _, i := range sortedIfs(cfg) {
		if i.AE || i.Member != m {
			continue
		}
		l := &Link{
			Name:        i.Linux,
			Kind:        Physical,
			Up:          !i.Disabled,
			MTU:         model.LinuxMTU(i.MTU),
			Alias:       i.Description,
			FlowControl: i.FlowControl,
		}
		switch {
		case i.Parent != "":
			if b := s.Links[i.Parent]; b != nil {
				l.Master = b.Name
				l.MTU = b.MTU // members always use the bundle's MTU
			} else {
				l.Up = false
				notes = append(notes, fmt.Sprintf("%s: bundle %s is not configured; the port is kept down", i.Name, i.Parent))
			}
		case i.Switching:
			l.Master = BridgeName
			l.VLANs, l.DropTagged = portVLANs(i)
			l.MaxLearned = i.MACLimit
		}
		s.Links[l.Name] = l
	}
	return s, notes
}

// portVLANs returns the bridge VLAN entries of a switch port and whether
// tagged frames must be dropped on ingress (access ports, reference 5.3.2).
func portVLANs(i *model.Interface) (map[uint16]VlanFlags, bool) {
	v := map[uint16]VlanFlags{}
	switch i.Mode {
	case "trunk":
		for _, id := range i.VLANs {
			v[uint16(id)] = VlanFlags{}
		}
		if i.NativeVLAN != 0 {
			v[uint16(i.NativeVLAN)] = VlanFlags{PVID: true, Untagged: true}
		}
		return v, false
	default:
		if i.AccessVLAN != 0 {
			v[uint16(i.AccessVLAN)] = VlanFlags{PVID: true, Untagged: true}
		}
		return v, true
	}
}

func sortedIfs(cfg *model.Config) []*model.Interface {
	out := make([]*model.Interface, 0, len(cfg.Interfaces))
	for _, i := range cfg.Interfaces {
		out = append(out, i)
	}
	slices.SortFunc(out, func(a, b *model.Interface) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
