//go:build linux

package dataplane

import (
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
	Addrs    map[string][]string `json:"addrs,omitempty"` // port -> addresses switchd added
	IPv6Fwd  bool                `json:"ipv6_forwarding,omitempty"`
	AcceptRA []string            `json:"accept_ra,omitempty"` // interfaces switched from accept_ra 1 to 2
}

func (k *Netlink) l3StatePath() string { return filepath.Join(k.StateDir, "l3-owned.json") }

func (k *Netlink) loadL3() l3Owned {
	st := l3Owned{Addrs: map[string][]string{}}
	if raw, err := os.ReadFile(k.l3StatePath()); err == nil {
		_ = json.Unmarshal(raw, &st)
		if st.Addrs == nil {
			st.Addrs = map[string][]string{}
		}
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

	want := map[string]bool{}
	for _, i := range l.Ifs {
		want[i.Name] = true
		c, err := k.syncL3If(i, &st)
		note(c, err)
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

	c, warnings, err := syncRoutes(l.Routes)
	note(c, err)

	// Devices that are no longer configured.
	links, err := netlink.LinkList()
	if err != nil {
		errs = append(errs, err)
	}
	for _, ln := range links {
		name := ln.Attrs().Name
		if isOwnL3Device(name) && ln.Type() == "vlan" && !want[name] {
			note(true, netlink.LinkDel(ln))
		}
	}

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

// syncL3If converges one routed interface.
func (k *Netlink) syncL3If(i L3If, st *l3Owned) (bool, error) {
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

	// Forwarding only on switchd's own L3 interfaces; no redirects; loose
	// reverse-path filter.
	for f, v := range map[string]string{"forwarding": "1", "send_redirects": "0", "rp_filter": "2"} {
		c, err := writeSysctl("/proc/sys/net/ipv4/conf/"+i.Name+"/"+f, v)
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
func syncRoutes(want []Route) (bool, []string, error) {
	cur, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Protocol: RouteProto}, netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return false, nil, err
	}
	changed := false
	var errs []error
	var warnings []string
	keep := map[string]bool{}
	for _, r := range want {
		nr := &netlink.Route{Protocol: RouteProto, Priority: RouteMetric, Table: unix.RT_TABLE_MAIN,
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
		keep[r.Prefix.String()] = true
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
		if c.Dst == nil {
			continue
		}
		p, ok := netip.AddrFromSlice(c.Dst.IP)
		ones, _ := c.Dst.Mask.Size()
		if ok && keep[netip.PrefixFrom(p.Unmap(), ones).String()] {
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
		if c.Dst == nil || c.Dst.String() != r.Dst.String() || c.Type != r.Type || c.Priority != r.Priority {
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
