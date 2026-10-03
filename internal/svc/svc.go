// Package svc is the service protocol between switchd and the cer-
// daemons (reference 1.9, PLAN.md Phase 9a), on top of package ipc.
//
// Every daemon connects to switchd's service socket. switchd publishes:
//
//	topic "config", key <daemon>: the daemon's configuration (its own
//	                              type, computed by switchd)
//	topic "role",   key "":       Role (member id, mastership, stack)
//
// and serves:
//
//	"stack.call"  StackCall -> the reply of the same daemon on another
//	              member (over the stacking protocol)
//	"notify"      Notice: a message to every CLI session of the stack
//
// A daemon names the stacking-protocol methods it serves in its hello
// (Meta); switchd hands it calls for them as "stack:<method>" with
// StackIn.
package svc

import (
	"encoding/json"
	"path/filepath"
)

// SocketDir holds the sockets of all programs.
const SocketDir = "/run/ceros"

// Switchd is switchd's program name on the service protocol.
const Switchd = "switchd"

// Socket returns the socket of a program in dir ("" = SocketDir).
func Socket(dir, program string) string {
	if dir == "" {
		dir = SocketDir
	}
	return filepath.Join(dir, program+".sock")
}

// Topics and methods.
const (
	TopicConfig = "config"
	TopicRole   = "role"
	MethodStack = "stack.call"
	MethodNote  = "notify"
	// StackPrefix precedes stacking-protocol methods handed to a daemon.
	StackPrefix = "stack:"
	// MethodStatus is served by every daemon: its state for show commands.
	MethodStatus = "status"
)

// Role is the member's place in the stack.
type Role struct {
	Member int `json:"member"`
	// Master: this member is the master (standalone: always).
	Master    bool   `json:"master"`
	MasterID  int    `json:"master_id,omitempty"`
	Members   []int  `json:"members,omitempty"`   // configured switch members
	Reachable []int  `json:"reachable,omitempty"` // members reachable over the stack (not this one)
	StackID   string `json:"stack_id,omitempty"`
	HostName  string `json:"host_name,omitempty"`
	// ChassisName is the stack's name (one system to the outside).
	ChassisName string `json:"chassis_name,omitempty"`
}

// Meta is a daemon's hello metadata.
type Meta struct {
	// Stack: stacking-protocol methods this daemon serves.
	Stack []string `json:"stack,omitempty"`
}

// StackCall asks switchd to call the same daemon (or Daemon) on another
// member.
type StackCall struct {
	Member    int             `json:"member"`
	Daemon    string          `json:"daemon,omitempty"` // "": the caller
	Method    string          `json:"method"`
	Data      json.RawMessage `json:"data,omitempty"`
	TimeoutMs int             `json:"timeout_ms,omitempty"`
}

// StackIn is a stacking-protocol call handed to a daemon.
type StackIn struct {
	From int             `json:"from"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Notice is a message for the CLI sessions.
type Notice struct {
	Text string `json:"text"`
}

// TopicLACPPorts (key "": map of kernel port name to whether LACP has the
// port carrying traffic in its bundle) is served by the program that runs
// LACP: LLDP reports bundle membership with it (reference 5.5).
const TopicLACPPorts = "lacp-ports"

// TopicLeases (served by cer-dhcpcd; key: kernel device, value: Lease) are
// the DHCP leases; switchd adds their addresses and default routes
// (reference 5.3.2, 1.9).
const TopicLeases = "leases"

// Lease is what switchd takes from a DHCP lease.
type Lease struct {
	Addr   string `json:"addr"`             // prefix, e.g. 10.1.2.50/24
	Router string `json:"router,omitempty"` // "" none
}
