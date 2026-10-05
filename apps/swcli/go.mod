module github.com/thxrben/cerium-switchd/apps/swcli

go 1.27.1

require (
	github.com/thxrben/cerium-switchd/lib/platform v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/sys v0.0.0-00010101000000-000000000000
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
)

replace github.com/thxrben/cerium-switchd/lib/sys => ../../lib/sys

replace github.com/thxrben/cerium-switchd/lib/platform => ../../lib/platform
