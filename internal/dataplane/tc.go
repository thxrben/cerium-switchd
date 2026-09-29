//go:build linux

package dataplane

import (
	"errors"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// tc filter priorities owned by switchd on the ingress hook of a port.
// Lower numbers run first.
const (
	prioPassPrioTagged = 0x7a00 // flower vlan_id 0 -> pass
	prioDropTagged     = 0x7a01 // matchall 802.1Q -> drop
)

const ethP8021Q = 0x8100

func ensureClsact(l netlink.Link) error {
	q := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: l.Attrs().Index,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	if err := netlink.QdiscAdd(q); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	return nil
}

// addPassPrioTagged adds "flower vlan_id 0 action pass". The netlink library
// cannot encode vlan_id 0 (it treats 0 as unset), so the request is built
// here.
func addPassPrioTagged(l netlink.Link) error {
	req := nl.NewNetlinkRequest(unix.RTM_NEWTFILTER, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK)
	req.AddData(&nl.TcMsg{
		Family:  nl.FAMILY_ALL,
		Ifindex: int32(l.Attrs().Index),
		Handle:  1,
		Parent:  netlink.HANDLE_MIN_INGRESS,
		Info:    netlink.MakeHandle(prioPassPrioTagged, nl.Swap16(ethP8021Q)),
	})
	req.AddData(nl.NewRtAttr(nl.TCA_KIND, nl.ZeroTerminated("flower")))
	opts := nl.NewRtAttr(nl.TCA_OPTIONS, nil)
	// The kernel only parses VLAN keys when the ethertype key says 802.1Q.
	opts.AddRtAttr(nl.TCA_FLOWER_KEY_ETH_TYPE, nl.Uint16Attr(nl.Swap16(ethP8021Q)))
	opts.AddRtAttr(nl.TCA_FLOWER_KEY_VLAN_ID, nl.Uint16Attr(0))
	acts := opts.AddRtAttr(nl.TCA_FLOWER_ACT, nil)
	a := acts.AddRtAttr(1, nil)
	a.AddRtAttr(nl.TCA_ACT_KIND, nl.ZeroTerminated("gact"))
	ao := a.AddRtAttr(nl.TCA_ACT_OPTIONS, nil)
	gen := nl.TcGen{Action: int32(netlink.TC_ACT_OK)}
	ao.AddRtAttr(nl.TCA_GACT_PARMS, gen.Serialize())
	req.AddData(opts)
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

func addDropTagged(l netlink.Link) error {
	return netlink.FilterAdd(&netlink.MatchAll{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: l.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Priority:  prioDropTagged,
			Protocol:  ethP8021Q,
			Handle:    1,
		},
		Actions: []netlink.Action{&netlink.GenericAction{ActionAttrs: netlink.ActionAttrs{Action: netlink.TC_ACT_SHOT}}},
	})
}

// ingressPrios returns the priorities of switchd's ingress filters on l.
func ingressPrios(l netlink.Link) map[uint16]bool {
	out := map[uint16]bool{}
	fs, err := netlink.FilterList(l, netlink.HANDLE_MIN_INGRESS)
	if err != nil {
		return out
	}
	for _, f := range fs {
		if p := f.Attrs().Priority; p == prioPassPrioTagged || p == prioDropTagged {
			out[p] = true
		}
	}
	return out
}

func delFilter(l netlink.Link, prio uint16, kind string) error {
	err := netlink.FilterDel(&netlink.GenericFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: l.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Priority:  prio,
			Protocol:  ethP8021Q,
		},
		FilterType: kind,
	})
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// setDropTagged installs or removes the access-port filter. On: the pass
// rule for priority-tagged frames goes in before the drop rule, so VID 0
// frames are never dropped. Off: the drop rule goes first.
func setDropTagged(l netlink.Link, on bool) error {
	have := ingressPrios(l)
	if on {
		if err := ensureClsact(l); err != nil {
			return err
		}
		if !have[prioPassPrioTagged] {
			if err := addPassPrioTagged(l); err != nil {
				return err
			}
		}
		if !have[prioDropTagged] {
			return addDropTagged(l)
		}
		return nil
	}
	if have[prioDropTagged] {
		if err := delFilter(l, prioDropTagged, "matchall"); err != nil {
			return err
		}
	}
	if have[prioPassPrioTagged] {
		return delFilter(l, prioPassPrioTagged, "flower")
	}
	return nil
}
