//go:build linux

package dataplane

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// The underlay of the stack tunnels (reference 5.2, docs/stack-protocol.md
// "Stack tunnels"): the hidden VRF swstack with the stacking ports, the
// member's address, permanent neighbours per stacking link and routes to
// the other members along the shortest paths of the stack topology.

// StackUnderlay is the desired underlay of this member.
type StackUnderlay struct {
	Member int
	// Ports are all designated stacking ports that exist (kernel names).
	Ports []string
	// Links are the stacking links with an up member session.
	Links []StackLink
	// Hops: per reachable member, the neighbours on its shortest paths.
	Hops map[int][]int
}

// StackLink is one stacking link with an up member session.
type StackLink struct {
	Port     string
	Neighbor int
	MAC      net.HardwareAddr
}

// ensureStackVRF creates the hidden VRF if needed and returns its index.
func ensureStackVRF() (int, bool, error) {
	changed := false
	ln, err := netlink.LinkByName(StackVRF)
	if v, ok := ln.(*netlink.Vrf); err == nil && (!ok || v.Table != StackTable) {
		if err := netlink.LinkDel(ln); err != nil {
			return 0, false, err
		}
		ln, err = nil, errors.New("recreate")
	}
	if err != nil {
		if err := netlink.LinkAdd(&netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: StackVRF}, Table: StackTable}); err != nil {
			return 0, false, fmt.Errorf("%s: %w", StackVRF, err)
		}
		if ln, err = netlink.LinkByName(StackVRF); err != nil {
			return 0, false, err
		}
		changed = true
	}
	if ln.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(ln); err != nil {
			return 0, changed, err
		}
		changed = true
	}
	return ln.Attrs().Index, changed, nil
}

// IFLA_VXLAN_DF and its value "set" (not in the netlink library).
const (
	iflaVxlanDF  = 29
	vxlanDFSet   = 1
	iflaVxlanTTL = 5
)

// createTunnel creates a stack tunnel (VXLAN over swstack, outer DF set).
func createTunnel(name string, t TunnelOpts, mtu int) error {
	vrf, _, err := ensureStackVRF()
	if err != nil {
		return err
	}
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK)
	req.AddData(nl.NewIfInfomsg(unix.AF_UNSPEC))
	req.AddData(nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(name)))
	if mtu > 0 {
		req.AddData(nl.NewRtAttr(unix.IFLA_MTU, nl.Uint32Attr(uint32(mtu))))
	}
	info := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	info.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated("vxlan"))
	data := info.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	local, remote := t.Local.As4(), t.Remote.As4()
	data.AddRtAttr(nl.IFLA_VXLAN_ID, nl.Uint32Attr(uint32(t.VNI)))
	data.AddRtAttr(nl.IFLA_VXLAN_LINK, nl.Uint32Attr(uint32(vrf)))
	data.AddRtAttr(nl.IFLA_VXLAN_LOCAL, local[:])
	data.AddRtAttr(nl.IFLA_VXLAN_GROUP, remote[:])
	data.AddRtAttr(iflaVxlanTTL, nl.Uint8Attr(16))
	data.AddRtAttr(nl.IFLA_VXLAN_LEARNING, nl.Uint8Attr(0))
	data.AddRtAttr(nl.IFLA_VXLAN_PORT, htons16(StackUDPPort))
	data.AddRtAttr(iflaVxlanDF, nl.Uint8Attr(vxlanDFSet))
	req.AddData(info)
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("%s: creating the stack tunnel: %w", name, err)
	}
	return nil
}

func htons16(v uint16) []byte { return []byte{byte(v >> 8), byte(v)} }

// SyncStackUnderlay converges the underlay; it reports whether anything
// changed. Only the VRF, its routes, the neighbours on stacking ports and
// the stacking ports' VRF membership and sysctls are touched.
func SyncStackUnderlay(u StackUnderlay) (bool, error) {
	vrf, changed, err := ensureStackVRF()
	if err != nil {
		return false, err
	}
	var errs []error
	note := func(c bool, err error) {
		changed = changed || c
		if err != nil {
			errs = append(errs, err)
		}
	}
	vrfLink, err := netlink.LinkByIndex(vrf)
	if err != nil {
		return changed, err
	}

	// The member's address on the VRF device.
	own := netip.PrefixFrom(StackAddr(u.Member), 32)
	addrs, _ := netlink.AddrList(vrfLink, netlink.FAMILY_V4)
	has := false
	for _, a := range addrs {
		p, _ := netip.AddrFromSlice(a.IP.To4())
		if p == own.Addr() {
			has = true
			continue
		}
		note(true, netlink.AddrDel(vrfLink, &a))
	}
	if !has && u.Member > 0 {
		note(true, netlink.AddrAdd(vrfLink, &netlink.Addr{IPNet: &net.IPNet{IP: own.Addr().AsSlice(), Mask: net.CIDRMask(32, 32)}}))
	}

	// Ports: in the VRF, IPv4 forwarding on, no reverse-path filter, and
	// routes over a link without carrier are ignored at once.
	index := map[string]int{}
	links, _ := netlink.LinkList()
	for _, ln := range links {
		a := ln.Attrs()
		if slices.Contains(u.Ports, a.Name) {
			index[a.Name] = a.Index
			if a.MasterIndex != vrf {
				// Enslaving restarts the port once (the kernel cycles it).
				note(true, netlink.LinkSetMaster(ln, vrfLink))
			}
			for f, v := range map[string]string{"forwarding": "1", "rp_filter": "0", "ignore_routes_with_linkdown": "1"} {
				note(writeSysctl("/proc/sys/net/ipv4/conf/"+a.Name+"/"+f, v))
			}
		} else if a.MasterIndex == vrf && ln.Type() != "vxlan" {
			note(true, netlink.LinkSetNoMaster(ln)) // no longer a stacking port
		}
	}

	// Neighbours: the peer's address on each link, bound to the MAC the
	// stacking protocol learned (no ARP on stacking ports).
	type nkey struct {
		port string
		ip   netip.Addr
	}
	want := map[nkey]net.HardwareAddr{}
	for _, l := range u.Links {
		if index[l.Port] != 0 {
			want[nkey{l.Port, StackAddr(l.Neighbor)}] = l.MAC
		}
	}
	for port, idx := range index {
		neighs, _ := netlink.NeighList(idx, netlink.FAMILY_V4)
		for _, n := range neighs {
			ip, _ := netip.AddrFromSlice(n.IP.To4())
			mac, ok := want[nkey{port, ip}]
			if ok && n.State == netlink.NUD_PERMANENT && n.HardwareAddr.String() == mac.String() {
				delete(want, nkey{port, ip})
				continue
			}
			if !ok {
				note(true, netlink.NeighDel(&n))
			}
		}
	}
	for k, mac := range want {
		note(true, netlink.NeighSet(&netlink.Neigh{LinkIndex: index[k.port], Family: netlink.FAMILY_V4,
			State: netlink.NUD_PERMANENT, IP: k.ip.AsSlice(), HardwareAddr: mac}))
	}

	// Routes: every reachable member over every link to a neighbour on one
	// of its shortest paths.
	byNeighbor := map[int][]string{}
	for _, l := range u.Links {
		if index[l.Port] != 0 {
			byNeighbor[l.Neighbor] = append(byNeighbor[l.Neighbor], l.Port)
		}
	}
	routes := map[netip.Addr][]stackHop{}
	for m, ns := range u.Hops {
		var hops []stackHop
		for _, n := range ns {
			for _, p := range byNeighbor[n] {
				hops = append(hops, stackHop{index[p], StackAddr(n)})
			}
		}
		if len(hops) > 0 {
			slices.SortFunc(hops, func(a, b stackHop) int { return a.idx - b.idx })
			routes[StackAddr(m)] = hops
		}
	}
	cur, _ := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: StackTable}, netlink.RT_FILTER_TABLE)
	for _, r := range cur {
		if r.Protocol != RouteProto || r.Dst == nil {
			continue
		}
		dst, _ := netip.AddrFromSlice(r.Dst.IP.To4())
		hops, ok := routes[dst]
		if ok && sameHops(r, hops) {
			delete(routes, dst)
			continue
		}
		if !ok {
			note(true, netlink.RouteDel(&r))
		}
	}
	for dst, hops := range routes {
		r := &netlink.Route{Table: StackTable, Protocol: RouteProto,
			Dst: &net.IPNet{IP: dst.AsSlice(), Mask: net.CIDRMask(32, 32)}}
		for _, h := range hops {
			r.MultiPath = append(r.MultiPath, &netlink.NexthopInfo{LinkIndex: h.idx, Gw: h.gw.AsSlice(), Flags: int(netlink.FLAG_ONLINK)})
		}
		note(true, netlink.RouteReplace(r))
	}
	return changed, errors.Join(errs...)
}

// stackHop is one next hop of an underlay route.
type stackHop struct {
	idx int
	gw  netip.Addr
}

// sameHops compares an installed route with the wanted next hops (sorted
// by interface index).
func sameHops(r netlink.Route, want []stackHop) bool {
	var have []stackHop
	add := func(idx int, gw net.IP) {
		a, _ := netip.AddrFromSlice(gw.To4())
		have = append(have, stackHop{idx, a})
	}
	if len(r.MultiPath) == 0 {
		add(r.LinkIndex, r.Gw)
	}
	for _, nh := range r.MultiPath {
		add(nh.LinkIndex, nh.Gw)
	}
	slices.SortFunc(have, func(a, b stackHop) int { return a.idx - b.idx })
	return slices.Equal(have, want)
}
