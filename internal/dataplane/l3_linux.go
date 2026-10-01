//go:build linux

package dataplane

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Static routes are installed with this protocol id, so switchd only ever
// removes its own routes, and with this metric, so they never replace a
// route the operating system installed for the same destination.
const (
	RouteProto  = 250
	RouteMetric = 20
)

// l3Owned is what switchd changed outside its own devices, so that it can
// be undone (addresses on ports, IPv6 forwarding, accept_ra).
type l3Owned struct {
	Addrs    map[string][]string `json:"addrs,omitempty"`  // port -> addresses switchd added
	Tables   map[string]int      `json:"tables,omitempty"` // VRF (routing instance) -> routing table
	IPv6Fwd  bool                `json:"ipv6_forwarding,omitempty"`
	AcceptRA []string            `json:"accept_ra,omitempty"` // interfaces switched from accept_ra 1 to 2
}

func (k *Netlink) l3StatePath() string { return filepath.Join(k.StateDir, "l3-owned.json") }

func (k *Netlink) loadL3() l3Owned {
	st := l3Owned{}
	if raw, err := os.ReadFile(k.l3StatePath()); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if st.Addrs == nil {
		st.Addrs = map[string][]string{}
	}
	if st.Tables == nil {
		st.Tables = map[string]int{}
	}
	return st
}

func (k *Netlink) saveL3(st l3Owned) error {
	if k.StateDir == "" {
		return nil
	}
	raw, _ := json.Marshal(st)
	tmp := k.l3StatePath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, k.l3StatePath())
}

// isOwnL3Device reports whether a kernel name is an irb or subinterface
// device created by switchd.
func isOwnL3Device(name string) bool {
	return strings.HasPrefix(name, "irb.") || strings.HasPrefix(name, "sw-")
}

// SyncSelfVLANs makes the bridge device a member of vids.
func (k *Netlink) SyncSelfVLANs(vids []int, prune bool) (bool, error) {
	br, err := netlink.LinkByName(BridgeName)
	if err != nil {
		if len(vids) == 0 {
			return false, nil
		}
		return false, errors.New("bridge missing")
	}
	all, err := netlink.BridgeVlanList()
	if err != nil {
		return false, err
	}
	have := map[int]bool{}
	for _, v := range all[int32(br.Attrs().Index)] {
		have[int(v.Vid)] = true
	}
	changed := false
	for _, v := range vids {
		if !have[v] {
			if err := netlink.BridgeVlanAdd(br, uint16(v), false, false, true, false); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	if prune {
		for v := range have {
			if !slices.Contains(vids, v) {
				if err := netlink.BridgeVlanDel(br, uint16(v), false, false, true, false); err != nil {
					return changed, err
				}
				changed = true
			}
		}
	}
	return changed, nil
}

func writeSysctl(path, value string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(raw)) == value {
		return false, nil
	}
	return true, os.WriteFile(path, []byte(value), 0o644)
}

// SyncL3 converges the routed interfaces and static routes. Order: devices
// and addresses first (new before old), then routes, then removal of
// devices that are no longer configured. Routes that cannot be installed
// yet (next hop unreachable) are returned as warnings, not errors.
func (k *Netlink) SyncL3(l *L3) (bool, []string, error) {
	if l == nil {
		l = &L3{}
	}
	st := k.loadL3()
	changed := false
	var errs []error
	if c, err := k.syncGatewayMAC(l); err != nil {
		errs = append(errs, err)
	} else {
		changed = changed || c
	}
	note := func(c bool, err error) {
		changed = changed || c
		if err != nil {
			errs = append(errs, err)
		}
	}

	// IPv6 routing needs forwarding for the whole system; interfaces that
	// autoconfigure from router advertisements keep doing so (accept_ra 2).
	if l.IPv6() && !st.IPv6Fwd {
		links, _ := netlink.LinkList()
		for _, ln := range links {
			name := ln.Attrs().Name
			p := "/proc/sys/net/ipv6/conf/" + name + "/accept_ra"
			if raw, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(raw)) == "1" {
				if err := os.WriteFile(p, []byte("2"), 0o644); err == nil {
					st.AcceptRA = append(st.AcceptRA, name)
				}
			}
		}
		// New interfaces (e.g. a hot-plugged NIC) default to 2 as well.
		_, _ = writeSysctl("/proc/sys/net/ipv6/conf/default/accept_ra", "2")
		c, err := writeSysctl("/proc/sys/net/ipv6/conf/all/forwarding", "1")
		note(c, err)
		if err == nil {
			st.IPv6Fwd = true
		}
	}

	// Routing instances: one VRF device each, with a routing table that
	// stays the same across restarts.
	vrfs := map[string]VRF{}
	tables := map[string]int{}
	for _, v := range l.VRFs {
		vrfs[v.Name] = v
		t, c, err := k.syncVRF(v, &st)
		note(c, err)
		tables[v.Name] = t
	}
	if len(l.VRFs) > 0 {
		// Services listening in the default VRF (e.g. the OS SSH server)
		// accept connections that arrive through an instance.
		for _, p := range l3mdevSysctls {
			c, err := writeSysctl(p, "1")
			note(c, err)
		}
	}

	// DHCP leases become addresses of their units and default routes of
	// their instances (unless the instance has a static default route).
	var dhcpIfs []DHCPIf
	for _, i := range l.Ifs {
		if i.DHCP {
			dhcpIfs = append(dhcpIfs, DHCPIf{Name: i.Name, Unit: i.Unit, VRF: i.VRF})
		}
	}
	if k.DHCP != nil {
		leases := k.DHCP(dhcpIfs)
		ifs := slices.Clone(l.Ifs)
		routes := slices.Clone(l.Routes)
		defaulted := map[string]bool{}
		for n, i := range ifs {
			le, ok := leases[i.Name]
			if !i.DHCP || !ok {
				continue
			}
			ifs[n].Addrs = append(slices.Clone(i.Addrs), le.Addr)
			static := slices.ContainsFunc(l.Routes, func(r Route) bool { return r.VRF == i.VRF && r.Prefix.Bits() == 0 && r.Prefix.Addr().Is4() })
			if le.Router.IsValid() && !static && !defaulted[i.VRF] {
				defaulted[i.VRF] = true
				routes = append(routes, Route{VRF: i.VRF, Prefix: netip.MustParsePrefix("0.0.0.0/0"), NextHops: []netip.Addr{le.Router}})
			}
		}
		l = &L3{VRFs: l.VRFs, Ifs: ifs, Routes: routes, CME: l.CME, Bare: l.Bare, Unconfigured: l.Unconfigured}
	}

	// The chassis management interface, after its routing instance exists.
	note(k.syncCME(l.CME))
	unconf := map[string]bool{}
	for _, n := range l.Unconfigured {
		unconf[n] = true
	}
	note(syncBare(l.Bare, unconf))

	want := map[string]bool{}
	var protect []string // data L3 interfaces: only ping/ND/replies reach the switch
	for _, i := range l.Ifs {
		want[i.Name] = true
		c, err := k.syncL3If(i, &st, vrfs[i.VRF])
		note(c, err)
		if !vrfs[i.VRF].Mgmt {
			protect = append(protect, i.Name)
		}
	}
	for _, v := range l.VRFs {
		if !v.Mgmt {
			protect = append(protect, v.Name)
		}
	}
	// Addresses switchd put on ports that are no longer routed.
	for port, addrs := range st.Addrs {
		if want[port] {
			continue
		}
		if ln, err := netlink.LinkByName(port); err == nil {
			for _, a := range addrs {
				if ad, err := netlink.ParseAddr(a); err == nil {
					if err := netlink.AddrDel(ln, ad); err == nil {
						changed = true
					}
				}
			}
		}
		delete(st.Addrs, port)
	}

	c, warnings, err := syncRoutes(l.Routes, tables)
	note(c, err)

	// Devices that are no longer configured.
	links, err := netlink.LinkList()
	if err != nil {
		errs = append(errs, err)
	}
	for _, ln := range links {
		name := ln.Attrs().Name
		if (isOwnL3Device(name) || name == legacyMgmtIRB) && ln.Type() == "vlan" && !want[name] {
			note(true, netlink.LinkDel(ln))
		}
	}
	// Routing instances that are no longer configured (and the management
	// VRF of older versions): members leave, then the VRF goes. (Fresh list:
	// devices were deleted above.)
	if links, err = netlink.LinkList(); err != nil {
		errs = append(errs, err)
	}
	for _, ln := range links {
		name := ln.Attrs().Name
		_, ours := st.Tables[name]
		legacy := name == legacyMgmtVRF && ln.Type() == "vrf"
		if (!ours && !legacy) || ln.Type() != "vrf" {
			continue
		}
		if _, keep := vrfs[name]; keep {
			continue
		}
		for _, m := range links {
			if m.Attrs().MasterIndex == ln.Attrs().Index {
				note(true, netlink.LinkSetNoMaster(m))
			}
		}
		note(true, netlink.LinkDel(ln))
		delete(st.Tables, name)
	}
	c, err = k.syncProtect(protect)
	note(c, err)
	mgmtTable := 0
	for _, v := range l.VRFs {
		if v.Mgmt {
			mgmtTable = tables[v.Name]
		}
	}
	var mgmtAddrs []netip.Addr
	if l.CME != nil {
		for _, a := range l.CME.Addrs {
			mgmtAddrs = append(mgmtAddrs, a.Addr())
		}
	}
	for _, i := range l.Ifs {
		if vrfs[i.VRF].Mgmt {
			for _, a := range i.Addrs {
				mgmtAddrs = append(mgmtAddrs, a.Addr())
			}
		}
	}
	c, err = syncOriginRules(mgmtTable, mgmtAddrs)
	note(c, err)

	if !l.IPv6() && st.IPv6Fwd {
		c, err := writeSysctl("/proc/sys/net/ipv6/conf/all/forwarding", "0")
		note(c, err)
		for _, name := range st.AcceptRA {
			_, _ = writeSysctl("/proc/sys/net/ipv6/conf/"+name+"/accept_ra", "1")
		}
		_, _ = writeSysctl("/proc/sys/net/ipv6/conf/default/accept_ra", "1")
		st.IPv6Fwd, st.AcceptRA = false, nil
	}
	if err := k.saveL3(st); err != nil {
		errs = append(errs, err)
	}
	return changed, warnings, errors.Join(errs...)
}

// Kernel objects of older versions (the management VRF before routing
// instances), removed when found.
const (
	legacyMgmtVRF = "mgmt"
	legacyMgmtIRB = "mgmt0"
)

// l3mdevSysctls let services listening in the default VRF accept
// connections that arrive through a VRF.
var l3mdevSysctls = []string{
	"/proc/sys/net/ipv4/tcp_l3mdev_accept",
	"/proc/sys/net/ipv4/udp_l3mdev_accept",
}

// syncVRF makes sure the VRF of a routing instance exists and is up; it
// returns its routing table.
func (k *Netlink) syncVRF(v VRF, st *l3Owned) (int, bool, error) {
	table := st.Tables[v.Name]
	if table == 0 {
		used := map[int]bool{}
		for _, t := range st.Tables {
			used[t] = true
		}
		table = 1001
		if v.Mgmt && !used[100] {
			table = 100 // the management table of older versions
		}
		for used[table] {
			table++
		}
		st.Tables[v.Name] = table
	}
	changed := false
	ln, _ := netlink.LinkByName(v.Name)
	if ln != nil && ln.Type() != "vrf" {
		// Never replace another device (commit check rejects the name).
		return table, false, fmt.Errorf("routing instance %s: a %s device of that name exists", v.Name, ln.Type())
	}
	if cur, ok := ln.(*netlink.Vrf); ln != nil && (!ok || int(cur.Table) != table) {
		// Wrong table: recreate (its members re-join below).
		if err := netlink.LinkDel(ln); err != nil {
			return table, changed, err
		}
		ln, changed = nil, true
	}
	if ln == nil {
		if err := netlink.LinkAdd(&netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: v.Name}, Table: uint32(table)}); err != nil {
			return table, changed, fmt.Errorf("creating VRF %s: %w", v.Name, err)
		}
		changed = true
		var err error
		if ln, err = netlink.LinkByName(v.Name); err != nil {
			return table, changed, err
		}
	}
	if ln.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(ln); err != nil {
			return table, changed, err
		}
		changed = true
	}
	return table, changed, nil
}

// syncL3If converges one routed interface.
func (k *Netlink) syncL3If(i L3If, st *l3Owned, vrf VRF) (bool, error) {
	changed := false
	ln, _ := netlink.LinkByName(i.Name)
	if i.Own {
		parent, err := netlink.LinkByName(i.Parent)
		if err != nil {
			return false, fmt.Errorf("%s: parent %s: %w", i.Name, i.Parent, err)
		}
		if v, ok := ln.(*netlink.Vlan); ln != nil && (!ok || v.VlanId != i.VID || v.ParentIndex != parent.Attrs().Index) {
			if err := netlink.LinkDel(ln); err != nil {
				return false, err
			}
			ln, changed = nil, true
		}
		if ln == nil {
			attrs := netlink.LinkAttrs{Name: i.Name, ParentIndex: parent.Attrs().Index}
			if i.MTU > 0 {
				attrs.MTU = i.MTU
			}
			if err := netlink.LinkAdd(&netlink.Vlan{LinkAttrs: attrs, VlanId: i.VID}); err != nil {
				return changed, fmt.Errorf("creating %s: %w", i.Name, err)
			}
			changed = true
			if ln, err = netlink.LinkByName(i.Name); err != nil {
				return changed, err
			}
		}
		if i.MTU > 0 && ln.Attrs().MTU != i.MTU {
			if err := netlink.LinkSetMTU(ln, i.MTU); err != nil {
				return changed, fmt.Errorf("%s mtu: %w", i.Name, err)
			}
			changed = true
		}
		up := ln.Attrs().Flags&net.FlagUp != 0
		if i.Up && !up {
			if err := netlink.LinkSetUp(ln); err != nil {
				return changed, err
			}
			changed = true
		} else if !i.Up && up {
			if err := netlink.LinkSetDown(ln); err != nil {
				return changed, err
			}
			changed = true
		}
	} else if ln == nil {
		return false, fmt.Errorf("%s: no such device", i.Name)
	}

	// Routing instance membership, before the addresses (their routes go
	// into the instance's table).
	master := 0
	if i.VRF != "" {
		v, err := netlink.LinkByName(i.VRF)
		if err != nil {
			return changed, fmt.Errorf("%s: routing instance %s: %w", i.Name, i.VRF, err)
		}
		master = v.Attrs().Index
	}
	if cur := ln.Attrs().MasterIndex; cur != master {
		curIsVRF := false
		if m, err := netlink.LinkByIndex(cur); err == nil && m.Type() == "vrf" {
			curIsVRF = true
		}
		switch {
		case master != 0:
			v, _ := netlink.LinkByIndex(master)
			if err := netlink.LinkSetMaster(ln, v); err != nil {
				return changed, fmt.Errorf("%s: joining %s: %w", i.Name, i.VRF, err)
			}
			changed = true
		case curIsVRF:
			if err := netlink.LinkSetNoMaster(ln); err != nil {
				return changed, err
			}
			changed = true
		}
	}

	// Forwarding only on switchd's own data L3 interfaces (management
	// interfaces are hosts); no redirects; loose reverse-path filter.
	fwd := "1"
	if vrf.Mgmt {
		fwd = "0"
	}
	for f, v := range map[string]string{"forwarding": fwd, "send_redirects": "0", "rp_filter": "2"} {
		c, err := writeSysctl("/proc/sys/net/ipv4/conf/"+i.Name+"/"+f, v)
		changed = changed || c
		if err != nil {
			return changed, err
		}
	}

	// irb units carry the same addresses and MAC on every member of a stack
	// (anycast gateway): duplicate address detection would see the other
	// members' copies and disable them, so it is off there, and addresses
	// that failed it before are added again.
	anycast := i.Own && i.Parent == BridgeName && i.Anycast
	if anycast {
		c, err := writeSysctl("/proc/sys/net/ipv6/conf/"+i.Name+"/accept_dad", "0")
		changed = changed || c
		if err != nil {
			return changed, err
		}
	}

	// Addresses: own devices get exactly the configured ones; on a port only
	// addresses switchd added are ever removed.
	cur, err := netlink.AddrList(ln, netlink.FAMILY_ALL)
	if err != nil {
		return changed, err
	}
	have := map[netip.Prefix]netlink.Addr{}
	for _, a := range cur {
		if anycast && a.Flags&unix.IFA_F_DADFAILED != 0 {
			re := a
			_ = netlink.AddrDel(ln, &a)
			re.Flags = (re.Flags &^ (unix.IFA_F_DADFAILED | unix.IFA_F_TENTATIVE)) | unix.IFA_F_NODAD
			if err := netlink.AddrAdd(ln, &re); err != nil && !errors.Is(err, unix.EEXIST) {
				return changed, fmt.Errorf("%s: address %s after failed DAD: %w", i.Name, a.IPNet, err)
			}
			changed = true
		}
		if a.IP.IsLinkLocalUnicast() {
			continue
		}
		ip, _ := netip.AddrFromSlice(a.IP)
		ones, _ := a.Mask.Size()
		have[netip.PrefixFrom(ip.Unmap(), ones)] = a
	}
	var added []string
	for _, p := range i.Addrs {
		added = append(added, p.String())
		if _, ok := have[p]; ok {
			continue
		}
		ad, err := netlink.ParseAddr(p.String())
		if err != nil {
			return changed, err
		}
		if anycast && p.Addr().Is6() {
			ad.Flags |= unix.IFA_F_NODAD
		}
		if err := netlink.AddrAdd(ln, ad); err != nil && !errors.Is(err, unix.EEXIST) {
			return changed, fmt.Errorf("%s: address %s: %w", i.Name, p, err)
		}
		changed = true
	}
	removable := func(p netip.Prefix) bool {
		return i.Own || slices.Contains(st.Addrs[i.Name], p.String())
	}
	for p, a := range have {
		if !slices.Contains(i.Addrs, p) && removable(p) {
			if err := netlink.AddrDel(ln, &a); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	if !i.Own {
		st.Addrs[i.Name] = added
	}
	return changed, nil
}

// syncRoutes makes switchd's routes in the main table match want. A route
// whose next hop is not reachable yet is skipped (it is retried on every
// reconcile) and reported.
func syncRoutes(want []Route, tables map[string]int) (bool, []string, error) {
	cur, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Protocol: RouteProto, Table: unix.RT_TABLE_UNSPEC},
		netlink.RT_FILTER_PROTOCOL|netlink.RT_FILTER_TABLE)
	if err != nil {
		return false, nil, err
	}
	changed := false
	var errs []error
	var warnings []string
	keep := map[string]bool{}
	for _, r := range want {
		table := unix.RT_TABLE_MAIN
		if r.VRF != "" {
			if table = tables[r.VRF]; table == 0 {
				continue // the instance could not be created (reported)
			}
		}
		nr := &netlink.Route{Protocol: RouteProto, Priority: RouteMetric, Table: table,
			Dst: &net.IPNet{IP: r.Prefix.Addr().AsSlice(), Mask: net.CIDRMask(r.Prefix.Bits(), r.Prefix.Addr().BitLen())}}
		if r.Prefix.Addr().Is6() {
			nr.Family = netlink.FAMILY_V6
		} else {
			nr.Family = netlink.FAMILY_V4
		}
		switch {
		case r.Discard:
			nr.Type = unix.RTN_BLACKHOLE
		case len(r.NextHops) == 1:
			nr.Gw = r.NextHops[0].AsSlice()
		default:
			for _, h := range r.NextHops {
				nr.MultiPath = append(nr.MultiPath, &netlink.NexthopInfo{Gw: h.AsSlice()})
			}
		}
		keep[fmt.Sprintf("%d %s", table, r.Prefix)] = true
		if routePresent(cur, nr) {
			continue
		}
		if err := netlink.RouteReplace(nr); err != nil {
			if errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) {
				warnings = append(warnings, fmt.Sprintf("route %s: next hop not reachable; inactive until it is", r.Prefix))
			} else {
				errs = append(errs, fmt.Errorf("route %s: %w", r.Prefix, err))
			}
			continue
		}
		changed = true
	}
	for _, c := range cur {
		if c.Dst == nil || c.Table == StackTable {
			continue // (the stack tunnels' routes are SyncStackUnderlay's)
		}
		p, ok := netip.AddrFromSlice(c.Dst.IP)
		ones, _ := c.Dst.Mask.Size()
		if ok && keep[fmt.Sprintf("%d %s", c.Table, netip.PrefixFrom(p.Unmap(), ones))] {
			continue
		}
		if err := netlink.RouteDel(&c); err != nil {
			errs = append(errs, err)
			continue
		}
		changed = true
	}
	return changed, warnings, errors.Join(errs...)
}

// routePresent reports whether an identical route exists.
func routePresent(cur []netlink.Route, r *netlink.Route) bool {
	for _, c := range cur {
		if c.Dst == nil || c.Table != r.Table || c.Dst.String() != r.Dst.String() || c.Type != r.Type || c.Priority != r.Priority {
			continue
		}
		if len(r.MultiPath) == 0 {
			if len(c.MultiPath) == 0 && c.Gw.Equal(r.Gw) {
				return true
			}
			continue
		}
		if len(c.MultiPath) != len(r.MultiPath) {
			continue
		}
		same := true
		for _, h := range r.MultiPath {
			if !slices.ContainsFunc(c.MultiPath, func(x *netlink.NexthopInfo) bool { return x.Gw.Equal(h.Gw) }) {
				same = false
			}
		}
		if same {
			return true
		}
	}
	return false
}

// GatewayMAC derives the stack-wide gateway MAC from the stack id: locally
// administered, unicast.
func GatewayMAC(stackID string) net.HardwareAddr {
	return derivedMAC("ceros gateway mac\x00" + stackID)
}

// MemberMAC derives member m's own irb MAC.
func MemberMAC(stackID string, m int) net.HardwareAddr {
	return derivedMAC(fmt.Sprintf("ceros member mac\x00%s\x00%d", stackID, m))
}

func derivedMAC(seed string) net.HardwareAddr {
	if strings.HasSuffix(seed, "\x00") || strings.Contains(seed, "\x00\x00") {
		return nil // no stack id
	}
	h := sha256.Sum256([]byte(seed))
	mac := net.HardwareAddr(h[:6])
	mac[0] = mac[0]&^1 | 2
	return mac
}

// syncGatewayMAC gives the bridge and switchd's irb devices the stack-wide
// MAC. Changing the MAC of an up device does not take it down.
func (k *Netlink) syncGatewayMAC(l *L3) (bool, error) {
	if len(k.GatewayMAC) != 6 {
		return false, nil
	}
	want := map[string]net.HardwareAddr{BridgeName: k.GatewayMAC}
	for _, i := range l.Ifs {
		if i.Own && i.Parent == BridgeName {
			want[i.Name] = k.GatewayMAC
			if !i.Anycast && len(k.MemberMAC) == 6 {
				want[i.Name] = k.MemberMAC
			}
		}
	}
	changed := false
	var errs []error
	for n, mac := range want {
		ln, err := netlink.LinkByName(n)
		if err != nil {
			continue // created later in this sync; the next one sets it
		}
		if ln.Attrs().HardwareAddr.String() == mac.String() {
			continue
		}
		if err := netlink.LinkSetHardwareAddr(ln, mac); err != nil {
			errs = append(errs, fmt.Errorf("%s: gateway MAC: %w", n, err))
			continue
		}
		changed = true
	}
	return changed, errors.Join(errs...)
}
