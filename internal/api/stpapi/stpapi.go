// Package stpapi is the API of cer-rstpd: what switchd and the other programs
// use of it (configuration, requests, states, method and topic names).
package stpapi

import (
	"time"

	"github.com/thxrben/cerium-switchd/pkg/rstp"
)

// Config is what the controller runs (computed by switchd from the
// configuration).
type Config struct {
	Member int  `json:"member"`
	On     bool `json:"on"` // protocols rstp is configured (and not disabled)
	// StackID derives the bridge's MAC (the same on every member).
	StackID       string          `json:"stack_id"`
	Bridge        BridgeConfig    `json:"bridge"`
	Ports         map[string]Port `json:"ports,omitempty"`
	SwitchMembers []int           `json:"switch_members,omitempty"`
	// BPDUBlock: protocols layer2-control bpdu-block, the listed
	// interfaces of this member with their devices; BPDUTimeout its
	// disable-timeout in seconds (0: until cleared).
	BPDUBlock   map[string]string `json:"bpdu_block,omitempty"`
	BPDUTimeout int               `json:"bpdu_timeout,omitempty"`
}

// BridgeConfig is protocols rstp's bridge settings.
type BridgeConfig struct {
	BridgePriority int `json:"bridge_priority"`
	HelloTime      int `json:"hello_time"`
	MaxAge         int `json:"max_age"`
	ForwardDelay   int `json:"forward_delay"`
}

// Port is an RSTP port of the stack.
type Port struct {
	// Members with a device for it (an MC-LAG bundle: both).
	Members []int `json:"members"`
	AE      bool  `json:"ae,omitempty"`
	LACP    bool  `json:"lacp,omitempty"`
	// Device is this member's kernel device ("": none here, or not
	// plugged in); Legs are a bundle's local member ports (speed).
	Device string      `json:"device,omitempty"`
	Legs   []string    `json:"legs,omitempty"`
	Config *PortConfig `json:"config,omitempty"`
}

// PortConfig is protocols rstp interface <port>.
type PortConfig struct {
	Cost       int   `json:"cost,omitempty"`
	Priority   int   `json:"priority,omitempty"`
	Edge       bool  `json:"edge,omitempty"`
	RootGuard  bool  `json:"root_guard,omitempty"`
	PointToPnt *bool `json:"p2p,omitempty"`
}

// TopicBPDUBlocked (key: interface name, value: Blocked) is served by
// cer-rstpd; MethodClearBPDU (request: interface name, "" all) clears.
const (
	TopicBPDUBlocked = "bpdu-blocked"
	MethodClearBPDU  = "stp.clear-bpdu"
)

// Blocked is a port shut down by bpdu-block.
type Blocked struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until,omitempty"` // zero: until cleared
	From  string    `json:"from"`            // the BPDU's source MAC
}

// Status is what "show spanning-tree" shows (from the owner).
type Status struct {
	Running bool
	// Error: why RSTP does not run although configured ("": it runs, or
	// is not configured).
	Error      string
	Owner      int
	Bridge     rstp.BridgeConfig
	Root       rstp.Vector
	RootPort   string
	Times      rstp.Times
	Changes    uint64
	SinceTicks uint64
	Ports      []PortStatus
}

type PortStatus struct {
	Name string
	rstp.PortStatus
}

// MethodClear (ClearRequest) is served by cer-rstpd.
const MethodClear = "stp.clear"

// ClearRequest is clear spanning-tree protocol-migration|statistics.
type ClearRequest struct {
	Migration bool   `json:"migration,omitempty"` // false: statistics
	Port      string `json:"port,omitempty"`      // "": every port
}
