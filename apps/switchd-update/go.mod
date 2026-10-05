module github.com/thxrben/cerium-switchd/apps/switchd-update

go 1.27.1

require (
	github.com/thxrben/cerium-switchd/lib/conf v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/platform v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/software v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/sys v0.0.0-00010101000000-000000000000
	golang.org/x/sys v0.48.0
)

replace github.com/thxrben/cerium-switchd/lib/sys => ../../lib/sys

replace github.com/thxrben/cerium-switchd/lib/platform => ../../lib/platform

replace github.com/thxrben/cerium-switchd/lib/conf => ../../lib/conf

replace github.com/thxrben/cerium-switchd/lib/software => ../../lib/software
