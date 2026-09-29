//go:build linux

package dataplane

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Management plane objects owned by switchd.
const (
	MgmtVRF   = "mgmt"
	MgmtIRB   = "mgmt0" // VLAN interface on the bridge (IRB-like)
	mgmtTable = 100
)

// l3mdevSysctls let services listening in the default VRF (sshd) accept
// connections that arrive through the management VRF.
var l3mdevSysctls = []string{
	"/proc/sys/net/ipv4/tcp_l3mdev_accept",
	"/proc/sys/net/ipv4/udp_l3mdev_accept",
}

// SyncMgmt converges the management interface (reference 5.2). With m ==
// nil everything switchd created for it is removed; without a management
// block nothing else of the host's network configuration is touched.
func (k *Netlink) SyncMgmt(m *Mgmt) (bool, error) {
	changed := false
	note := func(err error) error {
		changed = true
		return err
	}
	vrf, _ := netlink.LinkByName(MgmtVRF)
	if m == nil {
		if vrf == nil {
			return false, nil
		}
		if err := k.releaseVRFMembers(vrf, ""); err != nil {
			return true, err
		}
		return true, netlink.LinkDel(vrf)
	}
	if m.VLAN == 0 && m.Port == "" {
		return false, errors.New("management: neither vlan nor interface")
	}

	for _, p := range l3mdevSysctls {
		if raw, err := os.ReadFile(p); err == nil && string(raw) != "1\n" {
			if err := os.WriteFile(p, []byte("1"), 0o644); err != nil {
				return changed, err
			}
			changed = true
		}
	}

	if vrf == nil {
		if err := note(netlink.LinkAdd(&netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: MgmtVRF}, Table: mgmtTable})); err != nil {
			return changed, fmt.Errorf("creating VRF %s: %w", MgmtVRF, err)
		}
		var err error
		if vrf, err = netlink.LinkByName(MgmtVRF); err != nil {
			return changed, err
		}
	}
	if vrf.Attrs().Flags&net.FlagUp == 0 {
		if err := note(netlink.LinkSetUp(vrf)); err != nil {
			return changed, err
		}
	}

	// The IP interface: mgmt0 on the bridge, or the dedicated port.
	var ipIf netlink.Link
	br, _ := netlink.LinkByName(BridgeName)
	if m.VLAN != 0 {
		if br == nil {
			return changed, errors.New("management: bridge missing")
		}
		cur, _ := netlink.LinkByName(MgmtIRB)
		if v, ok := cur.(*netlink.Vlan); cur != nil && (!ok || v.VlanId != m.VLAN || v.ParentIndex != br.Attrs().Index) {
			if err := note(netlink.LinkDel(cur)); err != nil {
				return changed, err
			}
			cur = nil
		}
		if cur == nil {
			err := note(netlink.LinkAdd(&netlink.Vlan{
				LinkAttrs: netlink.LinkAttrs{Name: MgmtIRB, ParentIndex: br.Attrs().Index},
				VlanId:    m.VLAN,
			}))
			if err != nil {
				return changed, fmt.Errorf("creating %s: %w", MgmtIRB, err)
			}
			if cur, err = netlink.LinkByName(MgmtIRB); err != nil {
				return changed, err
			}
		}
		ipIf = cur
	} else {
		if cur, err := netlink.LinkByName(MgmtIRB); err == nil {
			if err := note(netlink.LinkDel(cur)); err != nil {
				return changed, err
			}
		}
		p, err := netlink.LinkByName(m.Port)
		if err != nil {
			return changed, fmt.Errorf("management port %s: %w", m.Port, err)
		}
		ipIf = p
	}
	// Anything else in the VRF (e.g. the previous dedicated port) leaves it.
	if err := k.releaseVRFMembers(vrf, ipIf.Attrs().Name); err != nil {
		return changed, err
	}
	if ipIf.Attrs().MasterIndex != vrf.Attrs().Index {
		if err := note(netlink.LinkSetMaster(ipIf, vrf)); err != nil {
			return changed, err
		}
	}
	if ipIf.Attrs().Flags&net.FlagUp == 0 {
		if err := note(netlink.LinkSetUp(ipIf)); err != nil {
			return changed, err
		}
	}

	c, err := syncAddrs(ipIf, m.Addrs)
	changed = changed || c
	if err != nil {
		return changed, err
	}
	c, err = syncGateways(ipIf, m.Gateways)
	return changed || c, err
}

// releaseVRFMembers takes every member of the VRF except keep out of it:
// down, no master, no addresses.
func (k *Netlink) releaseVRFMembers(vrf netlink.Link, keep string) error {
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	for _, l := range links {
		a := l.Attrs()
		if a.MasterIndex != vrf.Attrs().Index || a.Name == keep {
			continue
		}
		if a.Name == MgmtIRB {
			if err := netlink.LinkDel(l); err != nil {
				return err
			}
			continue
		}
		if err := netlink.LinkSetDown(l); err != nil {
			return err
		}
		if err := netlink.LinkSetNoMaster(l); err != nil {
			return err
		}
		addrs, _ := netlink.AddrList(l, netlink.FAMILY_ALL)
		for _, ad := range addrs {
			if !ad.IP.IsLinkLocalUnicast() {
				_ = netlink.AddrDel(l, &ad)
			}
		}
	}
	return nil
}

// syncAddrs makes want the addresses of l (IPv6 link-local excluded). New
// addresses are added before old ones are removed.
func syncAddrs(l netlink.Link, want []string) (bool, error) {
	cur, err := netlink.AddrList(l, netlink.FAMILY_ALL)
	if err != nil {
		return false, err
	}
	changed := false
	have := map[string]netlink.Addr{}
	for _, a := range cur {
		if !a.IP.IsLinkLocalUnicast() {
			have[a.IPNet.String()] = a
		}
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		ad, err := netlink.ParseAddr(w)
		if err != nil {
			return changed, fmt.Errorf("management address %s: %w", w, err)
		}
		key := ad.IPNet.String()
		wantSet[key] = true
		if _, ok := have[key]; !ok {
			if err := netlink.AddrAdd(l, ad); err != nil && !errors.Is(err, unix.EEXIST) {
				return changed, err
			}
			changed = true
		}
	}
	for key, a := range have {
		if !wantSet[key] {
			if err := netlink.AddrDel(l, &a); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

// syncGateways makes the default routes of the management table point to
// gws (one per family).
func syncGateways(l netlink.Link, gws []string) (bool, error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: mgmtTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return false, err
	}
	changed := false
	var want []net.IP
	for _, g := range gws {
		ip := net.ParseIP(g)
		if ip == nil {
			return false, fmt.Errorf("gateway %s: invalid address", g)
		}
		want = append(want, ip)
	}
	isDefault := func(r netlink.Route) bool {
		return r.Dst == nil || (r.Dst.IP.IsUnspecified() && isZeroMask(r.Dst.Mask))
	}
	for _, gw := range want {
		found := slices.ContainsFunc(routes, func(r netlink.Route) bool { return isDefault(r) && r.Gw.Equal(gw) })
		if !found {
			r := &netlink.Route{Table: mgmtTable, Gw: gw, LinkIndex: l.Attrs().Index}
			if gw.To4() == nil {
				r.Family = netlink.FAMILY_V6
			}
			if err := netlink.RouteReplace(r); err != nil {
				return changed, fmt.Errorf("default route via %s: %w", gw, err)
			}
			changed = true
		}
	}
	for _, r := range routes {
		if isDefault(r) && r.Gw != nil && !slices.ContainsFunc(want, func(g net.IP) bool { return g.Equal(r.Gw) }) {
			if err := netlink.RouteDel(&r); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

func isZeroMask(m net.IPMask) bool {
	for _, b := range m {
		if b != 0 {
			return false
		}
	}
	return true
}
