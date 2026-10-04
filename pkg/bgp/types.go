// Package bgp is a BGP-4 speaker (RFC 4271) for one routing instance: the
// sessions with their state machines and timers, capabilities (4-byte AS,
// multiprotocol IPv4/IPv6 unicast, route refresh, graceful restart), the
// Adj-RIBs, the decision process with multipath, route reflection (RFC
// 4456) and graceful restart (RFC 4724). Messages are encoded by GoBGP's
// packet codec; everything else is here. Policies are hooks (the program
// evaluates them), connections come from a Transport (sockets with TCP MD5
// and TTL are the program's), and routes leave through OnRoutes.
package bgp

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Family is an address family (AFI/SAFI) the speaker carries.
type Family uint8

const (
	IPv4Unicast Family = iota
	IPv6Unicast
)

func (f Family) String() string {
	if f == IPv6Unicast {
		return "inet6-unicast"
	}
	return "inet-unicast"
}

// FamilyOf is the family of a prefix.
func FamilyOf(p netip.Prefix) Family {
	if p.Addr().Is4() {
		return IPv4Unicast
	}
	return IPv6Unicast
}

// Neighbor is a configured neighbour with its effective settings.
type Neighbor struct {
	Addr     netip.Addr
	Group    string
	PeerAS   uint32
	LocalAS  uint32 // the AS this session uses (local-as, else the speaker's)
	Internal bool
	// HoldTime in seconds (0: no keepalives; 3..65535).
	HoldTime int
	Passive  bool
	Families []Family
	// Cluster: this speaker reflects routes for the neighbour (a client).
	Cluster       netip.Addr
	RemovePrivate bool
	// Graceful restart (RFC 4724): advertised and honoured (helper).
	GracefulRestart bool
	RestartTime     int // seconds announced to the neighbour
	StaleTime       int // seconds its routes are kept while it restarts
	Multipath       bool
	MultipleAS      bool
	Disabled        bool
	// NextHop4/6: the address announced as next hop self in the other
	// family than the session's (invalid: the session's own address).
	NextHop4, NextHop6 netip.Addr
	// Transport settings (the program's): they only matter for a reset.
	LocalAddress netip.Addr
	TTL          int
	AuthKey      string
}

// resetKeys are the settings whose change needs a new session.
func (n *Neighbor) resetKey() string {
	return fmt.Sprintf("%d|%d|%v|%d|%v|%v|%v|%v|%d|%q|%d", n.PeerAS, n.LocalAS, n.Internal, n.HoldTime, n.Passive,
		n.Families, n.LocalAddress, n.GracefulRestart, n.TTL, n.AuthKey, n.RestartTime)
}

func (n *Neighbor) carries(f Family) bool { return slices.Contains(n.Families, f) }

// Config is the speaker's configuration.
type Config struct {
	AS        uint32
	RouterID  netip.Addr // IPv4
	Neighbors []Neighbor
	// ConnectRetry is the time between connection attempts (0: 30 s; the
	// first attempt is at once).
	ConnectRetry time.Duration
	// Restarting: the speaker restarted with the routes kept in the
	// forwarding plane (graceful restart R bit, for the restart time).
	Restarting bool
}

// Segment is one AS path segment.
type Segment struct {
	Set  bool // AS_SET (else AS_SEQUENCE)
	ASNs []uint32
}

// Origin values.
const (
	OriginIGP        = 0
	OriginEGP        = 1
	OriginIncomplete = 2
)

// Attrs are a path's attributes.
type Attrs struct {
	Origin    uint8
	ASPath    []Segment
	NextHop   netip.Addr
	LinkLocal netip.Addr // IPv6: the next hop's link-local address
	MED       *uint32
	LocalPref *uint32
	// Communities (RFC 1997) and large communities (RFC 8092).
	Communities     []uint32
	Large           [][3]uint32
	OriginatorID    netip.Addr
	ClusterList     []netip.Addr
	AtomicAggregate bool
	// Unknown: optional transitive attributes passed on unchanged (with the
	// partial bit).
	Unknown []RawAttr
}

// RawAttr is an attribute the speaker does not know.
type RawAttr struct {
	Flags, Type uint8
	Value       []byte
}

// Clone copies the attributes (slices included).
func (a Attrs) Clone() Attrs {
	b := a
	b.ASPath = make([]Segment, len(a.ASPath))
	for i, s := range a.ASPath {
		b.ASPath[i] = Segment{Set: s.Set, ASNs: slices.Clone(s.ASNs)}
	}
	if a.MED != nil {
		v := *a.MED
		b.MED = &v
	}
	if a.LocalPref != nil {
		v := *a.LocalPref
		b.LocalPref = &v
	}
	b.Communities = slices.Clone(a.Communities)
	b.Large = slices.Clone(a.Large)
	b.ClusterList = slices.Clone(a.ClusterList)
	b.Unknown = slices.Clone(a.Unknown)
	return b
}

// ASPathLen is the length for the decision process (an AS_SET counts 1).
func (a *Attrs) ASPathLen() int {
	n := 0
	for _, s := range a.ASPath {
		if s.Set {
			n++
		} else {
			n += len(s.ASNs)
		}
	}
	return n
}

// ASNs lists the path's AS numbers in order (sets flattened).
func (a *Attrs) ASNs() []uint32 {
	var out []uint32
	for _, s := range a.ASPath {
		out = append(out, s.ASNs...)
	}
	return out
}

// FirstAS is the neighbouring AS of the path (0: empty path).
func (a *Attrs) FirstAS() uint32 {
	for _, s := range a.ASPath {
		if !s.Set && len(s.ASNs) > 0 {
			return s.ASNs[0]
		}
	}
	return 0
}

// Prepend adds ASNs in front (as AS_SEQUENCE).
func (a *Attrs) Prepend(asns ...uint32) {
	if len(asns) == 0 {
		return
	}
	if len(a.ASPath) > 0 && !a.ASPath[0].Set && len(a.ASPath[0].ASNs)+len(asns) <= 255 {
		a.ASPath[0].ASNs = append(slices.Clone(asns), a.ASPath[0].ASNs...)
		return
	}
	a.ASPath = append([]Segment{{ASNs: slices.Clone(asns)}}, a.ASPath...)
}

// PathString is the AS path as Junos shows it ("65001 65002 {65003} I").
func (a *Attrs) PathString() string {
	var parts []string
	for _, s := range a.ASPath {
		ns := make([]string, len(s.ASNs))
		for i, n := range s.ASNs {
			ns[i] = strconv.FormatUint(uint64(n), 10)
		}
		if s.Set {
			parts = append(parts, "{"+strings.Join(ns, " ")+"}")
		} else {
			parts = append(parts, ns...)
		}
	}
	parts = append(parts, [...]string{"I", "E", "?"}[min(a.Origin, 2)])
	return strings.Join(parts, " ")
}

// Path is a route to a prefix with its attributes and where it came from.
type Path struct {
	Prefix netip.Prefix
	Attrs
	// Peer is the neighbour it was learned from (invalid: originated here).
	Peer   netip.Addr
	PeerAS uint32
	PeerID netip.Addr // the neighbour's router id
	EBGP   bool
	// Source: for originated paths the protocol the route came from
	// (static, ospf, direct, ...: export policies match it).
	Source string
	Stale  bool // kept while the neighbour restarts (graceful restart)
	Since  time.Time

	// Set by an export policy: next-hop self, an explicit next hop
	// (NextHop), a MED (MED; else a MED is not sent to another AS).
	NextHopSelf   bool
	PolicyNextHop bool
	PolicyMED     bool
}

// Local reports whether the path is originated by this speaker.
func (p *Path) Local() bool { return !p.Peer.IsValid() }

// localPref is the effective local preference (default 100).
func (p *Path) localPref() uint32 {
	if p.LocalPref != nil {
		return *p.LocalPref
	}
	return 100
}

func (p *Path) med() uint32 {
	if p.MED != nil {
		return *p.MED
	}
	return 0
}

// isPrivateAS: RFC 6996 private AS numbers.
func isPrivateAS(as uint32) bool {
	return (as >= 64512 && as <= 65534) || (as >= 4200000000 && as <= 4294967294)
}

// asTrans is AS_TRANS (RFC 6793).
const asTrans = 23456
