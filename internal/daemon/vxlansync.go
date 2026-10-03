package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/thxrben/cerium-switchd/internal/dataplane"
)

// vxlanSync distributes the remote MACs the members learn on their VXLAN
// ports (reference 5.7): a frame from a remote VTEP arrives at one member,
// which learns where its source is; every other member installs the
// address on its own VXLAN port with the same VTEP, so its hosts reach the
// remote host directly (frames from a stack tunnel never enter VXLAN).
type vxlanSync struct {
	member int
	stack  *stackCtl
	log    *slog.Logger

	mu        sync.Mutex
	from      map[int]vxlanSet // other members' learned addresses
	seen      map[int]time.Time
	installed map[vxlanMAC]bool
	lastSent  string
	sentAt    time.Time
}

// vxlanMAC is a remote MAC behind a VTEP in a VNI.
type vxlanMAC struct {
	VNI  int        `json:"vni"`
	VLAN int        `json:"vlan"`
	MAC  string     `json:"mac"`
	VTEP netip.Addr `json:"vtep"`
}

type vxlanSet []vxlanMAC

// vxlanForget: a member's addresses are dropped when nothing came from it
// for this long (it is gone).
const vxlanForget = 30 * time.Second

func newVXLANSync(member int, stack *stackCtl, log *slog.Logger) *vxlanSync {
	v := &vxlanSync{member: member, stack: stack, log: log, from: map[int]vxlanSet{}, seen: map[int]time.Time{}, installed: map[vxlanMAC]bool{}}
	stack.node.Handle("vxlan-macs", func(from int, raw json.RawMessage) (any, error) {
		var set vxlanSet
		if err := json.Unmarshal(raw, &set); err != nil {
			return nil, err
		}
		v.mu.Lock()
		v.from[from], v.seen[from] = set, time.Now()
		v.mu.Unlock()
		return nil, nil
	})
	return v
}

func (v *vxlanSync) run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			v.step(time.Now())
		}
	}
}

func (v *vxlanSync) step(now time.Time) {
	local := learnedVXLAN()
	raw, _ := json.Marshal(local)
	v.mu.Lock()
	send := string(raw) != v.lastSent || now.Sub(v.sentAt) >= 10*time.Second
	if send {
		v.lastSent, v.sentAt = string(raw), now
	}
	for id, at := range v.seen {
		if now.Sub(at) > vxlanForget {
			delete(v.from, id)
			delete(v.seen, id)
		}
	}
	want := map[vxlanMAC]bool{}
	for _, set := range v.from {
		for _, m := range set {
			want[m] = true
		}
	}
	v.mu.Unlock()
	if send && (len(local) > 0 || v.lastSentNonEmpty()) {
		for _, id := range v.stack.node.Mesh.Reachable() {
			if id == v.member {
				continue
			}
			go func(id int) {
				if _, err := v.stack.node.Call(id, "vxlan-macs", local, 2*time.Second); err != nil {
					v.log.Debug("vxlan: addresses to a member", "member", id, "err", err)
				}
			}(id)
		}
	}
	v.apply(want, local)
}

// lastSentNonEmpty: the members must hear once that the list is empty now.
func (v *vxlanSync) lastSentNonEmpty() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.lastSent != "null" && v.lastSent != "[]"
}

// apply installs what other members learned (not what this member learned
// itself) and removes what they no longer report.
func (v *vxlanSync) apply(want map[vxlanMAC]bool, local vxlanSet) {
	mine := map[[2]any]bool{}
	for _, m := range local {
		mine[[2]any{m.VNI, m.MAC}] = true
	}
	v.mu.Lock()
	installed := maps.Clone(v.installed)
	v.mu.Unlock()
	for m := range installed {
		if !want[m] {
			if err := vxlanDel(m); err != nil {
				v.log.Debug("vxlan: remote address", "mac", m.MAC, "err", err) // e.g. the port is gone
			}
			delete(installed, m)
		}
	}
	for _, m := range slices.SortedFunc(maps.Keys(want), func(a, b vxlanMAC) int { return cmpMAC(a, b) }) {
		if installed[m] || mine[[2]any{m.VNI, m.MAC}] {
			continue
		}
		if err := vxlanSet1(m); err != nil {
			v.log.Debug("vxlan: remote address", "mac", m.MAC, "vtep", m.VTEP, "err", err)
			continue
		}
		installed[m] = true
	}
	v.mu.Lock()
	v.installed = installed
	v.mu.Unlock()
}

func cmpMAC(a, b vxlanMAC) int {
	if a.VNI != b.VNI {
		return a.VNI - b.VNI
	}
	if a.MAC < b.MAC {
		return -1
	}
	if a.MAC > b.MAC {
		return 1
	}
	return 0
}

// learnedVXLAN lists the remote MACs this member's bridge learned on its
// VXLAN ports, with the VTEP each is behind (from the VXLAN port's own
// table).
func learnedVXLAN() vxlanSet {
	links, err := netlink.LinkList()
	if err != nil {
		return nil
	}
	var out vxlanSet
	for _, l := range links {
		vni := dataplane.VXLANVNI(l.Attrs().Name)
		if vni == 0 {
			continue
		}
		neighs, err := netlink.NeighList(l.Attrs().Index, unix.AF_BRIDGE)
		if err != nil {
			continue
		}
		vtep := map[string]netip.Addr{}
		for _, n := range neighs {
			if n.MasterIndex == 0 && n.IP != nil && len(n.HardwareAddr) == 6 {
				a, _ := netip.AddrFromSlice(n.IP.To4())
				vtep[macOf(&n)] = a
			}
		}
		for _, n := range neighs {
			// Learned by the bridge here: not local, static or installed.
			if n.MasterIndex == 0 || n.Vlan == 0 || n.State&(unix.NUD_PERMANENT|unix.NUD_NOARP) != 0 || n.Flags&netlink.NTF_EXT_LEARNED != 0 {
				continue
			}
			a, ok := vtep[macOf(&n)]
			if !ok || !a.IsValid() {
				continue
			}
			out = append(out, vxlanMAC{VNI: vni, VLAN: n.Vlan, MAC: macOf(&n), VTEP: a})
		}
	}
	slices.SortFunc(out, cmpMAC)
	return out
}

// vxlanSet1 installs a remote MAC on this member's VXLAN port: in the
// bridge (externally learned: it does not age, and the bridge replaces it
// if it learns the address itself) and in the port's own table (the VTEP).
func vxlanSet1(m vxlanMAC) error {
	l, err := netlink.LinkByName(dataplane.VXLANName(m.VNI))
	if err != nil {
		return err
	}
	mac, err := net.ParseMAC(m.MAC)
	if err != nil {
		return err
	}
	if err := netlink.NeighSet(&netlink.Neigh{LinkIndex: l.Attrs().Index, Family: unix.AF_BRIDGE, State: netlink.NUD_NOARP,
		Flags: netlink.NTF_SELF, HardwareAddr: mac, IP: m.VTEP.AsSlice()}); err != nil {
		return err
	}
	return netlink.NeighSet(&netlink.Neigh{LinkIndex: l.Attrs().Index, Family: unix.AF_BRIDGE, State: netlink.NUD_REACHABLE,
		Flags: netlink.NTF_MASTER | netlink.NTF_EXT_LEARNED, HardwareAddr: mac, Vlan: m.VLAN})
}

func vxlanDel(m vxlanMAC) error {
	l, err := netlink.LinkByName(dataplane.VXLANName(m.VNI))
	if err != nil {
		return err
	}
	mac, err := net.ParseMAC(m.MAC)
	if err != nil {
		return err
	}
	e1 := netlink.NeighDel(&netlink.Neigh{LinkIndex: l.Attrs().Index, Family: unix.AF_BRIDGE, Flags: netlink.NTF_MASTER,
		HardwareAddr: mac, Vlan: m.VLAN})
	e2 := netlink.NeighDel(&netlink.Neigh{LinkIndex: l.Attrs().Index, Family: unix.AF_BRIDGE, Flags: netlink.NTF_SELF,
		HardwareAddr: mac, IP: m.VTEP.AsSlice()})
	if e1 != nil {
		return e1
	}
	return e2
}

func macOf(n *netlink.Neigh) string { return strings.ToLower(n.HardwareAddr.String()) }
