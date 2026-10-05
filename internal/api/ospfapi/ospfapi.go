// Package ospfapi is the API of cer-ospfd: what switchd and the other programs
// use of it (configuration, requests, states, method and topic names).
package ospfapi

import (
	"fmt"
	"net/netip"

	"github.com/thxrben/cerium-switchd/internal/api/bfdapi"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/pkg/bfd"
	"github.com/thxrben/cerium-switchd/pkg/ospf"
)

// BFDSpec is an interface's bfd-liveness-detection.
type BFDSpec struct {
	IntervalMs int    `json:"interval_ms"`
	Multiplier int    `json:"multiplier"`
	AuthType   string `json:"auth_type,omitempty"` // keyed-md5, keyed-sha-1 ("": none)
	AuthKeyID  int    `json:"auth_key_id,omitempty"`
	AuthKey    string `json:"auth_key,omitempty"`
}

// spec converts an interface's settings for cer-bfdd.
// Session is the BFD session of a neighbour on unit.
func (s BFDSpec) Session(k bfd.Key, unit string) bfdapi.SessionSpec {
	return bfdapi.SessionSpec{Key: k, Interface: unit, IntervalMs: s.IntervalMs, Multiplier: s.Multiplier,
		AuthType: s.AuthType, AuthKeyID: s.AuthKeyID, AuthKey: s.AuthKey}
}

// Config is what switchd gives cer-ospfd.
type Config struct {
	Instances []Instance      `json:"instances,omitempty"`
	Policies  *model.Policies `json:"policies,omitempty"`
	// ExtLimit is the memory slots' capacity of OSPF routes: the external
	// database overflow limit (RFC 1765; 0: none; reference 5.1).
	ExtLimit int `json:"ext_limit,omitempty"`
}

// Instance is OSPF or OSPFv3 of one routing instance.
type Instance struct {
	Name            string       `json:"name"` // routing instance ("": default)
	VRF             string       `json:"vrf,omitempty"`
	Version         ospf.Version `json:"version"`
	RouterID        ospf.ID      `json:"router_id"`
	Interfaces      []Iface      `json:"interfaces,omitempty"`
	Export          []string     `json:"export,omitempty"`
	ReferenceBW     uint64       `json:"reference_bw"` // bit/s
	Overload        bool         `json:"overload,omitempty"`
	OverloadTimeout int          `json:"overload_timeout,omitempty"` // s after start (0: always)
	GracefulRestart bool         `json:"graceful_restart,omitempty"`
	RestartDuration int          `json:"restart_duration,omitempty"`
}

// Iface is an OSPF interface.
type Iface struct {
	Unit         string         `json:"unit"`
	Device       string         `json:"device,omitempty"` // kernel device on this member ("": another member's)
	Area         ospf.ID        `json:"area"`
	Prefixes     []netip.Prefix `json:"prefixes,omitempty"` // the unit's addresses of the family
	P2P          bool           `json:"p2p,omitempty"`
	Passive      bool           `json:"passive,omitempty"`
	Metric       int            `json:"metric,omitempty"` // 0: reference bandwidth / speed
	Priority     int            `json:"priority"`
	Hello        int            `json:"hello"`
	Dead         int            `json:"dead"`
	Retransmit   int            `json:"retransmit"`
	TransitDelay int            `json:"transit_delay"`
	Simple       string         `json:"simple,omitempty"`
	MD5          map[int]string `json:"md5,omitempty"`
	// Owners are the members that have the unit's device (a routed port:
	// its member; a bundle: the members of its legs); IRB: an irb (on
	// every member, its frames reach the master in their VLAN).
	Owners []int `json:"owners,omitempty"`
	IRB    bool  `json:"irb,omitempty"`
	// BFD: bfd-liveness-detection (nil: none).
	BFD *BFDSpec `json:"bfd,omitempty"`
}

// Key identifies the instance (routing instance and OSPF version).
func (i Instance) Key() string { return fmt.Sprintf("%s/v%d", i.Name, i.Version) }

// InstanceStatus is the state of one instance for show ospf|ospf3.
type InstanceStatus struct {
	Instance string       `json:"instance"`
	Version  ospf.Version `json:"version"`
	ospf.Status
}

// StatusRequest selects the status (MethodStatus).
type StatusRequest struct {
	Version  ospf.Version `json:"version,omitempty"` // 0: both
	Instance *string      `json:"instance,omitempty"`
	Detail   bool         `json:"detail,omitempty"`
}

// ClearRequest is clear ospf|ospf3 neighbor (MethodClear).
type ClearRequest struct {
	Version  ospf.Version `json:"version"`
	Instance string       `json:"instance"`
	Neighbor netip.Addr   `json:"neighbor,omitempty"`
}

const (
	MethodStatus = "ospf.status"
	MethodClear  = "ospf.clear"
)
