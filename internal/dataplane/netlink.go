//go:build linux

package dataplane

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

// Netlink is the real Kernel.
type Netlink struct {
	// SysRoot is normally "/sys" (used to tell physical ports apart).
	SysRoot string
	// StateDir keeps what switchd changed outside its own devices (L3).
	StateDir string

	mlOnce sync.Once
	macl   *macLimits

	protMu    sync.Mutex
	protected string // nftables rules last installed by syncProtect
	protInit  bool
}

func (k *Netlink) sysRoot() string {
	if k.SysRoot == "" {
		return "/sys"
	}
	return k.SysRoot
}

func (k *Netlink) physical(name string) bool {
	_, err := os.Stat(filepath.Join(k.sysRoot(), "class", "net", name, "device"))
	return err == nil
}

// Read returns the bridge, all links and the VLANs of bridge ports.
func (k *Netlink) Read() (*State, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	byIndex := map[int]string{}
	for _, l := range links {
		// A VRF as master (routing instances) is managed by SyncL3, not by
		// the planner: it is reported as no master.
		if l.Type() != "vrf" {
			byIndex[l.Attrs().Index] = l.Attrs().Name
		}
	}
	s := &State{Links: map[string]*Link{}}
	maxes := maxMTUs()
	for _, l := range links {
		a := l.Attrs()
		if a.Name == "lo" {
			continue
		}
		ln := &Link{
			Name:    a.Name,
			Up:      a.Flags&net.FlagUp != 0,
			MTU:     a.MTU,
			MaxMTU:  maxes[a.Name],
			Alias:   a.Alias,
			Master:  byIndex[a.MasterIndex],
			Present: true,
		}
		switch v := l.(type) {
		case *netlink.Bridge:
			ln.Kind = BridgeKind
			if a.Name == BridgeName {
				s.Bridge = &BridgeOpts{AgeingSeconds: 300}
				if v.AgeingTime != nil {
					s.Bridge.AgeingSeconds = int(*v.AgeingTime) / 100
				}
			}
		case *netlink.Vxlan:
			ln.Kind = Other
			if TunnelMember(a.Name) > 0 {
				ln.Kind = Tunnel
				ln.Tunnel = &TunnelOpts{VNI: v.VxlanId}
				ln.Tunnel.Local, _ = netip.AddrFromSlice(v.SrcAddr.To4())
				ln.Tunnel.Remote, _ = netip.AddrFromSlice(v.Group.To4())
			}
		case *netlink.Bond:
			ln.Kind = Bond
			ln.Bond = &BondOpts{
				Mode:       v.Mode.String(),
				HashPolicy: v.XmitHashPolicy.String(),
				MinLinks:   v.MinLinks,
				MIIMon:     v.Miimon,
			}
		default:
			if l.Type() == "team" {
				ln.Kind = Bond
				ln.Bond = &BondOpts{Mode: "lacp", HashPolicy: teamHashPolicy(a.Index)}
				break
			}
			if l.Type() == "device" && k.physical(a.Name) {
				ln.Kind = Physical
			} else {
				ln.Kind = Other
			}
		}
		if ln.Kind == Physical || ln.Kind == Bond {
			ln.MaxLearned = k.maxLearned(a.Name)
			if ln.Kind == Physical {
				ln.FlowControl = readFlowControl(a.Name)
			}
			tc := readTC(l)
			ln.DropTagged = tc.dropTagged()
			ln.StormBroadcast, ln.StormMulticast = tc.stormBroadcast, tc.stormMulticast
		}
		s.Links[a.Name] = ln
	}
	vlans, err := netlink.BridgeVlanList()
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if ln := s.Links[l.Attrs().Name]; ln != nil && ln.Master == BridgeName {
			if pi, err := netlink.LinkGetProtinfo(l); err == nil {
				ln.NoLearning, ln.Isolated = !pi.Learning, pi.Isolated
			}
		}
	}
	for idx, infos := range vlans {
		l := s.Links[byIndex[int(idx)]]
		if l == nil || l.Master != BridgeName {
			continue
		}
		l.VLANs = map[uint16]VlanFlags{}
		for _, vi := range infos {
			l.VLANs[vi.Vid] = VlanFlags{
				PVID:     vi.Flags&nl.BRIDGE_VLAN_INFO_PVID != 0,
				Untagged: vi.Flags&nl.BRIDGE_VLAN_INFO_UNTAGGED != 0,
			}
		}
	}
	return s, nil
}

func (k *Netlink) byName(n string) (netlink.Link, error) {
	l, err := netlink.LinkByName(n)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", n, err)
	}
	return l, nil
}

func bondAttrs(b *netlink.Bond, o *BondOpts) {
	b.Mode = netlink.StringToBondMode(o.Mode)
	b.XmitHashPolicy = netlink.StringToBondXmitHashPolicyMap[o.HashPolicy]
	b.MinLinks = o.MinLinks
	b.Miimon = o.MIIMon
}

// Apply performs one operation.
func (k *Netlink) Apply(op Op) error {
	switch op.Kind {
	case OpCreateBridge, OpSetBridge:
		on, off := true, false
		pvid := uint16(0)
		ageing := uint32(op.Bridge.AgeingSeconds * 100)
		br := &netlink.Bridge{
			LinkAttrs:         netlink.LinkAttrs{Name: BridgeName},
			VlanFiltering:     &on,
			VlanDefaultPVID:   &pvid,
			MulticastSnooping: &off, // flood multicast: never drop for lack of a querier
			AgeingTime:        &ageing,
		}
		if op.Kind == OpCreateBridge {
			return netlink.LinkAdd(br)
		}
		return netlink.LinkModify(br)
	case OpCreateTunnel:
		return createTunnel(op.Link, *op.Tunnel, op.MTU)
	case OpCreateBond:
		if op.Bond.Team() {
			return createTeam(op.Link, op.Bond.HashPolicy)
		}
		b := netlink.NewLinkBond(netlink.LinkAttrs{Name: op.Link})
		bondAttrs(b, op.Bond)
		return netlink.LinkAdd(b)
	}
	l, err := k.byName(op.Link)
	if err != nil {
		return err
	}
	switch op.Kind {
	case OpSetBond:
		if l.Type() == "team" {
			return setTeamHash(l.Attrs().Index, op.Bond.HashPolicy)
		}
		b := netlink.NewLinkBond(netlink.LinkAttrs{Name: op.Link, Index: l.Attrs().Index})
		bondAttrs(b, op.Bond)
		return netlink.LinkModify(b)
	case OpDeleteLink:
		return netlink.LinkDel(l)
	case OpSetUp:
		return netlink.LinkSetUp(l)
	case OpSetDown:
		return netlink.LinkSetDown(l)
	case OpSetMaster:
		if op.Master == "" {
			return netlink.LinkSetNoMaster(l)
		}
		m, err := k.byName(op.Master)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetMaster(l, m); err != nil {
			return err
		}
		if m.Type() == "team" {
			// Carries nothing until LACP says so (the port is still down).
			return teamPortInit(m.Attrs().Index, l.Attrs().Index)
		}
		return nil
	case OpSetMTU:
		return netlink.LinkSetMTU(l, op.MTU)
	case OpSetAlias:
		return netlink.LinkSetAlias(l, op.Alias)
	case OpVlanDel:
		return netlink.BridgeVlanDel(l, op.VID, false, false, false, true)
	case OpVlanSet:
		return netlink.BridgeVlanAdd(l, op.VID, op.Flags.PVID, op.Flags.Untagged, false, true)
	case OpSetDropTagged:
		return setDropTagged(l, op.Bool)
	case OpSetStormBroadcast:
		return setStorm(l, prioStormBroadcast, op.Int)
	case OpSetStormMulticast:
		return setStorm(l, prioStormMulticast, op.Int)
	case OpSetMaxLearned:
		k.setMaxLearned(op.Link, op.Int)
		return nil
	case OpSetLearning:
		return netlink.LinkSetLearning(l, op.Bool)
	case OpSetIsolated:
		return netlink.LinkSetIsolated(l, op.Bool)
	case OpSetFlowControl:
		return setFlowControl(op.Link, op.Bool)
	}
	return fmt.Errorf("unknown op %d", op.Kind)
}
