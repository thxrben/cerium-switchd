// Package mclag runs a member's side of its MC-LAG pair (reference 5.6):
// leg states with the peer, holds (delay-restore, minority part,
// maintenance mode, consistency), the split horizon, MAC synchronisation
// and the multicast groups of MC-LAG bundles. cer-mclagd runs it.
package mclag

import (
	"encoding/json"
	"time"
)

// LACP is what the controller needs from LACP (cer-lacpd).
type LACP interface {
	Legs() map[string]bool
	Ready() map[string]int
	SetHold(bundle string, hold bool)
	SetPeerReady(bundle string, n int)
}

// Stack reaches the other members over the stacking protocol.
type Stack interface {
	Reachable() []int
	Call(member int, method string, req any, timeout time.Duration) (json.RawMessage, error)
	Handle(method string, h func(from int, req json.RawMessage) (any, error))
}
