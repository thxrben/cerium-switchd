package bfdd

import "github.com/thxrben/cerium-switchd/internal/api/bfdapi"

// The API of this program is in package bfdapi (switchd and the other
// programs use it); here under the names this package always used.
type (
	SessionSpec = bfdapi.SessionSpec
	Set         = bfdapi.Set
	State       = bfdapi.State
)

const (
	TopicSessions = bfdapi.TopicSessions
	MethodSet     = bfdapi.MethodSet
)
