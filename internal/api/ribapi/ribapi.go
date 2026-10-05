// Package ribapi is the API of cer-ribd: what switchd and the other programs
// use of it (configuration, requests, states, method and topic names).
package ribapi

import (
	"net/netip"

	"github.com/thxrben/cerium-switchd/pkg/rib"
)

// Config is what switchd gives cer-ribd.
type Config struct {
	Instances map[string]Instance `json:"instances"` // "" = the default instance
	// OSPFLimit is the memory slots' capacity of OSPF routes (0: none;
	// reference 5.1).
	OSPFLimit int `json:"ospf_limit,omitempty"`
	// OSPFGrace is the longest OSPF graceful restart (seconds, reference
	// 5.13): OSPF's routes stay that long after a start when it is longer
	// than Grace.
	OSPFGrace int `json:"ospf_grace,omitempty"`
}

// Instance is a routing instance on this member.
type Instance struct {
	// VRF is its kernel VRF device ("" for the default instance).
	VRF string `json:"vrf,omitempty"`
	// Devices: unit -> kernel device on this member (next hops through
	// units of other members are not installed here).
	Devices map[string]string `json:"devices,omitempty"`
	// Routes: the direct, local, static and DHCP routes (by Protocol).
	Routes []rib.Route `json:"routes,omitempty"`
}

// SetRoutes replaces a protocol's routes in an instance (one source of
// it, e.g. a BGP neighbour). Full: the protocol has sent all of them after
// its (re)start, so routes of its kernel id that it did not send can go.
type SetRoutes struct {
	Instance string       `json:"instance"`
	Protocol rib.Protocol `json:"protocol"`
	Source   string       `json:"source,omitempty"`
	Routes   []rib.Route  `json:"routes,omitempty"`
	Full     bool         `json:"full,omitempty"`
}

// RoutesDelta is a protocol's change by prefix (PLAN 15b: BGP sends what
// changed, not tables): every route of the protocol at each prefix
// (Source tells the neighbours apart; none: the prefix is gone).
type RoutesDelta struct {
	Instance string       `json:"instance"`
	Protocol rib.Protocol `json:"protocol"`
	// Seq numbers the deltas per (instance, protocol); a gap means one was
	// lost: the answer asks for a sync and nothing is applied.
	Seq uint64 `json:"seq"`
	// Sync: "begin" starts a full sync (any Seq accepted), "end" ends it:
	// the protocol's routes the sync did not set are withdrawn.
	Sync     string         `json:"sync,omitempty"`
	Prefixes []PrefixRoutes `json:"prefixes,omitempty"`
	Full     bool           `json:"full,omitempty"`
}

// PrefixRoutes is one prefix of a delta.
type PrefixRoutes struct {
	Prefix netip.Prefix `json:"prefix"`
	Routes []rib.Route  `json:"routes,omitempty"`
}

// DeltaReply answers a delta.
type DeltaReply struct {
	Resync bool `json:"resync,omitempty"` // a delta was lost: sync again
}

// DefaultRoute is 0.0.0.0/0.
var DefaultRoute = netip.MustParsePrefix("0.0.0.0/0")
