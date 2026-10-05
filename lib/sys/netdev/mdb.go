package netdev

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink/nl"
)

// The bridge's multicast database through rtnetlink (RTM_GETMDB, uapi
// linux/if_bridge.h) instead of `bridge -j mdb show`: iproute2's JSON
// changes between versions (the router ports did already).

const (
	mdbaMDB    = 1
	mdbaRouter = 2

	mdbaMDBEntry     = 1
	mdbaMDBEntryInfo = 1

	mdbaEattrTimer     = 1
	mdbaEattrSrcList   = 2
	mdbaEattrGroupMode = 3

	mdbaSrcListEntry   = 1
	mdbaSrcAttrAddress = 1

	mdbaRouterPort      = 1
	mdbaRouterPattrType = 2
	mdbaRouterPattrVID  = 5
	mdbaRouterPattrTimr = 1

	mdbPermanent = 1
	mcastInclude = 1

	// sizeofBrMDBEntry is struct br_mdb_entry (ifindex, state, flags,
	// vid, a 16-byte address union and its protocol, padded to 4).
	sizeofBrMDBEntry = 28
	sizeofBrPortMsg  = 8

	rtrPerm = 2 // MDB_RTR_TYPE_PERM
)

// mdbEntry is one decoded membership (ports by ifindex).
type mdbEntry struct {
	ifindex   int
	vid       int
	group     string
	permanent bool
	expires   float64
	mode      string
	sources   []string
}

type mdbRouter struct {
	ifindex   int
	vid       int
	permanent bool
	expires   float64
}

func nlAttrs(b []byte) (map[uint16][]byte, []syscall.NetlinkRouteAttr, error) {
	as, err := nl.ParseRouteAttr(b)
	if err != nil {
		return nil, nil, err
	}
	m := make(map[uint16][]byte, len(as))
	for _, a := range as {
		m[a.Attr.Type&nl.NLA_TYPE_MASK] = a.Value
	}
	return m, as, nil
}

// centis is a kernel timer in clock ticks (USER_HZ = 100) as seconds.
func centis(b []byte) float64 {
	if len(b) != 4 {
		return 0
	}
	return float64(nl.NativeEndian().Uint32(b)) / 100
}

func mdbGroup(addr []byte, proto uint16) string {
	switch proto {
	case 0x0800:
		return netip.AddrFrom4([4]byte(addr[:4])).String()
	case 0x86dd:
		return netip.AddrFrom16([16]byte(addr[:16])).String()
	}
	return net.HardwareAddr(addr[:6]).String()
}

// parseMDB decodes one RTM_GETMDB reply (after the netlink header): the
// bridge's ifindex, its memberships and router ports.
func parseMDB(b []byte) (int, []mdbEntry, []mdbRouter, error) {
	if len(b) < sizeofBrPortMsg {
		return 0, nil, nil, errors.New("mdb: short message")
	}
	bridge := int(nl.NativeEndian().Uint32(b[4:8]))
	top, _, err := nlAttrs(b[sizeofBrPortMsg:])
	if err != nil {
		return 0, nil, nil, err
	}
	var es []mdbEntry
	if v, ok := top[mdbaMDB]; ok {
		_, entries, err := nlAttrs(v)
		if err != nil {
			return 0, nil, nil, err
		}
		for _, e := range entries {
			if e.Attr.Type&nl.NLA_TYPE_MASK != mdbaMDBEntry {
				continue
			}
			_, infos, err := nlAttrs(e.Value)
			if err != nil {
				return 0, nil, nil, err
			}
			for _, in := range infos {
				if in.Attr.Type&nl.NLA_TYPE_MASK != mdbaMDBEntryInfo || len(in.Value) < sizeofBrMDBEntry {
					continue
				}
				v := in.Value
				me := mdbEntry{ifindex: int(nl.NativeEndian().Uint32(v[0:4])), permanent: v[4] == mdbPermanent,
					vid: int(nl.NativeEndian().Uint16(v[6:8])), group: mdbGroup(v[8:24], binary.BigEndian.Uint16(v[24:26]))}
				if len(v) > sizeofBrMDBEntry {
					ea, _, err := nlAttrs(v[sizeofBrMDBEntry:])
					if err != nil {
						return 0, nil, nil, err
					}
					me.expires = centis(ea[mdbaEattrTimer])
					if gm, ok := ea[mdbaEattrGroupMode]; ok && len(gm) == 1 {
						me.mode = "exclude"
						if gm[0] == mcastInclude {
							me.mode = "include"
						}
					}
					if sl, ok := ea[mdbaEattrSrcList]; ok {
						_, srcs, _ := nlAttrs(sl)
						for _, s := range srcs {
							sa, _, _ := nlAttrs(s.Value)
							switch a := sa[mdbaSrcAttrAddress]; len(a) {
							case 4:
								me.sources = append(me.sources, netip.AddrFrom4([4]byte(a)).String())
							case 16:
								me.sources = append(me.sources, netip.AddrFrom16([16]byte(a)).String())
							}
						}
					}
				}
				es = append(es, me)
			}
		}
	}
	var rs []mdbRouter
	if v, ok := top[mdbaRouter]; ok {
		_, ports, err := nlAttrs(v)
		if err != nil {
			return 0, nil, nil, err
		}
		for _, p := range ports {
			if p.Attr.Type&nl.NLA_TYPE_MASK != mdbaRouterPort || len(p.Value) < 4 {
				continue
			}
			r := mdbRouter{ifindex: int(nl.NativeEndian().Uint32(p.Value[:4]))}
			if len(p.Value) > 4 {
				pa, _, err := nlAttrs(p.Value[4:])
				if err == nil {
					if t := pa[mdbaRouterPattrType]; len(t) == 1 {
						r.permanent = t[0] == rtrPerm
					}
					if vid := pa[mdbaRouterPattrVID]; len(vid) == 2 {
						r.vid = int(nl.NativeEndian().Uint16(vid))
					}
					r.expires = centis(pa[mdbaRouterPattrTimr])
				}
			}
			rs = append(rs, r)
		}
	}
	return bridge, es, rs, nil
}
