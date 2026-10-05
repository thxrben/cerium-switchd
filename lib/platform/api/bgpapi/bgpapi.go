// Package bgpapi is the API of cer-bgpd: what switchd and the other programs
// use of it (configuration, requests, states, method and topic names).
package bgpapi

import (
	"net/netip"

	"github.com/thxrben/cerium-switchd/lib/bgp"
	"github.com/thxrben/cerium-switchd/lib/conf/model"
)

// Config is what switchd gives cer-bgpd.
type Config struct {
	Instances []Instance      `json:"instances,omitempty"`
	Policies  *model.Policies `json:"policies,omitempty"`
	// Limits are the memory slots' capacities (0: none; reference 5.1).
	Limits Limits `json:"limits,omitzero"`
}

// Limits are BGP's capacities: prefixes per family, further paths.
type Limits struct {
	IPv4  int `json:"ipv4,omitempty"`
	IPv6  int `json:"ipv6,omitempty"`
	Paths int `json:"paths,omitempty"`
}

// Instance is BGP of one routing instance.
type Instance struct {
	Name      string     `json:"name"` // "": default
	VRF       string     `json:"vrf,omitempty"`
	AS        uint32     `json:"as"`
	RouterID  netip.Addr `json:"router_id"`
	Neighbors []Neighbor `json:"neighbors,omitempty"`
}

// Neighbor is a neighbour with its policies and BFD.
type Neighbor struct {
	bgp.Neighbor
	Import []string   `json:"import,omitempty"`
	Export []string   `json:"export,omitempty"`
	BFDCfg *BFDConfig `json:"bfd,omitempty"`
	// Owner: the member whose routed port reaches the neighbour (0: any,
	// e.g. irb); another member than the master relays the session.
	Owner int `json:"owner,omitempty"`
}

// BFDConfig is a neighbour's bfd-liveness-detection (reference 5.12).
type BFDConfig struct {
	IntervalMs int    `json:"interval_ms"`
	Multiplier int    `json:"multiplier"`
	AuthType   string `json:"auth_type,omitempty"`
	AuthKeyID  int    `json:"auth_key_id,omitempty"`
	AuthKey    string `json:"auth_key,omitempty"`
	// Multihop (UDP 4784, RFC 5883) from Local: eBGP multihop and iBGP to
	// a neighbour that is not directly connected.
	Multihop bool       `json:"multihop,omitempty"`
	Local    netip.Addr `json:"local,omitempty"`
}

const (
	MethodStatus = "bgp.status"
	MethodAdj    = "bgp.adj"
	MethodClear  = "bgp.clear"
	// MethodCounts is the received prefixes and further paths (Counts).
	MethodCounts = "bgp.counts"
)

// InstanceStatus is show bgp summary|neighbor of one instance.
type InstanceStatus struct {
	Instance  string               `json:"instance"`
	AS        uint32               `json:"as"`
	RouterID  netip.Addr           `json:"router_id"`
	Neighbors []bgp.NeighborStatus `json:"neighbors"`
}

// AdjRequest asks for a neighbour's Adj-RIB-In (received) or -Out.
type AdjRequest struct {
	Instance string     `json:"instance"`
	Neighbor netip.Addr `json:"neighbor"`
	Out      bool       `json:"out,omitempty"`
}

// ClearRequest is clear bgp neighbor.
type ClearRequest struct {
	Instance string     `json:"instance"`
	Neighbor netip.Addr `json:"neighbor,omitempty"` // invalid: all
	Mode     string     `json:"mode,omitempty"`     // "", soft, soft-inbound
}

// Counts are the received IPv4 and IPv6 prefixes and further paths of
// every instance (the memory slots' entries, reference 5.1).
type Counts struct {
	IPv4  int `json:"ipv4"`
	IPv6  int `json:"ipv6"`
	Paths int `json:"paths"`
}
