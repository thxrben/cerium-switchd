//go:build linux

package dataplane

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

// Netlink is the real Kernel.
type Netlink struct {
	// SysRoot is normally "/sys" (used to tell physical ports apart).
	SysRoot string
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
		byIndex[l.Attrs().Index] = l.Attrs().Name
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
		case *netlink.Bond:
			ln.Kind = Bond
			ln.Bond = &BondOpts{
				Mode:       v.Mode.String(),
				HashPolicy: v.XmitHashPolicy.String(),
				MinLinks:   v.MinLinks,
				MIIMon:     v.Miimon,
			}
		default:
			if l.Type() == "device" && k.physical(a.Name) {
				ln.Kind = Physical
			} else {
				ln.Kind = Other
			}
		}
		if ln.Kind == Physical || ln.Kind == Bond {
			p := ingressPrios(l)
			ln.DropTagged = p[prioDropTagged] && p[prioPassPrioTagged]
		}
		s.Links[a.Name] = ln
	}
	vlans, err := netlink.BridgeVlanList()
	if err != nil {
		return nil, err
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
	case OpCreateBond:
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
		return netlink.LinkSetMaster(l, m)
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
	case OpSetFlowControl, OpSetMaxLearned:
		return fmt.Errorf("%s: %w", op, ErrUnsupported)
	}
	return fmt.Errorf("unknown op %d", op.Kind)
}
