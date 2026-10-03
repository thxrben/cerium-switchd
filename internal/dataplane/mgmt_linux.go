//go:build linux

package dataplane

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"

	"github.com/thxrben/cerium-switchd/pkg/nlx"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// syncCME converges the chassis management interface (reference 1.8): a
// macvlan device named cme on the active management port. It is created
// down, joins the management instance and gets its addresses before it
// comes up, so coming up announces them (arp_notify, ndisc_notify) and
// the management network learns the new place of the address at once.
func (k *Netlink) syncCME(c *CMEIf) (bool, error) {
	ln, _ := nlx.LinkByName(CMEName)
	if c == nil {
		if ln == nil {
			return false, nil
		}
		return true, nlx.LinkDel(ln)
	}
	parent, err := nlx.LinkByName(c.Parent)
	if err != nil {
		return false, fmt.Errorf("cme: management port %s: %w", c.Parent, err)
	}
	changed := false
	if mv, ok := ln.(*netlink.Macvlan); ln != nil && (!ok || mv.ParentIndex != parent.Attrs().Index || mv.HardwareAddr.String() != c.MAC.String()) {
		if err := nlx.LinkDel(ln); err != nil {
			return false, err
		}
		ln, changed = nil, true
	}
	if ln == nil {
		attrs := netlink.LinkAttrs{Name: CMEName, ParentIndex: parent.Attrs().Index, HardwareAddr: c.MAC}
		if err := nlx.LinkAdd(&netlink.Macvlan{LinkAttrs: attrs, Mode: netlink.MACVLAN_MODE_PRIVATE}); err != nil {
			return changed, fmt.Errorf("creating cme: %w", err)
		}
		changed = true
		if ln, err = nlx.LinkByName(CMEName); err != nil {
			return changed, err
		}
	}
	for path, v := range map[string]string{
		"/proc/sys/net/ipv4/conf/cme/arp_notify":   "1",
		"/proc/sys/net/ipv4/conf/cme/forwarding":   "0",
		"/proc/sys/net/ipv6/conf/cme/accept_dad":   "0",
		"/proc/sys/net/ipv6/conf/cme/ndisc_notify": "1",
		"/proc/sys/net/ipv6/conf/cme/accept_ra":    "0",
		"/proc/sys/net/ipv6/conf/cme/disable_ipv6": "0",
	} {
		c, err := writeSysctl(path, v)
		changed = changed || c
		if err != nil {
			return changed, err
		}
	}
	master := 0
	if c.VRF != "" {
		v, err := nlx.LinkByName(c.VRF)
		if err != nil {
			return changed, fmt.Errorf("cme: routing instance %s: %w", c.VRF, err)
		}
		master = v.Attrs().Index
	}
	if ln.Attrs().MasterIndex != master {
		if master == 0 {
			err = nlx.LinkSetNoMaster(ln)
		} else {
			v, _ := nlx.LinkByIndex(master)
			err = nlx.LinkSetMaster(ln, v)
		}
		if err != nil {
			return changed, fmt.Errorf("cme: routing instance %s: %w", c.VRF, err)
		}
		changed = true
	}
	c2, err := syncExactAddrs(ln, c.Addrs)
	changed = changed || c2
	if err != nil {
		return changed, fmt.Errorf("cme: %w", err)
	}
	up := ln.Attrs().Flags&net.FlagUp != 0
	switch {
	case c.Up && !up:
		err = nlx.LinkSetUp(ln)
	case !c.Up && up:
		err = nlx.LinkSetDown(ln)
	default:
		return changed, nil
	}
	return true, err
}

// syncExactAddrs gives a link exactly the addresses want (link-local
// addresses aside); IPv6 addresses skip duplicate address detection.
func syncExactAddrs(ln netlink.Link, want []netip.Prefix) (bool, error) {
	cur, err := nlx.AddrList(ln, netlink.FAMILY_ALL)
	if err != nil {
		return false, err
	}
	changed := false
	have := map[netip.Prefix]bool{}
	for _, a := range cur {
		if a.IP.IsLinkLocalUnicast() {
			continue
		}
		ip, _ := netip.AddrFromSlice(a.IP)
		ones, _ := a.Mask.Size()
		p := netip.PrefixFrom(ip.Unmap(), ones)
		if slices.Contains(want, p) {
			have[p] = true
			continue
		}
		if err := nlx.AddrDel(ln, &a); err != nil {
			return changed, err
		}
		changed = true
	}
	for _, p := range want {
		if have[p] {
			continue
		}
		ad, err := netlink.ParseAddr(p.String())
		if err != nil {
			return changed, err
		}
		if p.Addr().Is6() {
			ad.Flags |= unix.IFA_F_NODAD
		}
		if err := nlx.AddrAdd(ln, ad); err != nil && !errors.Is(err, unix.EEXIST) {
			return changed, fmt.Errorf("address %s: %w", p, err)
		}
		changed = true
	}
	return changed, nil
}

// syncBare removes every address from ports that must not have any
// (management ports, unconfigured ports; reference 1.4) and disables IPv6
// on them. unconfigured ports also lose their description.
func syncBare(ports []string, unconfigured map[string]bool) (bool, error) {
	changed := false
	var errs []error
	for _, n := range ports {
		ln, err := nlx.LinkByName(n)
		if err != nil {
			continue
		}
		c, err := writeSysctl("/proc/sys/net/ipv6/conf/"+n+"/disable_ipv6", "1")
		changed = changed || c
		if err != nil {
			errs = append(errs, err)
		}
		c, err = syncExactAddrs(ln, nil)
		changed = changed || c
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n, err))
		}
		flushNeighbors(ln) // not a change of configuration: not reported
		if unconfigured[n] && ln.Attrs().Alias != "" {
			changed = true
			if err := nlx.LinkSetAlias(ln, ""); err != nil {
				errs = append(errs, fmt.Errorf("%s: description: %w", n, err))
			}
		}
	}
	return changed, errors.Join(errs...)
}

// syncNoIP disables IPv6 on layer 2 devices (reference 1.5: no IP on
// switch ports).
func syncNoIP(links []string) (bool, error) {
	changed := false
	var errs []error
	for _, n := range links {
		c, err := writeSysctl("/proc/sys/net/ipv6/conf/"+n+"/disable_ipv6", "1")
		changed = changed || c
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return changed, errors.Join(errs...)
}

// flushNeighbors removes the neighbour entries of a link that has no
// address any more (they would stay in show arp until the kernel collects
// garbage).
func flushNeighbors(ln netlink.Link) bool {
	changed := false
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		ns, err := nlx.NeighList(ln.Attrs().Index, fam)
		if err != nil {
			continue
		}
		for _, n := range ns {
			if n.State&netlink.NUD_PERMANENT != 0 {
				continue
			}
			if nlx.NeighDel(&n) == nil {
				changed = true
			}
		}
	}
	return changed
}

// Carrier reports whether a kernel port has a link.
func Carrier(name string) bool {
	ln, err := nlx.LinkByName(name)
	return err == nil && ln.Attrs().RawFlags&unix.IFF_LOWER_UP != 0
}

// VTEPDevice holds the stack's VXLAN source address on every member
// (reference 5.7).
const VTEPDevice = "swvtep"

// syncVTEP keeps the VTEP device with exactly addr (invalid: no device).
func syncVTEP(addr netip.Addr) (bool, error) {
	ln, _ := nlx.LinkByName(VTEPDevice)
	if !addr.IsValid() {
		if ln == nil {
			return false, nil
		}
		return true, nlx.LinkDel(ln)
	}
	changed := false
	if ln == nil {
		if err := nlx.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: VTEPDevice}}); err != nil {
			return false, fmt.Errorf("%s: %w", VTEPDevice, err)
		}
		var err error
		if ln, err = nlx.LinkByName(VTEPDevice); err != nil {
			return true, err
		}
		changed = true
	}
	if c, err := writeSysctl("/proc/sys/net/ipv6/conf/"+VTEPDevice+"/disable_ipv6", "1"); err == nil {
		changed = changed || c
	}
	c, err := syncExactAddrs(ln, []netip.Prefix{netip.PrefixFrom(addr, addr.BitLen())})
	changed = changed || c
	if err != nil {
		return changed, err
	}
	if ln.Attrs().Flags&net.FlagUp == 0 {
		if err := nlx.LinkSetUp(ln); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}
