// Package software handles cerOS software (reference 3.6,
// docs/os-image.md): signed bundles, the boot state and the system slots,
// and fetching bundles.
package software

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
)

// Arch is the architecture name of this program (the bundles' naming).
func Arch() string { return runtime.GOARCH }

// Sum returns the SHA-256 of b in hex.
func Sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
