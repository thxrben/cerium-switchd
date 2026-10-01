// Package dataplane turns the committed configuration into kernel state:
// the switch bridge, bonds, bridge port VLANs, MTUs and admin state.
//
// It works in three steps:
//  1. Compute: configuration -> desired State of this member (pure).
//  2. Plan: actual State (read from the kernel) + desired State -> ordered
//     list of Ops that touches only what differs, never takes down links it
//     does not change, and never lets a port pass through a state that
//     combines old and new permissions (PLAN.md §4.14).
//  3. Execute the Ops against a Kernel (netlink, or a fake in tests).
package dataplane

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sort"
	"strings"

	"mclag/internal/model"
)

// BridgeName is the switch bridge owned by switchd.
const BridgeName = "swbr0"

// Kind of a link.
type Kind int

const (
	Physical Kind = iota
	Bond
	BridgeKind
	Other  // any other virtual device (never touched)
	Tunnel // a stack tunnel (VXLAN to another member, reference 5.2)
)

func (k Kind) String() string {
	return [...]string{"physical", "bond", "bridge", "other", "tunnel"}[k]
}

// The stack tunnels and their hidden underlay instance (reference 5.2,
// docs/stack-protocol.md "Stack tunnels").
const (
	StackVRF        = "swstack"
	StackTable      = 999
	StackUDPPort    = 4789
	MaxStackPortMTU = model.MaxStackPortMTU
)

// StackAddr is the underlay address of a member.
func StackAddr(member int) netip.Addr { return netip.AddrFrom4([4]byte{169, 254, 64, byte(member)}) }

// TunnelName is the kernel name of the stack tunnel to a member.
func TunnelName(member int) string { return fmt.Sprintf("swvc%d", member) }

// TunnelMember returns the member a stack tunnel leads to (0: not a tunnel).
func TunnelMember(name string) int {
	var m int
	if _, err := fmt.Sscanf(name, "swvc%d", &m); err != nil || TunnelName(m) != name {
		return 0
	}
	return m
}

// VXLANName is the kernel name of a member's VXLAN port of a VNI towards
// the remote VTEPs (reference 5.7).
func VXLANName(vni int) string { return fmt.Sprintf("swvx%d", vni) }

// VXLANVNI returns the VNI of a VXLAN port (0: not one).
func VXLANVNI(name string) int {
	var v int
	if _, err := fmt.Sscanf(name, "swvx%d", &v); err != nil || VXLANName(v) != name {
		return 0
	}
	return v
}

// TunnelVNI is the VNI of the tunnel between two members.
func TunnelVNI(a, b int) int { return 32*min(a, b) + max(a, b) }

// TunnelOpts are the parameters of a stack tunnel, or of a VXLAN port to
// the remote VTEPs (VXLANName: Remote unset, Port the UDP port).
type TunnelOpts struct {
	VNI           int
	Local, Remote netip.Addr
	Port          int // VXLAN ports only
}

// VlanFlags are the per-VLAN flags of a bridge port.
type VlanFlags struct {
	PVID     bool // untagged ingress is classified into this VLAN
	Untagged bool // egress without tag
}

// BondOpts are the parameters of a bundle: a static bundle is a bond
// ("balance-xor"), an LACP bundle a team device ("lacp"), whose ports
// switchd's LACP enables one by one.
type BondOpts struct {
	Mode       string // "balance-xor" or "lacp"
	HashPolicy string // layer2, layer2+3, layer3+4
	MinLinks   int    // static bundles (LACP: enforced by the LACP runtime)
	MIIMon     int    // ms (static bundles)
}

// Team reports whether the bundle is an LACP bundle (a team device).
func (o *BondOpts) Team() bool { return o != nil && o.Mode == "lacp" }

// Link is the state of one network device that switchd manages.
type Link struct {
	Name   string
	Kind   Kind
	Up     bool
	MTU    int // kernel MTU (without Ethernet header)
	Alias  string
	Master string // BridgeName, a bond name, or ""
	// VLANs of a bridge port (only when Master == BridgeName).
	VLANs map[uint16]VlanFlags
	Bond  *BondOpts // Kind == Bond
	// FlowControl: nil leaves the driver default.
	FlowControl *bool
	// MaxLearned limits learned MAC addresses (0 = unlimited).
	MaxLearned int
	// DropTagged drops 802.1Q-tagged frames on ingress (access ports).
	DropTagged bool
	// NoLearning: the bridge does not learn addresses on this port (the
	// tunnel to the MC-LAG peer).
	NoLearning bool
	// Storm control: received broadcast / multicast packets per second
	// (0 = unlimited). IEEE link-local multicast is never limited.
	StormBroadcast, StormMulticast int
	// Tunnel: Kind == Tunnel.
	Tunnel *TunnelOpts
	// Isolated: the bridge never forwards between two isolated ports (stack
	// tunnels).
	Isolated bool

	// Actual state only.
	Present bool // exists in the kernel
	MaxMTU  int
}

// Clone returns a deep copy.
func (l *Link) Clone() *Link {
	c := *l
	c.VLANs = maps.Clone(l.VLANs)
	if l.Bond != nil {
		b := *l.Bond
		c.Bond = &b
	}
	if l.FlowControl != nil {
		f := *l.FlowControl
		c.FlowControl = &f
	}
	if l.Tunnel != nil {
		t := *l.Tunnel
		c.Tunnel = &t
	}
	return &c
}

// BridgeOpts are the settings of the switch bridge.
type BridgeOpts struct {
	AgeingSeconds int
}

// L3 is the routed part of the default instance (reference 5.3.2, 5.3.3,
// 5.8): IP interfaces and static routes.
type L3 struct {
	VRFs   []VRF // routing instances
	Ifs    []L3If
	Routes []Route
	// CME is the chassis management interface on this member (only on the
	// master, reference 1.8; nil: none).
	CME *CMEIf
	// Bare are ports that carry no addresses and no IPv6 at all: management
	// ports and ports that are not configured (reference 1.4);
	// Unconfigured are the latter (they carry no description either).
	Bare, Unconfigured []string
	// NoIP are the layer 2 devices: the bridge, its ports, bundle member
	// ports and the stack tunnels. They never carry IP, not even an IPv6
	// link-local address (reference 1.5).
	NoIP []string
	// VTEP is the stack's VXLAN source address on this member (reference
	// 5.7; invalid: none), Remotes the remote VTEPs (accepted by the
	// protection of the routed interfaces), VXLANPort their UDP port.
	VTEP      netip.Addr
	Remotes   []netip.Addr
	VXLANPort int
}

// VRF is a routing instance in the kernel.
type VRF struct {
	Name string
	Mgmt bool // the management instance: its interfaces never forward
}

// L3If is one routed interface in the kernel.
type L3If struct {
	Name   string // kernel name: "irb.10", "sw-0-6.100", or the port itself (untagged unit 0)
	Parent string // kernel parent of a VLAN device ("" for a port itself)
	VID    int    // VLAN id of a VLAN device
	Own    bool   // switchd creates the device (irb, subinterface)
	Up     bool
	MTU    int // kernel MTU of an own device (0 = default)
	Addrs  []netip.Prefix
	VRF    string // routing instance ("" = default)
	// Anycast: an irb unit with addresses shared by every member; it uses
	// the stack-wide gateway MAC and no DAD. Other irb units (only
	// per-member addresses, e.g. management) use this member's own MAC.
	Anycast bool
	// DHCP: the IPv4 address comes from a lease (reference 5.3.2); Unit is
	// the configuration name.
	DHCP bool
	Unit string
}

// DHCPIf is an interface whose address comes from DHCP.
type DHCPIf struct {
	Name, Unit, VRF string
}

// DHCPLease is what the data plane takes from a lease.
type DHCPLease struct {
	Addr   netip.Prefix
	Router netip.Addr // invalid: none
}

// Route is a static route of the default instance.
type Route struct {
	VRF      string // routing instance ("" = default)
	Prefix   netip.Prefix
	NextHops []netip.Addr
	Discard  bool
}

// IPv6 reports whether any data interface (not management) has an IPv6
// address, i.e. IPv6 routing is needed.
func (l *L3) IPv6() bool {
	mgmt := map[string]bool{}
	for _, v := range l.VRFs {
		mgmt[v.Name] = v.Mgmt
	}
	for _, i := range l.Ifs {
		if mgmt[i.VRF] {
			continue
		}
		for _, a := range i.Addrs {
			if a.Addr().Is6() {
				return true
			}
		}
	}
	return false
}

// State is the managed part of a member's kernel configuration.
type State struct {
	Bridge *BridgeOpts // nil: no bridge
	Links  map[string]*Link
	// L3 holds the routed interfaces (never nil in a desired state).
	L3 *L3
	// SelfVLANs are the VLANs the bridge device itself joins (delivered to
	// the CPU): the VLANs of irb interfaces.
	SelfVLANs []int
}

// Clone returns a deep copy.
func (s *State) Clone() *State {
	c := &State{Links: map[string]*Link{}}
	if s.Bridge != nil {
		b := *s.Bridge
		c.Bridge = &b
	}
	if s.L3 != nil {
		l := &L3{Routes: slices.Clone(s.L3.Routes), VRFs: slices.Clone(s.L3.VRFs)}
		for _, i := range s.L3.Ifs {
			i.Addrs = slices.Clone(i.Addrs)
			l.Ifs = append(l.Ifs, i)
		}
		for n := range l.Routes {
			l.Routes[n].NextHops = slices.Clone(l.Routes[n].NextHops)
		}
		if s.L3.CME != nil {
			cme := *s.L3.CME
			cme.Addrs = slices.Clone(cme.Addrs)
			cme.MAC = slices.Clone(cme.MAC)
			l.CME = &cme
		}
		l.Bare = slices.Clone(s.L3.Bare)
		l.Unconfigured = slices.Clone(s.L3.Unconfigured)
		c.L3 = l
	}
	c.SelfVLANs = slices.Clone(s.SelfVLANs)
	for n, l := range s.Links {
		c.Links[n] = l.Clone()
	}
	return c
}

// names returns link names in a stable order.
func (s *State) names() []string {
	n := slices.Collect(maps.Keys(s.Links))
	sort.Strings(n)
	return n
}

func (l *Link) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s(%s up=%v mtu=%d master=%q", l.Name, l.Kind, l.Up, l.MTU, l.Master)
	if len(l.VLANs) > 0 {
		vids := slices.Sorted(maps.Keys(l.VLANs))
		b.WriteString(" vlans=")
		for i, v := range vids {
			if i > 0 {
				b.WriteByte(',')
			}
			f := l.VLANs[v]
			fmt.Fprintf(&b, "%d", v)
			if f.PVID {
				b.WriteString("P")
			}
			if f.Untagged {
				b.WriteString("U")
			}
		}
	}
	b.WriteByte(')')
	return b.String()
}
