//go:build linux

package dataplane

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// vxlanTable keeps frames that arrived over a stack tunnel out of the VXLAN
// ports: the member where they entered the stack sends them to the remote
// VTEPs itself (reference 5.7).
const vxlanTable = "switchd_vxlan"

var (
	vxlanMu   sync.Mutex
	vxlanLast = "\x00"
)

// SyncVXLAN converges the remote VTEPs of each VXLAN port (the all-zero
// MAC entries that replicate BUM traffic) and the split horizon towards
// the stack tunnels. remotes is by VXLAN port.
func (k *Netlink) SyncVXLAN(remotes map[string][]netip.Addr) (bool, error) {
	changed := false
	var errs []error
	links, err := netlink.LinkList()
	if err != nil {
		return false, err
	}
	var ports []string
	for _, l := range links {
		name := l.Attrs().Name
		if VXLANVNI(name) == 0 {
			continue
		}
		ports = append(ports, name)
		want := map[netip.Addr]bool{}
		for _, a := range remotes[name] {
			want[a] = true
		}
		neighs, err := netlink.NeighList(l.Attrs().Index, unix.AF_BRIDGE)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		have := map[netip.Addr]bool{}
		for _, n := range neighs {
			if !bytes.Equal(n.HardwareAddr, make(net.HardwareAddr, 6)) || n.IP == nil {
				continue
			}
			a, _ := netip.AddrFromSlice(n.IP.To4())
			if want[a] {
				have[a] = true
				continue
			}
			nn := n
			if err := netlink.NeighDel(&nn); err != nil {
				errs = append(errs, fmt.Errorf("%s: remote VTEP %s: %w", name, a, err))
			} else {
				changed = true
			}
		}
		for _, a := range slices.SortedFunc(maps.Keys(want), func(x, y netip.Addr) int { return x.Compare(y) }) {
			if have[a] {
				continue
			}
			n := &netlink.Neigh{LinkIndex: l.Attrs().Index, Family: unix.AF_BRIDGE, State: netlink.NUD_PERMANENT | netlink.NUD_NOARP,
				Flags: netlink.NTF_SELF, HardwareAddr: make(net.HardwareAddr, 6), IP: a.AsSlice()}
			if err := netlink.NeighAppend(n); err != nil {
				errs = append(errs, fmt.Errorf("%s: remote VTEP %s: %w", name, a, err))
			} else {
				changed = true
			}
		}
	}
	// The split horizon (one table, replaced atomically).
	var b strings.Builder
	fmt.Fprintf(&b, "table bridge %s\ndelete table bridge %s\n", vxlanTable, vxlanTable)
	if len(ports) > 0 {
		fmt.Fprintf(&b, "table bridge %s {\n\tchain forward {\n\t\ttype filter hook forward priority filter; policy accept;\n"+
			"\t\tiifname \"swvc*\" oifname \"swvx*\" counter drop\n\t\tiifname \"swvx*\" oifname \"swvx*\" counter drop\n\t}\n}\n", vxlanTable)
	}
	text := b.String()
	vxlanMu.Lock()
	defer vxlanMu.Unlock()
	if text != vxlanLast {
		nft, err := exec.LookPath("nft")
		if err != nil {
			if len(ports) > 0 {
				errs = append(errs, errors.New("nftables (the nft program) is required for VXLAN; install the nftables package"))
			}
		} else {
			cmd := exec.Command(nft, "-f", "-")
			cmd.Stdin = strings.NewReader(text)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Run(); err != nil {
				errs = append(errs, fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(out.String())))
			} else {
				vxlanLast, changed = text, true
			}
		}
	}
	return changed, errors.Join(errs...)
}
