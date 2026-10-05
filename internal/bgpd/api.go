package bgpd

import "github.com/thxrben/cerium-switchd/internal/api/bgpapi"

// The API of this program is in package bgpapi (switchd and the other
// programs use it); here under the names this package always used.
type (
	Config         = bgpapi.Config
	Limits         = bgpapi.Limits
	Instance       = bgpapi.Instance
	Neighbor       = bgpapi.Neighbor
	BFDConfig      = bgpapi.BFDConfig
	InstanceStatus = bgpapi.InstanceStatus
	AdjRequest     = bgpapi.AdjRequest
	ClearRequest   = bgpapi.ClearRequest
	Counts         = bgpapi.Counts
)

const (
	MethodStatus = bgpapi.MethodStatus
	MethodAdj    = bgpapi.MethodAdj
	MethodClear  = bgpapi.MethodClear
	MethodCounts = bgpapi.MethodCounts
)
