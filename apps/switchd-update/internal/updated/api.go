package updated

import "github.com/thxrben/cerium-switchd/lib/platform/api/updapi"

// The API of this program is in package updapi (switchd and the other
// programs use it); here under the names this package always used.
type (
	Request = updapi.Request
	Reply   = updapi.Reply
	State   = updapi.State
)

const (
	DefaultSocket = updapi.DefaultSocket
	KeysDir       = updapi.KeysDir
)

var (
	ErrRejected = updapi.ErrRejected
)
