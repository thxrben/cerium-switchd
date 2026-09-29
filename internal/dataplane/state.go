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
	"slices"
	"sort"
	"strings"
)

// BridgeName is the switch bridge owned by switchd.
const BridgeName = "swbr0"

// Kind of a link.
type Kind int

const (
	Physical Kind = iota
	Bond
	BridgeKind
	Other // any other virtual device (never touched)
)

func (k Kind) String() string { return [...]string{"physical", "bond", "bridge", "other"}[k] }

// VlanFlags are the per-VLAN flags of a bridge port.
type VlanFlags struct {
	PVID     bool // untagged ingress is classified into this VLAN
	Untagged bool // egress without tag
}

// BondOpts are the bond parameters switchd uses (static bundles; LACP is
// run in userspace in a later phase).
type BondOpts struct {
	Mode       string // always "balance-xor"
	HashPolicy string // layer2, layer2+3, layer3+4
	MinLinks   int
	MIIMon     int // ms
}

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
	// Storm control: received broadcast / multicast packets per second
	// (0 = unlimited). IEEE link-local multicast is never limited.
	StormBroadcast, StormMulticast int

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
	return &c
}

// BridgeOpts are the settings of the switch bridge.
type BridgeOpts struct {
	AgeingSeconds int
}

// State is the managed part of a member's kernel configuration.
type State struct {
	Bridge *BridgeOpts // nil: no bridge
	Links  map[string]*Link
}

// Clone returns a deep copy.
func (s *State) Clone() *State {
	c := &State{Links: map[string]*Link{}}
	if s.Bridge != nil {
		b := *s.Bridge
		c.Bridge = &b
	}
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
