module github.com/thxrben/cerium-switchd/lib/lacp

go 1.27.1

require (
	github.com/thxrben/cerium-switchd/lib/sys v0.0.0-00010101000000-000000000000
	golang.org/x/sys v0.48.0
)

replace github.com/thxrben/cerium-switchd/lib/sys => ../sys
