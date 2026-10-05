package ospf

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"time"
)

// Graceful restart (RFC 3623; OSPFv3 RFC 5187): grace LSAs, the helper
// role for a restarting neighbour, and the restarting role (restart.go).

const (
	// v2: opaque link-local LSA (type 9) of opaque type 3.
	V2OpaqueLink   LSType = 9
	graceOpaque           = 3
	tlvGracePeriod        = 1
	tlvGraceReason        = 2
	tlvGraceAddr          = 3 // v2 broadcast/NBMA: the restarting interface's address
)

// Grace restart reasons (RFC 3623 Appendix A).
const (
	ReasonUnknown  = 0
	ReasonSoftware = 1 // software restart
	ReasonReload   = 2 // software reload or upgrade
	ReasonSwitch   = 3 // switch to a redundant control processor
)

// GraceInfo is a grace LSA's content.
type GraceInfo struct {
	Period time.Duration
	Reason uint8
	Addr   netip.Addr // v2: the restarting interface's address
}

// isGrace reports whether an LSA is a grace LSA.
func (v Version) isGrace(l *LSA) bool {
	if v == V2 {
		return l.Type == V2OpaqueLink && uint32(l.ID)>>24 == graceOpaque
	}
	return l.Type == V3Grace
}

// graceInfo decodes a grace LSA's TLVs (from Raw: header, then body).
func graceInfo(l *LSA) (GraceInfo, bool) {
	if len(l.Raw) < lsaHeaderLen {
		return GraceInfo{}, false
	}
	var g GraceInfo
	b := l.Raw[lsaHeaderLen:]
	for len(b) >= 4 {
		t, n := binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[2:]))
		b = b[4:]
		if n > len(b) {
			return GraceInfo{}, false
		}
		v := b[:n]
		switch {
		case t == tlvGracePeriod && n == 4:
			g.Period = time.Duration(binary.BigEndian.Uint32(v)) * time.Second
		case t == tlvGraceReason && n == 1:
			g.Reason = v[0]
		case t == tlvGraceAddr && n == 4:
			g.Addr = netip.AddrFrom4([4]byte(v))
		}
		b = b[(n+3)&^3:]
	}
	return g, g.Period > 0
}

// graceLSA makes this router's grace LSA for an interface.
func (r *Router) graceLSA(i *iface, g GraceInfo) *LSA {
	var body []byte
	tlv := func(t uint16, v []byte) {
		body = binary.BigEndian.AppendUint16(body, t)
		body = binary.BigEndian.AppendUint16(body, uint16(len(v)))
		body = append(body, v...)
		for len(body)%4 != 0 {
			body = append(body, 0)
		}
	}
	tlv(tlvGracePeriod, binary.BigEndian.AppendUint32(nil, uint32(g.Period/time.Second)))
	tlv(tlvGraceReason, []byte{g.Reason})
	h := LSAHeader{AdvRtr: r.rid, Seq: InitialSeq}
	if r.v == V2 {
		h.Type, h.ID, h.Options = V2OpaqueLink, ID(graceOpaque<<24), OptE|OptO
		if a := i.cfg.Addr; a.Is4() && !i.cfg.P2P {
			tlv(tlvGraceAddr, a.AsSlice())
		}
	} else {
		h.Type, h.ID = V3Grace, ID(i.cfg.ID)
	}
	return rawLSA(r.v, h, body)
}

// rawLSA encodes an LSA of a type without a body decoder (kept as Raw).
func rawLSA(v Version, h LSAHeader, body []byte) *LSA {
	b := append(h.appendTo(v, make([]byte, 0, lsaHeaderLen+len(body))), body...)
	binary.BigEndian.PutUint16(b[18:], uint16(len(b)))
	binary.BigEndian.PutUint16(b[16:], 0)
	cs := fletcher(b[2:], 14)
	binary.BigEndian.PutUint16(b[16:], cs)
	h.Length, h.Checksum = uint16(len(b)), cs
	return &LSA{LSAHeader: h, V: v, Raw: b}
}

// adjacent reports whether a neighbour counts as fully adjacent: Full, or
// restarting while this router helps it (its adjacency stays announced and
// used for forwarding, RFC 3623 §3.2).
func (n *neighbor) adjacent() bool {
	return n.state == NbrFull || n.helping()
}

func (n *neighbor) helping() bool { return !n.helpUntil.IsZero() }

// graceReceived starts or ends helping the neighbour that sent a grace LSA
// on interface i (a MaxAge one: its restart is done).
func (r *Router) graceReceived(i *iface, l *LSA) {
	if !r.cfg.GracefulRestart {
		return
	}
	g, ok := graceInfo(l)
	var n *neighbor
	for _, x := range i.sortedNbrs() {
		if (r.v == V2 && !i.cfg.P2P && g.Addr.IsValid() && x.addr == g.Addr) || ((r.v == V3 || i.cfg.P2P || !g.Addr.IsValid()) && x.id == l.AdvRtr) {
			n = x
		}
	}
	if n == nil {
		return
	}
	if l.Age >= MaxAge {
		if n.helping() {
			n.stopHelping("restart completed")
		}
		return
	}
	left := g.Period - time.Duration(l.Age)*time.Second
	switch {
	case !ok || left <= 0:
		r.Log.Info("ospf: grace LSA ignored (no grace period left)", "version", r.v, "interface", i.cfg.Name, "neighbor", n.id)
	case n.helping():
		n.helpUntil = r.now.Add(left) // a renewed grace LSA
	case n.state != NbrFull:
		r.Log.Info("ospf: not helping a restart: the neighbour was not fully adjacent", "version", r.v, "interface", i.cfg.Name, "neighbor", n.id)
	case n.topologyPending():
		r.Log.Info("ospf: not helping a restart: the topology changed", "version", r.v, "interface", i.cfg.Name, "neighbor", n.id)
	default:
		n.helpUntil = r.now.Add(left)
		r.Log.Info("ospf: helping a restarting neighbour", "version", r.v, "interface", i.cfg.Name, "neighbor", n.id,
			"grace", left.Round(time.Second), "reason", g.Reason)
	}
}

// topologyPending: an LSA that changes the topology waits to be sent to the
// neighbour (it would not see it while it restarts).
func (n *neighbor) topologyPending() bool {
	for ref := range n.retrans {
		if n.ifc.r.v.topology(ref.Type) {
			return true
		}
	}
	return false
}

// topology reports whether an LSA type describes the topology (strict LSA
// checking, RFC 3623 §2.2: types 1-5 and 7; v3 the same functions).
func (v Version) topology(t LSType) bool {
	if v == V2 {
		return t >= V2Router && t <= V2External || t == 7
	}
	switch t {
	case V3Router, V3Network, V3InterAreaPrefix, V3InterAreaRouter, V3External, V3IntraAreaPrefix:
		return true
	}
	return false
}

// strictCheck is strict LSA checking (RFC 3623 §3.2): a new instance of a
// topology LSA whose content changed (old: the database copy before it; a
// refresh is no change) ends helping every neighbour of its scope.
func (r *Router) strictCheck(s *scope, l, old *LSA) {
	if !r.v.topology(l.Type) {
		return
	}
	if old != nil && l.Age < MaxAge && old.Age < MaxAge && len(old.Raw) >= lsaHeaderLen && len(l.Raw) >= lsaHeaderLen &&
		bytes.Equal(old.Raw[lsaHeaderLen:], l.Raw[lsaHeaderLen:]) {
		return
	}
	for _, i := range r.floodIfaces(s) {
		for _, n := range i.sortedNbrs() {
			if n.helping() && l.AdvRtr != n.id {
				n.stopHelping("topology changed")
			}
		}
	}
}

func (n *neighbor) stopHelping(why string) {
	r := n.ifc.r
	n.helpUntil = time.Time{}
	// Its current view of the link (from its last hello) counts again.
	changed := n.prio != n.helloPrio || n.dr != n.helloDR || n.bdr != n.helloBDR
	n.prio, n.dr, n.bdr = n.helloPrio, n.helloDR, n.helloBDR
	if changed && !n.ifc.cfg.P2P {
		defer n.ifc.neighborChange()
	}
	r.Log.Info("ospf: helping a restarting neighbour ended", "version", r.v, "interface", n.ifc.cfg.Name, "neighbor", n.id, "reason", why)
	r.dirty = true // the router LSA follows its real state now
	r.scheduleSPF()
}
