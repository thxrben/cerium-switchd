package ribd

import "github.com/thxrben/cerium-switchd/lib/platform/api/ribapi"

// The API of this program is in package ribapi (switchd and the other
// programs use it); here under the names this package always used.
type (
	Config       = ribapi.Config
	Instance     = ribapi.Instance
	SetRoutes    = ribapi.SetRoutes
	RoutesDelta  = ribapi.RoutesDelta
	PrefixRoutes = ribapi.PrefixRoutes
	DeltaReply   = ribapi.DeltaReply
)

var (
	DefaultRoute = ribapi.DefaultRoute
)
