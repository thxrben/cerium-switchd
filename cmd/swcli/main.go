// Command swcli is the CLI client and login shell (reference 3.1). It
// talks to switchd over its socket and needs nothing else of the switch.
package main

import (
	"os"

	"github.com/thxrben/cerium-switchd/internal/swcli"
)

func main() {
	os.Exit(swcli.Main(os.Args[1:]))
}
