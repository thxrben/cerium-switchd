// Package bfdapi is the API of cer-bfdd: what switchd and the other programs
// use of it (configuration, requests, states, method and topic names).
package bfdapi

import "github.com/thxrben/cerium-switchd/lib/bfd"

// TopicSessions (key: the session key's string) is a session's state.
const TopicSessions = "bfd-sessions"

// MethodSet replaces a client's sessions (Set).
const MethodSet = "bfd.set"

// SessionSpec is a session a client wants.
type SessionSpec struct {
	Key        bfd.Key `json:"key"`
	Interface  string  `json:"interface,omitempty"`
	IntervalMs int     `json:"interval_ms"` // minimum-interval (tx and rx)
	Multiplier int     `json:"multiplier"`
	// AuthType: keyed-md5, keyed-sha-1 ("": none).
	AuthType  string `json:"auth_type,omitempty"`
	AuthKeyID int    `json:"auth_key_id,omitempty"`
	AuthKey   string `json:"auth_key,omitempty"`
}

// Set is a client's complete list of sessions (a client sends it again
// after either side restarts).
type Set struct {
	Client   string        `json:"client"`
	Sessions []SessionSpec `json:"sessions,omitempty"`
}

// State is a session's state on TopicSessions.
type State struct {
	Up    bool   `json:"up"`
	State string `json:"state"`
	Diag  string `json:"diag,omitempty"`
}
