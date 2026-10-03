//go:build linux

package dataplane

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/thxrben/cerium-switchd/pkg/nlx"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

// The protocol redirection to the master (punt.go) on a port's ingress:
//
//	chain 0      0x7850  flower dst <gateway MAC>                -> goto chain 0x100
//	chain 0x100  1..n    flower 802.1Q vlan_id <irb VLAN>        -> goto chain 0x101
//	chain 0x100  n+1     matchall 802.1Q                         -> goto chain 1 (as before)
//	chain 0x100  n+2     matchall (untagged, port VLAN with irb) -> goto chain 0x102
//	chain 0x101  1..10   flower 802.1Q, protocol                 -> redirect to the tunnel
//	chain 0x102  1..10   flower untagged, protocol               -> push the port VLAN, redirect
//	chain 0x101/0x102 last: matchall                             -> goto chain 1
//
// After mirroring (it sees the frames too) and before storm control (only
// unicast to the switch gets here, which storm control never limits).
// Chain 1 (drop-tagged) is where the frames would have gone anyway.
const (
	prioPunt       = 0x7850
	chainPunt      = 0x100
	chainPuntTag   = 0x101
	chainPuntUntag = 0x102

	ethPIP   = 0x0800
	ethPIPv6 = 0x86dd

	tcVlanActPush  = 2
	tcEgressRedir  = 1
	tcActStolen    = 4
	tcActGoto1     = tcActGotoChain | chainTagged
	tcaVlanPushPro = nl.TCA_VLAN_PUSH_VLAN_PROTOCOL
)

// puntMatch is one protocol: IP protocol and a TCP/UDP port (0: any).
type puntMatch struct {
	proto   uint8
	dstPort uint16
	srcPort uint16
}

var puntMatches = []puntMatch{
	{proto: 89},                   // OSPF
	{proto: 17, dstPort: 3784},    // BFD single-hop
	{proto: 17, dstPort: 4784},    // BFD multihop
	{proto: 6, dstPort: 179},      // BGP, sessions to the switch
	{proto: 6, srcPort: 179},      // BGP, sessions the switch opened
}

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }

// protoKeys matches a protocol inside an untagged (vlan false) or tagged
// frame of an IP family.
func protoKeys(m puntMatch, ethType uint16, tagged bool) func(o *nl.RtAttr) {
	return func(o *nl.RtAttr) {
		if tagged {
			o.AddRtAttr(nl.TCA_FLOWER_KEY_ETH_TYPE, be16(ethP8021Q))
			o.AddRtAttr(nl.TCA_FLOWER_KEY_VLAN_ETH_TYPE, be16(ethType))
		} else {
			o.AddRtAttr(nl.TCA_FLOWER_KEY_ETH_TYPE, be16(ethType))
		}
		o.AddRtAttr(nl.TCA_FLOWER_KEY_IP_PROTO, []byte{m.proto})
		switch {
		case m.proto == 6 && m.dstPort != 0:
			o.AddRtAttr(nl.TCA_FLOWER_KEY_TCP_DST, be16(m.dstPort))
		case m.proto == 6 && m.srcPort != 0:
			o.AddRtAttr(nl.TCA_FLOWER_KEY_TCP_SRC, be16(m.srcPort))
		case m.proto == 17 && m.dstPort != 0:
			o.AddRtAttr(nl.TCA_FLOWER_KEY_UDP_DST, be16(m.dstPort))
		}
	}
}

// vlanPush pushes a VLAN tag (802.1Q) and continues.
func vlanPush(vid int) tcAction {
	return func(a *nl.RtAttr) {
		a.AddRtAttr(nl.TCA_ACT_KIND, nl.ZeroTerminated("vlan"))
		o := a.AddRtAttr(nl.TCA_ACT_OPTIONS, nil)
		p := nl.TcVlan{TcGen: nl.TcGen{Action: int32(netlink.TC_ACT_PIPE)}, Action: tcVlanActPush}
		o.AddRtAttr(nl.TCA_VLAN_PARMS, p.Serialize())
		o.AddRtAttr(nl.TCA_VLAN_PUSH_VLAN_ID, nl.Uint16Attr(uint16(vid)))
		o.AddRtAttr(tcaVlanPushPro, be16(ethP8021Q))
	}
}

// redirect sends the frame out of a device (the stack tunnel), ending
// its way through this member.
func redirect(ifindex int) tcAction {
	return func(a *nl.RtAttr) {
		a.AddRtAttr(nl.TCA_ACT_KIND, nl.ZeroTerminated("mirred"))
		o := a.AddRtAttr(nl.TCA_ACT_OPTIONS, nil)
		m := nl.TcMirred{TcGen: nl.TcGen{Action: tcActStolen}, Eaction: tcEgressRedir, Ifindex: uint32(ifindex)}
		o.AddRtAttr(nl.TCA_MIRRED_PARMS, m.Serialize())
	}
}

// puntRules are the rules of one port, in installation order (the hook
// into chain 0 last).
func puntRules(p *Punt, pp PuntPort, tunIdx int) []tcRule {
	var out []tcRule
	add := func(r tcRule) { out = append(out, r) }
	if len(pp.VLANs) > 0 {
		prio := uint16(1)
		for _, m := range puntMatches {
			for _, et := range []uint16{ethPIP, ethPIPv6} {
				add(tcRule{chain: chainPuntTag, prio: prio, proto: ethP8021Q, kind: "flower", keys: protoKeys(m, et, true),
					actions: []tcAction{redirect(tunIdx)}})
				prio++
			}
		}
		add(tcRule{chain: chainPuntTag, prio: prio, proto: ethPAll, kind: "matchall", actions: []tcAction{gact(tcActGoto1)}})
	}
	if pp.PVID != 0 {
		prio := uint16(1)
		for _, m := range puntMatches {
			for _, et := range []uint16{ethPIP, ethPIPv6} {
				add(tcRule{chain: chainPuntUntag, prio: prio, proto: et, kind: "flower", keys: protoKeys(m, et, false),
					actions: []tcAction{vlanPush(pp.PVID), redirect(tunIdx)}})
				prio++
			}
		}
		add(tcRule{chain: chainPuntUntag, prio: prio, proto: ethPAll, kind: "matchall", actions: []tcAction{gact(tcActGoto1)}})
	}
	prio := uint16(1)
	for _, vid := range pp.VLANs {
		v := vid
		add(tcRule{chain: chainPunt, prio: prio, proto: ethP8021Q, kind: "flower", keys: func(o *nl.RtAttr) {
			o.AddRtAttr(nl.TCA_FLOWER_KEY_ETH_TYPE, be16(ethP8021Q))
			o.AddRtAttr(nl.TCA_FLOWER_KEY_VLAN_ID, nl.Uint16Attr(uint16(v)))
		}, actions: []tcAction{gact(tcActGotoChain | chainPuntTag)}})
		prio++
	}
	// Other tagged frames: the usual way (chain 1).
	add(tcRule{chain: chainPunt, prio: prio, proto: ethP8021Q, kind: "matchall", actions: []tcAction{gact(tcActGoto1)}})
	prio++
	if pp.PVID != 0 {
		add(tcRule{chain: chainPunt, prio: prio, proto: ethPAll, kind: "matchall", actions: []tcAction{gact(tcActGotoChain | chainPuntUntag)}})
	} else {
		add(tcRule{chain: chainPunt, prio: prio, proto: ethPAll, kind: "matchall", actions: []tcAction{gact(tcActGoto1)}})
	}
	gw := p.GatewayMAC
	add(tcRule{chain: chainStorm, prio: prioPunt, proto: ethPAll, kind: "flower",
		keys: dstMAC(gw, macBroadcast), actions: []tcAction{gact(tcActGotoChain | chainPunt)}})
	return out
}

// flushChain removes every filter of an ingress chain.
func flushChain(l netlink.Link, chain uint32) error { return removeRule(l, chain, 0) }

// puntSig describes what a port has installed (to change only what
// differs).
func puntSig(p *Punt, pp PuntPort, tunIdx int) string {
	return fmt.Sprintf("%s/%d %x %d %v", p.Tunnel, tunIdx, []byte(p.GatewayMAC), pp.PVID, pp.VLANs)
}

// SyncPunt installs the protocol redirection (nil: removes it).
func (k *Netlink) SyncPunt(p *Punt) (bool, error) {
	k.puntMu.Lock()
	defer k.puntMu.Unlock()
	if k.punted == nil {
		k.punted = map[string]string{}
	}
	want := map[string]PuntPort{}
	tunIdx := 0
	if p != nil && len(p.Ports) > 0 {
		tl, err := nlx.LinkByName(p.Tunnel)
		if err != nil {
			return false, fmt.Errorf("stack tunnel %s: %w", p.Tunnel, err)
		}
		tunIdx = tl.Attrs().Index
		for _, pp := range p.Ports {
			want[pp.Name] = pp
		}
	}
	changed := false
	var errs []error
	// Ports that no longer redirect: the hook first, then the chains.
	for _, name := range slices.Sorted(keysOf(k.punted)) {
		if _, ok := want[name]; ok {
			continue
		}
		if l, err := nlx.LinkByName(name); err == nil {
			errs = append(errs, removePunt(l))
		}
		delete(k.punted, name)
		changed = true
	}
	for _, name := range slices.Sorted(keysOf(want)) {
		pp := want[name]
		sig := puntSig(p, pp, tunIdx)
		if k.punted[name] == sig {
			continue
		}
		l, err := nlx.LinkByName(name)
		if err != nil {
			continue // not there (yet)
		}
		if err := ensureClsact(l); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		// A changed port is rebuilt (the hook is installed last; while the
		// chains are rebuilt, frames take the normal way for a moment).
		if k.punted[name] != "" {
			for _, c := range []uint32{chainPuntTag, chainPuntUntag} {
				errs = append(errs, flushChain(l, c))
			}
			errs = append(errs, flushChain(l, chainPunt))
		}
		ok := true
		for _, r := range puntRules(p, pp, tunIdx) {
			if err := r.install(l); err != nil {
				errs = append(errs, fmt.Errorf("%s chain %#x prio %d: %w", name, r.chain, r.prio, err))
				ok = false
				break
			}
		}
		if ok {
			k.punted[name] = sig
			changed = true
		} else {
			removePunt(l)
		}
	}
	return changed, errors.Join(errs...)
}

func removePunt(l netlink.Link) error {
	return errors.Join(removeRule(l, chainStorm, prioPunt), flushChain(l, chainPunt), flushChain(l, chainPuntTag),
		flushChain(l, chainPuntUntag))
}

func keysOf[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
