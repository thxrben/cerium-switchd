package dataplane

import (
	"fmt"
	"maps"
	"mclag/internal/schema"
	"slices"

	"mclag/internal/model"
)

// PortNames maps an interface name ("1/0/3") of this member to its Linux
// name; ok is false while the port does not exist.
type PortNames func(name string) (linux string, ok bool)

// Compute returns the desired state of member m for cfg, plus notes about
// configuration that is accepted but not (yet) effective on this member.
// Ports that do not exist are left out; they are configured when they
// appear (the caller recomputes on link events).
func Compute(cfg *model.Config, m int, names PortNames) (*State, []string) {
	if mem := cfg.Members[m]; mem != nil && mem.Witness {
		// A witness has no data plane (reference 5.2, role witness).
		return &State{Links: map[string]*Link{}, L3: &L3{}}, nil
	}
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
			MaxLearned:     i.MACLimit,
			StormBroadcast: i.StormControl.Broadcast,
			StormMulticast: i.StormControl.Multicast,
		}
		if i.MCLAG {
			// minimum-links counts ports on both members; enforced by the
			// MC-LAG logic, not by the local bond.
			l.Bond.MinLinks = 0
		}
		if i.LACP != nil {
			// A team device; its ports carry traffic once LACP has them
			// collecting and distributing (minimum-links is enforced there).
			l.Bond = &BondOpts{Mode: "lacp", HashPolicy: l.Bond.HashPolicy}
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
		linux, ok := names(i.Name)
		if !ok {
			continue
		}
		l := &Link{
			Name:        linux,
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
		if i.Parent == "" {
			l.StormBroadcast, l.StormMulticast = i.StormControl.Broadcast, i.StormControl.Multicast
		}
		s.Links[l.Name] = l
	}
	computeTunnels(cfg, m, s)
	s.L3 = computeL3(cfg, m, names, s)
	self := map[int]bool{}
	for _, i := range s.L3.Ifs {
		if i.Parent == BridgeName {
			self[i.VID] = true
		}
	}
	s.SelfVLANs = slices.Sorted(maps.Keys(self))
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

// SubifName is the kernel name of a routed subinterface: Linux names
// cannot contain "/", so 1/0/6 unit 100 becomes "sw-0-6.100".
func SubifName(parent string, unit int) string {
	if p, ok := schema.ParsePhysical(parent); ok {
		return fmt.Sprintf("sw-%d-%d.%d", p.Card, p.Port, unit)
	}
	return fmt.Sprintf("%s.%d", parent, unit)
}

// computeL3 returns the routed interfaces of member m.
func computeL3(cfg *model.Config, m int, names PortNames, s *State) *L3 {
	l := &L3{}
	units := slices.Sorted(maps.Keys(cfg.L3))
	for _, n := range units {
		u := cfg.L3[n]
		if u.CME() {
			continue // see Management
		}
		if !u.OnMember(m) {
			continue // an irb whose addresses all belong to other members
		}
		i := L3If{Up: !u.Disabled, Addrs: u.AddrsOn(m), VRF: u.Instance, DHCP: u.DHCP, Unit: n}
		switch {
		case u.IRB():
			if u.VLAN == 0 {
				continue
			}
			i.Name, i.Parent, i.VID, i.Own = n, BridgeName, u.VLAN, true
			i.Anycast = len(u.AddrMember) < len(u.Addrs)
			i.MTU = 1500
			if v := cfg.VLANByID[u.VLAN]; v != nil && v.MTU != 0 {
				i.MTU = model.LinuxMTU(v.MTU)
			}
		default:
			parent := u.Parent
			if schema.IsAE(parent) {
				if s.Links[parent] == nil {
					continue // the bundle has no port on this member
				}
			} else {
				if u.Member != m {
					continue
				}
				linux, ok := names(parent)
				if !ok {
					continue
				}
				parent = linux
			}
			if u.Tag == 0 {
				i.Name = parent
			} else {
				i.Name, i.Parent, i.VID, i.Own = SubifName(u.Parent, u.Unit), parent, u.Tag, true
			}
		}
		l.Ifs = append(l.Ifs, i)
	}
	for _, r := range cfg.Routes {
		l.Routes = append(l.Routes, Route{Prefix: r.Prefix, NextHops: slices.Clone(r.NextHops), Discard: r.Discard})
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Instances)) {
		in := cfg.Instances[name]
		l.VRFs = append(l.VRFs, VRF{Name: name, Mgmt: name == cfg.System.MgmtInstance})
		for _, r := range in.Routes {
			l.Routes = append(l.Routes, Route{VRF: name, Prefix: r.Prefix, NextHops: slices.Clone(r.NextHops), Discard: r.Discard})
		}
	}
	return l
}

// computeTunnels adds the stack tunnels of member m (reference 5.2): one to
// every other switch member, carrying tagged the VLANs both ends have,
// isolated from each other; the tunnel to the MC-LAG peer does not learn.
func computeTunnels(cfg *model.Config, m int, s *State) {
	members := cfg.SwitchMembers()
	if len(members) < 2 || !slices.Contains(members, m) {
		return
	}
	mtu, _ := cfg.MaxDataMTU()
	mine := memberVLANs(cfg, m)
	peer := mclagPeer(cfg, m)
	for _, x := range members {
		if x == m {
			continue
		}
		l := &Link{
			Name:       TunnelName(x),
			Kind:       Tunnel,
			Up:         true,
			MTU:        model.LinuxMTU(mtu),
			Master:     BridgeName,
			VLANs:      map[uint16]VlanFlags{},
			Isolated:   true,
			NoLearning: x == peer,
			Tunnel:     &TunnelOpts{VNI: TunnelVNI(m, x), Local: StackAddr(m), Remote: StackAddr(x)},
		}
		for vid := range memberVLANs(cfg, x) {
			if mine[vid] {
				l.VLANs[uint16(vid)] = VlanFlags{}
			}
		}
		s.Links[l.Name] = l
	}
}

// memberVLANs returns the VLANs that exist on member x: those of its switch
// ports and bundles, and of its irb addresses.
func memberVLANs(cfg *model.Config, x int) map[int]bool {
	out := map[int]bool{}
	for _, i := range cfg.Interfaces {
		if !i.Switching || !(i.Member == x && !i.AE || i.AE && slices.Contains(i.MemberIDs, x)) {
			continue
		}
		for _, v := range i.VLANs {
			out[v] = true
		}
		for _, v := range []int{i.NativeVLAN, i.AccessVLAN} {
			if v != 0 {
				out[v] = true
			}
		}
	}
	for _, u := range cfg.L3 {
		if u.IRB() && u.VLAN != 0 && len(u.AddrsOn(x)) > 0 {
			out[u.VLAN] = true
		}
	}
	return out
}

// mclagPeer returns the MC-LAG peer of member m (0: none).
func mclagPeer(cfg *model.Config, m int) int {
	if p := cfg.PairOf(m); p != nil {
		return p.Peer(m)
	}
	return 0
}
