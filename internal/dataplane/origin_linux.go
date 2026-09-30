//go:build linux

package dataplane

import (
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Policy rules for what the switch itself originates (reference 1.5): with a
// management instance, connections that programs on the switch open without
// choosing a source address (DNS lookups, NTP, package updates, ssh
// clients, …) are routed by the table of management and nowhere else; when it
// has no route for them they fail (unreachable) instead of leaving through
// a data interface or the operating system's own NIC.
//
// The rules match the unspecified source address (the route lookup of a
// connect() or sendto() before the kernel picks a source), so replies of
// servers and everything bound to a device or an address keep their own
// routing: the OS sshd answers on the NIC it was reached on, and a data
// irb answers ping. They sit after the l3mdev rule (1000), so traffic bound
// to a VRF (syslog, the CLI's sshd in management, the stack tunnels) is
// routed by its VRF first.
const (
	originPrioTable = 1100
	originPrioBlock = 1101
)

// syncOriginRules installs the rules for the routing table of the
// management instance (0: none, the rules are removed). addrs are the
// addresses of its interfaces: a connection that already has such a source
// address (the kernel routes every packet of it again) keeps using the
// management table, and cannot leave elsewhere.
func syncOriginRules(table int, addrs []netip.Addr) (bool, error) {
	changed := false
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		// The sources the rules match: unspecified, and the addresses of
		// the management interfaces.
		var srcs []*net.IPNet
		if table != 0 {
			zero, bits := net.IPv4zero, 32
			if fam == netlink.FAMILY_V6 {
				zero, bits = net.IPv6zero, 128
			}
			srcs = append(srcs, &net.IPNet{IP: zero, Mask: net.CIDRMask(bits, bits)})
			for _, a := range addrs {
				if (a.Is4() || a.Is4In6()) == (fam == netlink.FAMILY_V4) && !a.IsLinkLocalUnicast() {
					srcs = append(srcs, &net.IPNet{IP: a.Unmap().AsSlice(), Mask: net.CIDRMask(a.BitLen(), a.BitLen())})
				}
			}
		}
		type key struct {
			prio int
			src  string
		}
		want := map[key]netlink.Rule{}
		for _, src := range srcs {
			to := *netlink.NewRule()
			to.Family, to.Priority, to.Src, to.Table = fam, originPrioTable, src, table
			block := *netlink.NewRule()
			block.Family, block.Priority, block.Src, block.Table = fam, originPrioBlock, src, 0
			block.Type = unix.FR_ACT_UNREACHABLE
			want[key{originPrioTable, src.String()}] = to
			want[key{originPrioBlock, src.String()}] = block
		}
		rules, err := netlink.RuleList(fam)
		if err != nil {
			fail(err)
			continue
		}
		have := map[key]bool{}
		for _, r := range rules {
			if r.Priority != originPrioTable && r.Priority != originPrioBlock {
				continue
			}
			k := key{r.Priority, ""}
			if r.Src != nil {
				k.src = r.Src.String()
			}
			w, ok := want[k]
			same := false
			if r.Priority == originPrioBlock {
				same = r.Type == unix.FR_ACT_UNREACHABLE
			} else {
				same = r.Type != unix.FR_ACT_UNREACHABLE && r.Table == w.Table
			}
			if ok && same && !have[k] {
				have[k] = true
				continue
			}
			rr := r
			if err := netlink.RuleDel(&rr); err != nil {
				fail(err)
			}
			changed = true
		}
		// Table rules first, so there is never a moment where a block
		// rule alone is in effect.
		for _, prio := range []int{originPrioTable, originPrioBlock} {
			for _, src := range srcs {
				k := key{prio, src.String()}
				if have[k] {
					continue
				}
				w := want[k]
				if err := netlink.RuleAdd(&w); err != nil {
					fail(err)
					continue
				}
				changed = true
			}
		}
	}
	return changed, firstErr
}
