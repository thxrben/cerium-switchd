//go:build linux

package netdev

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/thxrben/cerium-switchd/lib/sys/nlx"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Kernel protocol ids of the switch's routes (reference 5.8): static
// routes (and a DHCP lease's default route) with switchd's own id, the
// routing protocols with the standard ones. The metric keeps them from
// replacing a route the operating system installed for the same
// destination.
const (
	ProtoStatic = 250
	ProtoOSPF   = unix.RTPROT_OSPF // 188
	ProtoBGP    = unix.RTPROT_BGP  // 186
	RouteMetric = 20
)

// Route is a route to install.
type Route struct {
	VRF      string // routing instance's VRF device ("" = the main table)
	Prefix   netip.Prefix
	NextHops []netip.Addr
	// Devs are the next hops' devices (same order; "" lets the kernel
	// resolve the gateway).
	Devs    []string
	Discard bool
	Proto   int // kernel protocol id (0: ProtoStatic)
}

// vrfTable returns the routing table of a VRF device (0: none), cached in
// tables.
func vrfTable(name string, tables map[string]int) int {
	if t, ok := tables[name]; ok {
		return t
	}
	t := 0
	if l, err := nlx.LinkByName(name); err == nil {
		if v, ok := l.(*netlink.Vrf); ok {
			t = int(v.Table)
		}
	}
	tables[name] = t
	return t
}

// SyncRoutes makes the kernel's routes with the protocol ids protos (in
// every table but skipTable) match want: only routes that differ are
// replaced, routes of these ids that are not wanted are removed. A route
// whose device does not exist or whose next hop is not reachable yet is
// skipped and reported (call again later).
func SyncRoutes(want []Route, protos []int, skipTable int) (bool, []string, error) {
	var cur []netlink.Route
	for _, p := range protos {
		rs, err := nlx.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Protocol: netlink.RouteProtocol(p), Table: unix.RT_TABLE_UNSPEC},
			netlink.RT_FILTER_PROTOCOL|netlink.RT_FILTER_TABLE)
		if err != nil {
			return false, nil, err
		}
		cur = append(cur, rs...)
	}
	devIndex := map[string]int{}
	index := func(dev string) int {
		if dev == "" {
			return 0
		}
		if i, ok := devIndex[dev]; ok {
			return i
		}
		i := 0
		if ln, err := nlx.LinkByName(dev); err == nil {
			i = ln.Attrs().Index
		}
		devIndex[dev] = i
		return i
	}
	// Kernel routes by table and destination: the comparison stays linear
	// for large routing tables (BGP).
	curBy := map[string][]netlink.Route{}
	for _, c := range cur {
		if c.Dst != nil {
			k := fmt.Sprintf("%d %s", c.Table, c.Dst)
			curBy[k] = append(curBy[k], c)
		}
	}
	tables := map[string]int{}
	changed := false
	var errs []error
	var warnings []string
	keep := map[string]bool{}
	for _, r := range want {
		table := unix.RT_TABLE_MAIN
		if r.VRF != "" {
			if table = vrfTable(r.VRF, tables); table == 0 {
				warnings = append(warnings, fmt.Sprintf("route %s: routing instance %s does not exist yet", r.Prefix, r.VRF))
				continue
			}
		}
		proto := r.Proto
		if proto == 0 {
			proto = ProtoStatic
		}
		nr := &netlink.Route{Protocol: netlink.RouteProtocol(proto), Priority: RouteMetric, Table: table,
			Dst: &net.IPNet{IP: r.Prefix.Addr().AsSlice(), Mask: net.CIDRMask(r.Prefix.Bits(), r.Prefix.Addr().BitLen())}}
		if r.Prefix.Addr().Is6() {
			nr.Family = netlink.FAMILY_V6
		} else {
			nr.Family = netlink.FAMILY_V4
		}
		// The kernel reports unicast routes as RTN_UNICAST: the same here, or
		// routePresent never matches and every pass replaces the route.
		nr.Type = unix.RTN_UNICAST
		switch {
		case r.Discard:
			nr.Type = unix.RTN_BLACKHOLE
		case len(r.NextHops) == 1:
			if r.NextHops[0].IsValid() {
				nr.Gw = r.NextHops[0].AsSlice()
			}
			if len(r.Devs) > 0 {
				if nr.LinkIndex = index(r.Devs[0]); nr.LinkIndex == 0 {
					warnings = append(warnings, fmt.Sprintf("route %s: interface %s does not exist yet", r.Prefix, r.Devs[0]))
					continue
				}
			}
		default:
			for n, h := range r.NextHops {
				nh := &netlink.NexthopInfo{}
				if h.IsValid() {
					nh.Gw = h.AsSlice()
				}
				if n < len(r.Devs) {
					nh.LinkIndex = index(r.Devs[n])
				}
				nr.MultiPath = append(nr.MultiPath, nh)
			}
		}
		keep[fmt.Sprintf("%d %s %d", table, r.Prefix, proto)] = true
		if routePresent(curBy[fmt.Sprintf("%d %s", table, nr.Dst)], nr) {
			continue
		}
		if err := nlx.RouteReplace(nr); err != nil {
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
		if c.Dst == nil || (skipTable != 0 && c.Table == skipTable) {
			continue
		}
		p, ok := netip.AddrFromSlice(c.Dst.IP)
		ones, _ := c.Dst.Mask.Size()
		if ok && keep[fmt.Sprintf("%d %s %d", c.Table, netip.PrefixFrom(p.Unmap(), ones), c.Protocol)] {
			continue
		}
		if err := nlx.RouteDel(&c); err != nil {
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
		if c.Dst == nil || c.Table != r.Table || c.Dst.String() != r.Dst.String() || c.Type != r.Type || c.Priority != r.Priority ||
			c.Protocol != r.Protocol {
			continue
		}
		if len(r.MultiPath) == 0 {
			if len(c.MultiPath) == 0 && c.Gw.Equal(r.Gw) && (r.LinkIndex == 0 || c.LinkIndex == r.LinkIndex) {
				return true
			}
			continue
		}
		if len(c.MultiPath) != len(r.MultiPath) {
			continue
		}
		same := true
		for _, h := range r.MultiPath {
			if !slices.ContainsFunc(c.MultiPath, func(x *netlink.NexthopInfo) bool {
				return x.Gw.Equal(h.Gw) && (h.LinkIndex == 0 || x.LinkIndex == h.LinkIndex)
			}) {
				same = false
			}
		}
		if same {
			return true
		}
	}
	return false
}
