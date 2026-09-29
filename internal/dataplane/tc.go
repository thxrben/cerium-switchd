//go:build linux

package dataplane

import (
	"errors"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// switchd's ingress filters on a port (clsact ingress hook).
//
//	chain 0  0x7900  flower dst 01:80:c2:00:00:0x      -> goto chain 1  (link-local: never policed)
//	chain 0  0x7901  flower dst ff:ff:ff:ff:ff:ff      -> police bcast pps (excess: drop), goto chain 1
//	chain 0  0x7902  flower dst multicast              -> police mcast pps (excess: drop), goto chain 1
//	chain 0  0x79ff  matchall                          -> goto chain 1  (only with drop-tagged)
//	chain 1  0x7a00  flower 802.1Q vlan_id 0           -> pass          (priority-tagged frames)
//	chain 1  0x7a01  matchall 802.1Q                   -> drop          (access ports)
//
// The policer rate is also stored in the flower classid (unused on ingress)
// so that it can be read back.
const (
	prioLinkLocal      = 0x7900
	prioStormBroadcast = 0x7901
	prioStormMulticast = 0x7902
	prioToTagged       = 0x79ff
	prioPassPrioTagged = 0x7a00
	prioDropTagged     = 0x7a01

	chainStorm  = 0
	chainTagged = 1

	ethP8021Q = 0x8100
	ethPAll   = 0x0003

	tcActGotoChain = 0x20000000

	tcaPolicePktRate64  = 10
	tcaPolicePktBurst64 = 11
)

var (
	macLinkLocal     = []byte{0x01, 0x80, 0xc2, 0x00, 0x00, 0x00}
	macLinkLocalMask = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xf0}
	macBroadcast     = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	macMulticast     = []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00}
	macGroupBit      = []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00}
)

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

// tcAction encodes one action into its TCA_ACT_* container.
type tcAction func(a *nl.RtAttr)

func gact(action int32) tcAction {
	return func(a *nl.RtAttr) {
		a.AddRtAttr(nl.TCA_ACT_KIND, nl.ZeroTerminated("gact"))
		o := a.AddRtAttr(nl.TCA_ACT_OPTIONS, nil)
		gen := nl.TcGen{Action: action}
		o.AddRtAttr(nl.TCA_GACT_PARMS, gen.Serialize())
	}
}

// policePPS limits to pps packets per second: excess is dropped, conforming
// packets continue with the next action.
func policePPS(pps int) tcAction {
	return func(a *nl.RtAttr) {
		a.AddRtAttr(nl.TCA_ACT_KIND, nl.ZeroTerminated("police"))
		o := a.AddRtAttr(nl.TCA_ACT_OPTIONS, nil)
		p := nl.TcPolice{Action: int32(netlink.TC_ACT_SHOT)}
		o.AddRtAttr(nl.TCA_POLICE_TBF, p.Serialize())
		o.AddRtAttr(nl.TCA_POLICE_RESULT, nl.Uint32Attr(uint32(netlink.TC_ACT_PIPE)))
		o.AddRtAttr(tcaPolicePktRate64, nl.Uint64Attr(uint64(pps)))
		o.AddRtAttr(tcaPolicePktBurst64, nl.Uint64Attr(burstTicks(pps)))
	}
}

// burstTicks returns the policer's packet burst for pps: max(pps/10, 16)
// packets. The kernel expects it as the time to send that many packets at
// the rate, in psched ticks of 64 ns (act_police: PSCHED_TICKS2NS).
func burstTicks(pps int) uint64 {
	packets := max(pps/10, 16)
	ns := float64(packets) * 1e9 / float64(pps)
	return uint64(ns / 64)
}

// tcRule is one ingress filter.
type tcRule struct {
	chain   uint32
	prio    uint16
	proto   uint16
	kind    string // flower or matchall
	classid uint32
	keys    func(o *nl.RtAttr)
	actions []tcAction
}

// install adds the rule, or atomically replaces an existing one.
func (r tcRule) install(l netlink.Link) error {
	req := nl.NewNetlinkRequest(unix.RTM_NEWTFILTER, unix.NLM_F_CREATE|unix.NLM_F_REPLACE|unix.NLM_F_ACK)
	req.AddData(&nl.TcMsg{
		Family:  nl.FAMILY_ALL,
		Ifindex: int32(l.Attrs().Index),
		Handle:  1,
		Parent:  netlink.HANDLE_MIN_INGRESS,
		Info:    netlink.MakeHandle(r.prio, nl.Swap16(r.proto)),
	})
	req.AddData(nl.NewRtAttr(nl.TCA_KIND, nl.ZeroTerminated(r.kind)))
	req.AddData(nl.NewRtAttr(nl.TCA_CHAIN, nl.Uint32Attr(r.chain)))
	opts := nl.NewRtAttr(nl.TCA_OPTIONS, nil)
	actType := nl.TCA_MATCHALL_ACT
	if r.kind == "flower" {
		actType = nl.TCA_FLOWER_ACT
		if r.classid != 0 {
			opts.AddRtAttr(nl.TCA_FLOWER_CLASSID, nl.Uint32Attr(r.classid))
		}
		if r.keys != nil {
			r.keys(opts)
		}
	}
	acts := opts.AddRtAttr(actType, nil)
	for i, enc := range r.actions {
		enc(acts.AddRtAttr(i+1, nil))
	}
	req.AddData(opts)
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

// removeRule deletes the filter at chain/prio (any protocol and kind).
func removeRule(l netlink.Link, chain uint32, prio uint16) error {
	req := nl.NewNetlinkRequest(unix.RTM_DELTFILTER, unix.NLM_F_ACK)
	req.AddData(&nl.TcMsg{
		Family:  nl.FAMILY_ALL,
		Ifindex: int32(l.Attrs().Index),
		Parent:  netlink.HANDLE_MIN_INGRESS,
		Info:    netlink.MakeHandle(prio, 0),
	})
	req.AddData(nl.NewRtAttr(nl.TCA_CHAIN, nl.Uint32Attr(chain)))
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

func dstMAC(mac, mask []byte) func(o *nl.RtAttr) {
	return func(o *nl.RtAttr) {
		o.AddRtAttr(nl.TCA_FLOWER_KEY_ETH_DST, mac)
		o.AddRtAttr(nl.TCA_FLOWER_KEY_ETH_DST_MASK, mask)
	}
}

var (
	ruleLinkLocal = tcRule{chain: chainStorm, prio: prioLinkLocal, proto: ethPAll, kind: "flower",
		keys: dstMAC(macLinkLocal, macLinkLocalMask), actions: []tcAction{gact(tcActGotoChain | chainTagged)}}
	ruleToTagged = tcRule{chain: chainStorm, prio: prioToTagged, proto: ethPAll, kind: "matchall",
		actions: []tcAction{gact(tcActGotoChain | chainTagged)}}
	rulePassPrioTagged = tcRule{chain: chainTagged, prio: prioPassPrioTagged, proto: ethP8021Q, kind: "flower",
		keys: func(o *nl.RtAttr) {
			// The kernel only parses VLAN keys when the ethertype key says 802.1Q.
			o.AddRtAttr(nl.TCA_FLOWER_KEY_ETH_TYPE, nl.Uint16Attr(nl.Swap16(ethP8021Q)))
			o.AddRtAttr(nl.TCA_FLOWER_KEY_VLAN_ID, nl.Uint16Attr(0))
		},
		actions: []tcAction{gact(int32(netlink.TC_ACT_OK))}}
	ruleDropTagged = tcRule{chain: chainTagged, prio: prioDropTagged, proto: ethP8021Q, kind: "matchall",
		actions: []tcAction{gact(int32(netlink.TC_ACT_SHOT))}}
)

func stormRule(prio uint16, pps int) tcRule {
	mac, mask := macBroadcast, macBroadcast
	if prio == prioStormMulticast {
		mac, mask = macMulticast, macGroupBit
	}
	return tcRule{chain: chainStorm, prio: prio, proto: ethPAll, kind: "flower", classid: uint32(pps),
		keys: dstMAC(mac, mask), actions: []tcAction{policePPS(pps), gact(tcActGotoChain | chainTagged)}}
}

// tcState is what switchd's ingress filters on a port currently do.
type tcState struct {
	rules          map[[2]uint32]bool // (chain, prio) present
	stormBroadcast int
	stormMulticast int
}

func (t tcState) has(chain uint32, prio uint16) bool { return t.rules[[2]uint32{chain, uint32(prio)}] }

func (t tcState) dropTagged() bool {
	return t.has(chainStorm, prioToTagged) && t.has(chainTagged, prioPassPrioTagged) && t.has(chainTagged, prioDropTagged)
}

// validRules are the (chain, prio) positions switchd uses.
var validRules = map[[2]uint32]bool{
	{chainStorm, prioLinkLocal}: true, {chainStorm, prioStormBroadcast}: true, {chainStorm, prioStormMulticast}: true,
	{chainStorm, prioToTagged}: true, {chainTagged, prioPassPrioTagged}: true, {chainTagged, prioDropTagged}: true,
}

// readTC returns switchd's ingress rules on l. Rules in switchd's priority
// range at a position it never uses (e.g. from an older layout) are
// removed: they can only differ from what the configuration asks for.
func readTC(l netlink.Link) tcState {
	st := tcState{rules: map[[2]uint32]bool{}}
	fs, err := netlink.FilterList(l, netlink.HANDLE_MIN_INGRESS)
	if err != nil {
		return st
	}
	for _, f := range fs {
		a := f.Attrs()
		if a.Priority < prioLinkLocal || a.Priority > prioDropTagged {
			continue
		}
		chain := uint32(0)
		if a.Chain != nil {
			chain = *a.Chain
		}
		if !validRules[[2]uint32{chain, uint32(a.Priority)}] {
			_ = removeRule(l, chain, a.Priority)
			continue
		}
		st.rules[[2]uint32{chain, uint32(a.Priority)}] = true
		if fl, ok := f.(*netlink.Flower); ok && chain == chainStorm {
			switch a.Priority {
			case prioStormBroadcast:
				st.stormBroadcast = int(fl.ClassId)
			case prioStormMulticast:
				st.stormMulticast = int(fl.ClassId)
			}
		}
	}
	return st
}

// setDropTagged enables or disables the access-port filter. On: the chain
// 1 rules exist before the jump into chain 1 activates them, and the pass
// rule for priority-tagged frames precedes the drop rule. Off: the drop
// rule goes first.
func setDropTagged(l netlink.Link, on bool) error {
	st := readTC(l)
	if on {
		if err := ensureClsact(l); err != nil {
			return err
		}
		for _, r := range []tcRule{rulePassPrioTagged, ruleDropTagged, ruleToTagged} {
			if !st.has(r.chain, r.prio) {
				if err := r.install(l); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, r := range []tcRule{ruleDropTagged, rulePassPrioTagged, ruleToTagged} {
		if st.has(r.chain, r.prio) {
			if err := removeRule(l, r.chain, r.prio); err != nil {
				return err
			}
		}
	}
	return nil
}

// setStorm sets (pps > 0) or removes the broadcast or multicast policer.
// The link-local bypass is installed before any policer and removed after
// the last one.
func setStorm(l netlink.Link, prio uint16, pps int) error {
	st := readTC(l)
	if pps > 0 {
		if err := ensureClsact(l); err != nil {
			return err
		}
		if !st.has(chainStorm, prioLinkLocal) {
			if err := ruleLinkLocal.install(l); err != nil {
				return err
			}
		}
		return stormRule(prio, pps).install(l)
	}
	if st.has(chainStorm, prio) {
		if err := removeRule(l, chainStorm, prio); err != nil {
			return err
		}
	}
	other := uint16(prioStormMulticast)
	if prio == prioStormMulticast {
		other = prioStormBroadcast
	}
	if !st.has(chainStorm, other) && st.has(chainStorm, prioLinkLocal) {
		return removeRule(l, chainStorm, prioLinkLocal)
	}
	return nil
}
