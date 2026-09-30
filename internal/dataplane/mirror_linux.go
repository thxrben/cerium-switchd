//go:build linux

package dataplane

import (
	"fmt"
	"maps"
	"slices"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

// Mirror filters (before the storm filters of chain 0, so frames are
// copied as received; classification continues after the copy):
//
//	ingress 0x7800        matchall             -> mirror (every frame)
//	ingress 0x7801        flower num_vlans 0   -> mirror (untagged frames)
//	ingress 0x6000+vid    flower vlan_id vid   -> mirror (tagged frames of a VLAN)
//	egress  0x7800        matchall             -> mirror
const (
	prioMirrorAll      = 0x7800
	prioMirrorUntagged = 0x7801
	prioMirrorVLAN     = 0x6000

	tcaFlowerKeyNumOfVLANs = 102
)

func isMirrorPrio(egress bool, prio uint16) bool {
	if egress {
		return prio == prioMirrorAll
	}
	return prio == prioMirrorAll || prio == prioMirrorUntagged || (prio >= prioMirrorVLAN && prio <= prioMirrorVLAN+4094)
}

func mirred(ifindex int, last bool) tcAction {
	return func(a *nl.RtAttr) {
		a.AddRtAttr(nl.TCA_ACT_KIND, nl.ZeroTerminated("mirred"))
		o := a.AddRtAttr(nl.TCA_ACT_OPTIONS, nil)
		ctl := int32(netlink.TC_ACT_PIPE)
		if last {
			ctl = int32(netlink.TC_ACT_UNSPEC) // continue with the next filter
		}
		m := nl.TcMirred{TcGen: nl.TcGen{Action: ctl}, Eaction: int32(netlink.TCA_EGRESS_MIRROR), Ifindex: uint32(ifindex)}
		o.AddRtAttr(nl.TCA_MIRRED_PARMS, m.Serialize())
	}
}

type mirrorRule struct {
	egress bool
	prio   uint16
	vid    int // tagged: the VLAN; -1 untagged; 0 all
	outs   []int
}

func (r mirrorRule) tc() tcRule {
	t := tcRule{chain: chainStorm, prio: r.prio, proto: ethPAll, kind: "matchall", egress: r.egress}
	switch {
	case r.vid > 0:
		t.kind, t.proto = "flower", ethP8021Q
		vid := uint16(r.vid)
		t.keys = func(o *nl.RtAttr) {
			o.AddRtAttr(nl.TCA_FLOWER_KEY_ETH_TYPE, nl.Uint16Attr(nl.Swap16(ethP8021Q)))
			o.AddRtAttr(nl.TCA_FLOWER_KEY_VLAN_ID, nl.Uint16Attr(vid))
		}
	case r.vid < 0:
		t.kind = "flower"
		t.keys = func(o *nl.RtAttr) { o.AddRtAttr(tcaFlowerKeyNumOfVLANs, []byte{0}) }
	}
	for i, idx := range r.outs {
		t.actions = append(t.actions, mirred(idx, i == len(r.outs)-1))
	}
	return t
}

func mirrorOuts(f netlink.Filter) []int {
	var acts []netlink.Action
	switch x := f.(type) {
	case *netlink.MatchAll:
		acts = x.Actions
	case *netlink.Flower:
		acts = x.Actions
	}
	var out []int
	for _, a := range acts {
		if m, ok := a.(*netlink.MirredAction); ok {
			out = append(out, m.Ifindex)
		}
	}
	return out
}

// SyncMirrors installs the mirror filters of want (by device) and removes
// switchd's mirror filters elsewhere; unchanged filters are left alone.
func (k *Netlink) SyncMirrors(want map[string]*MirrorPort) (bool, error) {
	changed := false
	var errs []error
	links, err := netlink.LinkList()
	if err != nil {
		return false, err
	}
	for _, l := range links {
		name := l.Attrs().Name
		p := want[name]
		var rules []mirrorRule
		if p != nil {
			idx := func(devs []string) []int {
				var out []int
				for _, d := range devs {
					if o, err := netlink.LinkByName(d); err == nil {
						out = append(out, o.Attrs().Index)
					}
				}
				return out
			}
			if o := idx(p.Ingress); len(o) > 0 {
				rules = append(rules, mirrorRule{prio: prioMirrorAll, outs: o})
			}
			if o := idx(p.Untagged); len(o) > 0 {
				rules = append(rules, mirrorRule{prio: prioMirrorUntagged, vid: -1, outs: o})
			}
			for _, v := range slices.Sorted(maps.Keys(p.Tagged)) {
				if o := idx(p.Tagged[v]); len(o) > 0 {
					rules = append(rules, mirrorRule{prio: prioMirrorVLAN + uint16(v), vid: v, outs: o})
				}
			}
			if o := idx(p.Egress); len(o) > 0 {
				rules = append(rules, mirrorRule{egress: true, prio: prioMirrorAll, outs: o})
			}
		}
		// What is there now.
		have := map[[2]int][]int{} // (egress, prio) -> outs
		for _, eg := range []bool{false, true} {
			fs, err := netlink.FilterList(l, hook(eg))
			if err != nil {
				continue // no clsact
			}
			for _, f := range fs {
				if a := f.Attrs(); a.Chain != nil && *a.Chain == chainStorm && isMirrorPrio(eg, a.Priority) {
					e := 0
					if eg {
						e = 1
					}
					have[[2]int{e, int(a.Priority)}] = mirrorOuts(f)
				}
			}
		}
		if len(rules) == 0 && len(have) == 0 {
			continue
		}
		if len(rules) > 0 {
			if err := ensureClsact(l); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
		}
		keep := map[[2]int]bool{}
		for _, r := range rules {
			e := 0
			if r.egress {
				e = 1
			}
			key := [2]int{e, int(r.prio)}
			keep[key] = true
			if cur, ok := have[key]; ok && slices.Equal(cur, r.outs) {
				continue
			}
			if err := r.tc().install(l); err != nil {
				errs = append(errs, fmt.Errorf("%s: mirror filter %#x: %w", name, r.prio, err))
				continue
			}
			changed = true
		}
		for key := range have {
			if keep[key] {
				continue
			}
			if err := removeHookRule(l, key[0] == 1, chainStorm, uint16(key[1])); err != nil {
				errs = append(errs, err)
				continue
			}
			changed = true
		}
	}
	if len(errs) > 0 {
		return changed, fmt.Errorf("port mirroring: %v", errs)
	}
	return changed, nil
}
