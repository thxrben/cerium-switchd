package ospf

import (
	"fmt"
	"net/netip"
)

// IO sends packets. dst is a multicast group (AllSPF, AllDR of the
// version) or a neighbour's address (OSPFv3: its link-local address). The
// IO layer sends from the interface's address (OSPFv3: its link-local
// address) with TTL/hop limit 1 and the internetwork-control traffic
// class, and computes the OSPFv3 checksum.
type IO interface {
	Send(iface string, dst netip.Addr, pkt []byte)
}

// IfaceConfig is an OSPF interface (reference 5.13).
type IfaceConfig struct {
	Name string // unit name ("irb.10", "1/0/5.0"): the key
	Area ID
	// ID is the interface id: OSPFv3 announces it (hello, router and link
	// LSAs) and needs it unique among the router's interfaces; the kernel
	// interface index fits.
	ID uint32
	// Addr is the interface address the packets come from: OSPFv2 its
	// primary IPv4 address, OSPFv3 its link-local address.
	Addr netip.Addr
	// Prefixes are the subnets of the interface (OSPFv2: IPv4, the
	// primary first; OSPFv3: IPv6 global and unique-local).
	Prefixes     []netip.Prefix
	P2P          bool
	Passive      bool
	Cost         uint16
	Priority     uint8
	Hello        uint16 // seconds
	Dead         uint32
	Retransmit   uint16
	TransitDelay uint16
	MTU          uint16 // the IP MTU (DD packets announce it; 0: not checked)
	Auth         *Auth  // OSPFv2 only
	InstanceID   uint8  // OSPFv3 only
	// Up: the interface has carrier and its address.
	Up bool
}

// External is a route redistributed into OSPF (export policy applied).
type External struct {
	Prefix  netip.Prefix
	Metric  uint32
	Type1   bool
	Tag     uint32
	Forward netip.Addr // invalid: this router
}

// Config is the configuration of a router: one version in one routing
// instance.
type Config struct {
	Version    Version
	RouterID   ID
	Interfaces []IfaceConfig
	Externals  []External
	// Overload announces this router with maximum metric (RFC 6987).
	Overload bool
}

// PathType is the type of an OSPF route (RFC 2328 §11).
type PathType uint8

const (
	IntraArea PathType = iota
	InterArea
	External1
	External2
)

func (t PathType) String() string {
	return [...]string{"Intra", "Inter", "Ext1", "Ext2"}[t&3]
}

// NextHop is a next hop of an OSPF route.
type NextHop struct {
	Iface   string
	Gateway netip.Addr // invalid: directly connected
}

func (n NextHop) String() string {
	if !n.Gateway.IsValid() {
		return "via " + n.Iface
	}
	return fmt.Sprintf("%s via %s", n.Gateway, n.Iface)
}

// Route is a route computed by SPF.
type Route struct {
	Prefix   netip.Prefix
	Type     PathType
	Area     ID
	Cost     uint32 // Ext2: the cost to the ASBR (tie breaker)
	Cost2    uint32 // Ext2: the external metric
	Tag      uint32
	NextHops []NextHop
	// Direct: the network is attached to this router (the RIB has it as a
	// direct route; it is not installed).
	Direct bool
}

// better reports whether a is preferred over b (RFC 2328 §16.4.1/§11):
// path type, then cost (type 2: external metric, then cost to the ASBR).
func (a *Route) better(b *Route) bool {
	if a.Type != b.Type {
		return a.Type < b.Type
	}
	if a.Type == External2 && a.Cost2 != b.Cost2 {
		return a.Cost2 < b.Cost2
	}
	return a.Cost < b.Cost
}

// equal reports equal preference (ECMP).
func (a *Route) equal(b *Route) bool {
	return a.Type == b.Type && a.Cost == b.Cost && (a.Type != External2 || a.Cost2 == b.Cost2)
}
