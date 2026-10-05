module github.com/thxrben/cerium-switchd/apps/rtest

go 1.27.1

require (
	github.com/thxrben/cerium-switchd/lib/bfd v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/sys v0.0.0-00010101000000-000000000000
)

require (
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/thxrben/cerium-switchd/lib/sys => ../../lib/sys

replace github.com/thxrben/cerium-switchd/lib/bfd => ../../lib/bfd
