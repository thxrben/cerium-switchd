package stp

import "github.com/thxrben/cerium-switchd/lib/platform/api/stpapi"

// The API of this program is in package stpapi (switchd and the other
// programs use it); here under the names this package always used.
type (
	Config       = stpapi.Config
	BridgeConfig = stpapi.BridgeConfig
	Port         = stpapi.Port
	PortConfig   = stpapi.PortConfig
	Status       = stpapi.Status
	PortStatus   = stpapi.PortStatus
	ClearRequest = stpapi.ClearRequest
	Blocked      = stpapi.Blocked
)

const (
	MethodClear      = stpapi.MethodClear
	MethodClearBPDU  = stpapi.MethodClearBPDU
	TopicBPDUBlocked = stpapi.TopicBPDUBlocked
)
