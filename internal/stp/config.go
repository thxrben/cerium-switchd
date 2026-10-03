// Package stp runs RSTP for the stack as one bridge (reference 5.5): on
// every member the ports' facts, sockets and kernel states; on the owner
// (the lowest member reached) the state machines of all ports. cer-rstpd
// runs it.
package stp

import (
	"encoding/json"
	"time"
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

// Stack reaches the other members over the stacking protocol.
type Stack interface {
	Reachable() []int
	Draining() []int
	Call(member int, method string, req any, timeout time.Duration) (json.RawMessage, error)
	Handle(method string, h func(from int, req json.RawMessage) (any, error))
}
