package ospfd

import "github.com/thxrben/cerium-switchd/internal/api/ospfapi"

// The API of this program is in package ospfapi (switchd and the other
// programs use it); here under the names this package always used.
type (
	Config         = ospfapi.Config
	Instance       = ospfapi.Instance
	Iface          = ospfapi.Iface
	StatusRequest  = ospfapi.StatusRequest
	ClearRequest   = ospfapi.ClearRequest
	BFDSpec        = ospfapi.BFDSpec
	InstanceStatus = ospfapi.InstanceStatus
)

const (
	MethodStatus = ospfapi.MethodStatus
	MethodClear  = ospfapi.MethodClear
)
