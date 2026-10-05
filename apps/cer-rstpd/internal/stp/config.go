// Package stp runs RSTP for the stack as one bridge (reference 5.5): on
// every member the ports' facts, sockets and kernel states; on the owner
// (the lowest member reached) the state machines of all ports. cer-rstpd
// runs it.
package stp

import (
	"encoding/json"
	"time"
)

// Stack reaches the other members over the stacking protocol.
type Stack interface {
	Reachable() []int
	Draining() []int
	Call(member int, method string, req any, timeout time.Duration) (json.RawMessage, error)
	Handle(method string, h func(from int, req json.RawMessage) (any, error))
}
