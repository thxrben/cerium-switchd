package mclag

import "github.com/thxrben/cerium-switchd/internal/api/mclagapi"

// The API of this program is in package mclagapi (switchd and the other
// programs use it); here under the names this package always used.
type (
	Config = mclagapi.Config
	Pair   = mclagapi.Pair
	Status = mclagapi.Status
	Bundle = mclagapi.Bundle
)

const (
	RejoinAfter = mclagapi.RejoinAfter
)
