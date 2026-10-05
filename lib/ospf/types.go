package ospf

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// Version is the OSPF version: 2 (IPv4, RFC 2328) or 3 (IPv6, RFC 5340).
type Version uint8

const (
	V2 Version = 2
	V3 Version = 3
)

// ID is a 32-bit identifier: a router id, an area id, a link state id, and
// in OSPFv2 also an interface address (DR, BDR, link data). It is written
// dotted like an IPv4 address.
type ID uint32

// IDFrom converts an IPv4 address (invalid or IPv6: 0).
func IDFrom(a netip.Addr) ID {
	if !a.Is4() {
		return 0
	}
	return ID(binary.BigEndian.Uint32(a.AsSlice()))
}

// Addr returns the ID as an IPv4 address.
func (i ID) Addr() netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(i))
	return netip.AddrFrom4(b)
}

func (i ID) String() string { return i.Addr().String() }

// ParseID parses a dotted id or a decimal number ("0" is area 0.0.0.0).
func ParseID(s string) (ID, error) {
	if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
		return IDFrom(a), nil
	}
	var n uint32
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && fmt.Sprint(n) == s {
		return ID(n), nil
	}
	return 0, fmt.Errorf("ospf: bad id %q", s)
}

// Backbone is area 0.
const Backbone ID = 0

// LSType is an LSA type. OSPFv2 types are 1–5 (the low byte of the v2
// options+type field); OSPFv3 types are the 16-bit function codes with the
// U bit and the flooding scope (RFC 5340 A.4.2.1).
type LSType uint16

// OSPFv2 LSA types (RFC 2328 A.4.1).
const (
	V2Router      LSType = 1
	V2Network     LSType = 2
	V2Summary     LSType = 3
	V2ASBRSummary LSType = 4
	V2External    LSType = 5
)

// OSPFv3 LSA types (RFC 5340 A.4.2.1).
const (
	V3Router          LSType = 0x2001
	V3Network         LSType = 0x2002
	V3InterAreaPrefix LSType = 0x2003
	V3InterAreaRouter LSType = 0x2004
	V3External        LSType = 0x4005
	V3Link            LSType = 0x0008
	V3IntraAreaPrefix LSType = 0x2009
	V3Grace           LSType = 0x000b // RFC 5187
)

// Scope is an LSA's flooding scope.
type Scope uint8

const (
	ScopeLink Scope = iota
	ScopeArea
	ScopeAS
)

// Scope returns the flooding scope of an LSA type in a version. Unknown
// OSPFv3 types carry their scope in the S bits (RFC 5340 A.4.2.1).
func (v Version) Scope(t LSType) Scope {
	if v == V2 {
		switch t {
		case V2External:
			return ScopeAS
		case 9: // opaque link-local
			return ScopeLink
		case 11: // opaque AS
			return ScopeAS
		}
		return ScopeArea
	}
	switch (t >> 13) & 3 {
	case 0:
		return ScopeLink
	case 1:
		return ScopeArea
	case 2:
		return ScopeAS
	}
	return ScopeLink // reserved: treated as link-local
}

// Known reports whether the type is one this implementation parses.
func (v Version) Known(t LSType) bool {
	if v == V2 {
		return t >= V2Router && t <= V2External
	}
	switch t {
	case V3Router, V3Network, V3InterAreaPrefix, V3InterAreaRouter, V3External, V3Link, V3IntraAreaPrefix:
		return true
	}
	return false
}

// TypeName returns the name of an LSA type (as show ospf database).
func (v Version) TypeName(t LSType) string {
	if v == V2 {
		switch t {
		case V2Router:
			return "Router"
		case V2Network:
			return "Network"
		case V2Summary:
			return "Summary"
		case V2ASBRSummary:
			return "ASBRSum"
		case V2External:
			return "Extern"
		}
	} else {
		switch t {
		case V3Router:
			return "Router"
		case V3Network:
			return "Network"
		case V3InterAreaPrefix:
			return "InterArPfx"
		case V3InterAreaRouter:
			return "InterArRtr"
		case V3External:
			return "Extern"
		case V3Link:
			return "Link"
		case V3IntraAreaPrefix:
			return "IntraArPfx"
		case V3Grace:
			return "Grace"
		}
	}
	return fmt.Sprintf("0x%04x", uint16(t))
}

// Options bits. OSPFv2 uses the low byte (RFC 2328 A.2), OSPFv3 24 bits
// (RFC 5340 A.2).
const (
	OptV6 = 0x01 // v3: the router forwards IPv6
	OptE  = 0x02 // AS-external LSAs flooded (not a stub area)
	OptMC = 0x04
	OptN  = 0x08 // v2 NP / v3 N (NSSA)
	OptR  = 0x10 // v3: the router forwards (not a host)
	OptDC = 0x20
	OptL  = 0x10 // v2: link-local signalling (RFC 5613)
	OptO  = 0x40 // v2: opaque LSAs (RFC 5250)
	OptAF = 0x100
)

// Prefix options (OSPFv3, RFC 5340 A.4.1.1).
const (
	PrefixNU = 0x01 // no unicast: not used for routing
	PrefixLA = 0x02 // local address (a host route of the router itself)
	PrefixP  = 0x08 // propagate (NSSA)
	PrefixDN = 0x10
)

// Packet types (both versions).
const (
	TypeHello = 1
	TypeDD    = 2
	TypeLSR   = 3
	TypeLSU   = 4
	TypeLSAck = 5
)

var typeNames = [...]string{"", "Hello", "DD", "LSR", "LSU", "LSAck"}

// TypeName returns the name of a packet type.
func TypeName(t uint8) string {
	if int(t) < len(typeNames) && t > 0 {
		return typeNames[t]
	}
	return fmt.Sprintf("type %d", t)
}

// ProtoOSPF is the IP protocol number.
const ProtoOSPF = 89

// Multicast groups.
var (
	AllSPFRouters   = netip.MustParseAddr("224.0.0.5")
	AllDRouters     = netip.MustParseAddr("224.0.0.6")
	AllSPFRouters6  = netip.MustParseAddr("ff02::5")
	AllDRouters6    = netip.MustParseAddr("ff02::6")
	ipv6ChecksumOff = 12 // the checksum's offset in the OSPFv3 header (IPV6_CHECKSUM)
)

// IPv6ChecksumOffset is the offset of the OSPFv3 checksum for the raw
// socket option IPV6_CHECKSUM (the kernel computes and verifies it).
func IPv6ChecksumOffset() int { return ipv6ChecksumOff }

// AllSPF returns the AllSPFRouters group of a version.
func (v Version) AllSPF() netip.Addr {
	if v == V3 {
		return AllSPFRouters6
	}
	return AllSPFRouters
}

// AllDR returns the AllDRouters group of a version.
func (v Version) AllDR() netip.Addr {
	if v == V3 {
		return AllDRouters6
	}
	return AllDRouters
}

// Architectural constants (RFC 2328 B).
const (
	MaxAge        = 3600
	MaxAgeDiff    = 900
	LSRefreshTime = 1800
	MinLSInterval = 5
	MinLSArrival  = 1
	CheckAge      = 300
	InitialSeq    = int32(-0x7fffffff) // 0x80000001
	MaxSeq        = int32(0x7fffffff)
	LSInfinity    = 0xffffff
	MaxMetric     = 0xffff // RFC 6987 (overload)
	MaxECMP       = 16
)
