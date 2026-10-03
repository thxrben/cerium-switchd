// Command switchd-update is the update daemon (reference 3.6,
// docs/os-image.md §4): it installs software bundles, starts the new
// version and rolls back when it does not become healthy.
package main

import (
	"os"

	"github.com/thxrben/cerium-switchd/internal/updated"
)

func main() {
	os.Exit(updated.Main())
}
