package bgp

import (
	"errors"
	"net"
	"net/netip"

	"github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// The messages through GoBGP's codec: this file is the only one that
// knows its types.

const maxMsgLen = 4096

func (f Family) rf() bgp.RouteFamily {
	if f == IPv6Unicast {
		return bgp.RF_IPv6_UC
	}
	return bgp.RF_IPv4_UC
}

func familyOfRF(rf bgp.RouteFamily) (Family, bool) {
	switch rf {
	case bgp.RF_IPv4_UC:
		return IPv4Unicast, true
	case bgp.RF_IPv6_UC:
		return IPv6Unicast, true
	}
	return 0, false
}

func addrOf(ip net.IP) netip.Addr {
	a, _ := netip.AddrFromSlice(ip)
	return a.Unmap()
}

func nlri(p netip.Prefix) bgp.AddrPrefixInterface {
	if p.Addr().Is4() {
		return bgp.NewIPAddrPrefix(uint8(p.Bits()), p.Addr().String())
	}
	return bgp.NewIPv6AddrPrefix(uint8(p.Bits()), p.Addr().String())
}

func prefixOf(n bgp.AddrPrefixInterface) (netip.Prefix, bool) {
	switch v := n.(type) {
	case *bgp.IPAddrPrefix:
		return netip.PrefixFrom(addrOf(v.Prefix), int(v.Length)).Masked(), true
	case *bgp.IPv6AddrPrefix:
		return netip.PrefixFrom(addrOf(v.Prefix), int(v.Length)).Masked(), true
	}
	return netip.Prefix{}, false
}

// openMsg is our OPEN.
type openParams struct {
	AS         uint32
	HoldTime   uint16
	RouterID   netip.Addr
	Families   []Family
	GR         bool
	GRTime     uint16
	Restarting bool
}

func openMsg(o openParams) *bgp.BGPMessage {
	caps := []bgp.ParameterCapabilityInterface{bgp.NewCapFourOctetASNumber(o.AS), bgp.NewCapRouteRefresh()}
	var tuples []*bgp.CapGracefulRestartTuple
	for _, f := range o.Families {
		caps = append(caps, bgp.NewCapMultiProtocol(f.rf()))
		tuples = append(tuples, bgp.NewCapGracefulRestartTuple(f.rf(), o.Restarting))
	}
	if o.GR {
		caps = append(caps, bgp.NewCapGracefulRestart(o.Restarting, false, o.GRTime, tuples))
	}
	as2 := uint16(asTrans)
	if o.AS <= 0xffff {
		as2 = uint16(o.AS)
	}
	return bgp.NewBGPOpenMessage(as2, o.HoldTime, o.RouterID.String(), []bgp.OptionParameterInterface{bgp.NewOptionParameterCapability(caps)})
}

// peerOpen is what a neighbour's OPEN says.
type peerOpen struct {
	AS           uint32
	HoldTime     uint16
	RouterID     netip.Addr
	AS4          bool
	RouteRefresh bool
	Families     []Family // nil: no multiprotocol capability (IPv4 unicast only)
	GR           bool
	GRRestarting bool
	GRTime       uint16
	GRForwarding map[Family]bool
}

func parseOpen(o *bgp.BGPOpen) peerOpen {
	p := peerOpen{AS: uint32(o.MyAS), HoldTime: o.HoldTime, RouterID: addrOf(o.ID), GRForwarding: map[Family]bool{}}
	mp := false
	for _, op := range o.OptParams {
		oc, ok := op.(*bgp.OptionParameterCapability)
		if !ok {
			continue
		}
		for _, c := range oc.Capability {
			switch v := c.(type) {
			case *bgp.CapFourOctetASNumber:
				p.AS, p.AS4 = v.CapValue, true
			case *bgp.CapRouteRefresh, *bgp.CapRouteRefreshCisco:
				p.RouteRefresh = true
			case *bgp.CapMultiProtocol:
				mp = true
				if f, ok := familyOfRF(v.CapValue); ok {
					p.Families = append(p.Families, f)
				}
			case *bgp.CapGracefulRestart:
				p.GR, p.GRTime = true, v.Time
				p.GRRestarting = v.Flags&0x08 != 0
				for _, t := range v.Tuples {
					if f, ok := familyOfRF(bgp.AfiSafiToRouteFamily(t.AFI, t.SAFI)); ok {
						p.GRForwarding[f] = t.Flags&0x80 != 0
					}
				}
			}
		}
	}
	if !mp {
		p.Families = []Family{IPv4Unicast}
	}
	return p
}

// encodeASPath is the AS_PATH (and AS4_PATH for a 2-byte neighbour).
func encodeASPath(segs []Segment, as4 bool) []bgp.PathAttributeInterface {
	typ := func(s Segment) uint8 {
		if s.Set {
			return bgp.BGP_ASPATH_ATTR_TYPE_SET
		}
		return bgp.BGP_ASPATH_ATTR_TYPE_SEQ
	}
	if as4 {
		var ps []bgp.AsPathParamInterface
		for _, s := range segs {
			ps = append(ps, bgp.NewAs4PathParam(typ(s), s.ASNs))
		}
		return []bgp.PathAttributeInterface{bgp.NewPathAttributeAsPath(ps)}
	}
	var ps []bgp.AsPathParamInterface
	var ps4 []*bgp.As4PathParam
	wide := false
	for _, s := range segs {
		as := make([]uint16, len(s.ASNs))
		for i, n := range s.ASNs {
			if n > 0xffff {
				as[i], wide = asTrans, true
			} else {
				as[i] = uint16(n)
			}
		}
		ps = append(ps, bgp.NewAsPathParam(typ(s), as))
		ps4 = append(ps4, bgp.NewAs4PathParam(typ(s), s.ASNs))
	}
	out := []bgp.PathAttributeInterface{bgp.NewPathAttributeAsPath(ps)}
	if wide {
		out = append(out, bgp.NewPathAttributeAs4Path(ps4))
	}
	return out
}

// attrsFor encodes the attributes; the reachability (NLRI or MP_REACH) is
// added by the caller.
func attrsFor(a *Attrs, as4 bool) []bgp.PathAttributeInterface {
	out := []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(a.Origin)}
	out = append(out, encodeASPath(a.ASPath, as4)...)
	if a.MED != nil {
		out = append(out, bgp.NewPathAttributeMultiExitDisc(*a.MED))
	}
	if a.LocalPref != nil {
		out = append(out, bgp.NewPathAttributeLocalPref(*a.LocalPref))
	}
	if a.AtomicAggregate {
		out = append(out, bgp.NewPathAttributeAtomicAggregate())
	}
	if len(a.Communities) > 0 {
		out = append(out, bgp.NewPathAttributeCommunities(a.Communities))
	}
	if a.OriginatorID.IsValid() {
		out = append(out, bgp.NewPathAttributeOriginatorId(a.OriginatorID.String()))
	}
	if len(a.ClusterList) > 0 {
		cl := make([]string, len(a.ClusterList))
		for i, c := range a.ClusterList {
			cl[i] = c.String()
		}
		out = append(out, bgp.NewPathAttributeClusterList(cl))
	}
	if len(a.Large) > 0 {
		ls := make([]*bgp.LargeCommunity, len(a.Large))
		for i, l := range a.Large {
			ls[i] = &bgp.LargeCommunity{ASN: l[0], LocalData1: l[1], LocalData2: l[2]}
		}
		out = append(out, bgp.NewPathAttributeLargeCommunities(ls))
	}
	for _, u := range a.Unknown {
		out = append(out, bgp.NewPathAttributeUnknown(bgp.BGPAttrFlag(u.Flags)|bgp.BGP_ATTR_FLAG_PARTIAL, bgp.BGPAttrType(u.Type), u.Value))
	}
	return out
}

func attrsLen(as []bgp.PathAttributeInterface) int {
	n := 0
	for _, a := range as {
		n += a.Len()
	}
	return n
}

// updateMsgs announces prefixes of one family with the same attributes,
// in as few messages as fit.
func updateMsgs(f Family, a *Attrs, as4 bool, prefixes []netip.Prefix) []*bgp.BGPMessage {
	base := attrsFor(a, as4)
	budget := maxMsgLen - 19 - 4 - attrsLen(base) - 40 // MP_REACH header and next hops
	var out []*bgp.BGPMessage
	for len(prefixes) > 0 {
		n, used := 0, 0
		for n < len(prefixes) && used+1+(prefixes[n].Bits()+7)/8 <= budget {
			used += 1 + (prefixes[n].Bits()+7)/8
			n++
		}
		chunk := prefixes[:n]
		prefixes = prefixes[n:]
		if f == IPv4Unicast && a.NextHop.Is4() {
			ps := make([]*bgp.IPAddrPrefix, len(chunk))
			for i, p := range chunk {
				ps[i] = nlri(p).(*bgp.IPAddrPrefix)
			}
			as := append(append([]bgp.PathAttributeInterface{}, base...), bgp.NewPathAttributeNextHop(a.NextHop.String()))
			out = append(out, bgp.NewBGPUpdateMessage(nil, as, ps))
			continue
		}
		ns := make([]bgp.AddrPrefixInterface, len(chunk))
		for i, p := range chunk {
			ns[i] = nlri(p)
		}
		mp := bgp.NewPathAttributeMpReachNLRI(a.NextHop.String(), ns)
		if a.LinkLocal.IsValid() {
			mp.LinkLocalNexthop = net.IP(a.LinkLocal.AsSlice())
		}
		as := append([]bgp.PathAttributeInterface{mp}, base...)
		out = append(out, bgp.NewBGPUpdateMessage(nil, as, nil))
	}
	return out
}

// withdrawMsgs withdraws prefixes of one family.
func withdrawMsgs(f Family, prefixes []netip.Prefix) []*bgp.BGPMessage {
	var out []*bgp.BGPMessage
	const per = (maxMsgLen - 19 - 4 - 16) / 17
	for len(prefixes) > 0 {
		n := min(len(prefixes), per)
		chunk := prefixes[:n]
		prefixes = prefixes[n:]
		if f == IPv4Unicast {
			ps := make([]*bgp.IPAddrPrefix, n)
			for i, p := range chunk {
				ps[i] = nlri(p).(*bgp.IPAddrPrefix)
			}
			out = append(out, bgp.NewBGPUpdateMessage(ps, nil, nil))
			continue
		}
		ns := make([]bgp.AddrPrefixInterface, n)
		for i, p := range chunk {
			ns[i] = nlri(p)
		}
		out = append(out, bgp.NewBGPUpdateMessage(nil, []bgp.PathAttributeInterface{bgp.NewPathAttributeMpUnreachNLRI(ns)}, nil))
	}
	return out
}

func eorMsg(f Family) *bgp.BGPMessage { return bgp.NewEndOfRib(f.rf()) }

// reach is one announced prefix with its next hop.
type reach struct {
	Prefix    netip.Prefix
	NextHop   netip.Addr
	LinkLocal netip.Addr
}

// decoded is a received UPDATE.
type decoded struct {
	Attrs     Attrs
	Reach     []reach
	Withdrawn []netip.Prefix
	EOR       []Family
	// HasAS4Path: AS4_PATH to merge (a 2-byte neighbour).
	as4Path []Segment
}

// decodeUpdate converts an UPDATE; families the session does not carry are
// ignored. withdrawAll (RFC 7606 treat-as-withdraw) turns every announced
// prefix into a withdrawal.
func decodeUpdate(u *bgp.BGPUpdate, withdrawAll bool) decoded {
	var d decoded
	if eor, rf := u.IsEndOfRib(); eor {
		if f, ok := familyOfRF(rf); ok {
			d.EOR = append(d.EOR, f)
		}
		return d
	}
	for _, w := range u.WithdrawnRoutes {
		if p, ok := prefixOf(w); ok {
			d.Withdrawn = append(d.Withdrawn, p)
		}
	}
	var nh4 netip.Addr
	var classic []netip.Prefix
	for _, n := range u.NLRI {
		if p, ok := prefixOf(n); ok {
			classic = append(classic, p)
		}
	}
	for _, pa := range u.PathAttributes {
		switch v := pa.(type) {
		case *bgp.PathAttributeOrigin:
			d.Attrs.Origin = v.Value
		case *bgp.PathAttributeAsPath:
			for _, s := range v.Value {
				seg := Segment{Set: s.GetType() == bgp.BGP_ASPATH_ATTR_TYPE_SET}
				switch x := s.(type) {
				case *bgp.As4PathParam:
					seg.ASNs = append(seg.ASNs, x.AS...)
				case *bgp.AsPathParam:
					for _, n := range x.AS {
						seg.ASNs = append(seg.ASNs, uint32(n))
					}
				}
				d.Attrs.ASPath = append(d.Attrs.ASPath, seg)
			}
		case *bgp.PathAttributeAs4Path:
			for _, s := range v.Value {
				d.as4Path = append(d.as4Path, Segment{Set: s.Type == bgp.BGP_ASPATH_ATTR_TYPE_SET, ASNs: append([]uint32(nil), s.AS...)})
			}
		case *bgp.PathAttributeNextHop:
			nh4 = addrOf(v.Value)
		case *bgp.PathAttributeMultiExitDisc:
			m := v.Value
			d.Attrs.MED = &m
		case *bgp.PathAttributeLocalPref:
			l := v.Value
			d.Attrs.LocalPref = &l
		case *bgp.PathAttributeAtomicAggregate:
			d.Attrs.AtomicAggregate = true
		case *bgp.PathAttributeCommunities:
			d.Attrs.Communities = append([]uint32(nil), v.Value...)
		case *bgp.PathAttributeOriginatorId:
			d.Attrs.OriginatorID = addrOf(v.Value)
		case *bgp.PathAttributeClusterList:
			for _, c := range v.Value {
				d.Attrs.ClusterList = append(d.Attrs.ClusterList, addrOf(c))
			}
		case *bgp.PathAttributeLargeCommunities:
			for _, l := range v.Values {
				d.Attrs.Large = append(d.Attrs.Large, [3]uint32{l.ASN, l.LocalData1, l.LocalData2})
			}
		case *bgp.PathAttributeMpReachNLRI:
			if _, ok := familyOfRF(bgp.AfiSafiToRouteFamily(v.AFI, v.SAFI)); !ok {
				continue
			}
			nh, ll := addrOf(v.Nexthop), addrOf(v.LinkLocalNexthop)
			for _, n := range v.Value {
				if p, ok := prefixOf(n); ok {
					d.Reach = append(d.Reach, reach{Prefix: p, NextHop: nh, LinkLocal: ll})
				}
			}
		case *bgp.PathAttributeMpUnreachNLRI:
			for _, n := range v.Value {
				if p, ok := prefixOf(n); ok {
					d.Withdrawn = append(d.Withdrawn, p)
				}
			}
		case *bgp.PathAttributeAggregator, *bgp.PathAttributeExtendedCommunities:
			// not used; Aggregator is not passed on (a router that
			// aggregates sets it itself).
		default:
			f := pa.GetFlags()
			if f&bgp.BGP_ATTR_FLAG_OPTIONAL != 0 && f&bgp.BGP_ATTR_FLAG_TRANSITIVE != 0 {
				if u, ok := pa.(*bgp.PathAttributeUnknown); ok {
					d.Attrs.Unknown = append(d.Attrs.Unknown, RawAttr{Flags: uint8(f), Type: uint8(pa.GetType()), Value: append([]byte(nil), u.Value...)})
				}
			}
		}
	}
	for _, p := range classic {
		d.Reach = append(d.Reach, reach{Prefix: p, NextHop: nh4})
	}
	if withdrawAll {
		for _, r := range d.Reach {
			d.Withdrawn = append(d.Withdrawn, r.Prefix)
		}
		d.Reach = nil
	}
	return d
}

// mergeAS4 rebuilds the path of a 2-byte neighbour (RFC 6793 §4.2.3).
func mergeAS4(path, as4 []Segment) []Segment {
	n, n4 := (&Attrs{ASPath: path}).ASPathLen(), (&Attrs{ASPath: as4}).ASPathLen()
	if len(as4) == 0 || n4 > n {
		return path
	}
	keep := n - n4
	var out []Segment
	for _, s := range path {
		if keep <= 0 {
			break
		}
		if s.Set {
			out = append(out, s)
			keep--
			continue
		}
		k := min(keep, len(s.ASNs))
		out = append(out, Segment{ASNs: append([]uint32(nil), s.ASNs[:k]...)})
		keep -= k
	}
	return append(out, as4...)
}

// errorHandling classifies a decoding error (RFC 7606).
func errorHandling(err error) (withdraw, discard bool, notify *bgp.MessageError) {
	var me *bgp.MessageError
	if !errors.As(err, &me) {
		return false, false, bgp.NewMessageError(bgp.BGP_ERROR_UPDATE_MESSAGE_ERROR, bgp.BGP_ERROR_SUB_MALFORMED_ATTRIBUTE_LIST, nil, err.Error()).(*bgp.MessageError)
	}
	switch me.ErrorHandling {
	case bgp.ERROR_HANDLING_TREAT_AS_WITHDRAW, bgp.ERROR_HANDLING_AFISAFI_DISABLE:
		return true, false, nil
	case bgp.ERROR_HANDLING_ATTRIBUTE_DISCARD:
		return false, true, nil
	}
	return false, false, me
}

func routeRefreshMsg(f Family) *bgp.BGPMessage {
	afi, safi := bgp.RouteFamilyToAfiSafi(f.rf())
	return bgp.NewBGPRouteRefreshMessage(afi, 0, safi)
}
