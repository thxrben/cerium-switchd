module github.com/thxrben/cerium-switchd/apps/cer-lacpd

go 1.27.1

require (
	github.com/thxrben/cerium-switchd/lib/lacp v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/platform v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/sys v0.0.0-00010101000000-000000000000
)

require (
	github.com/vishvananda/netlink v1.3.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/thxrben/cerium-switchd/lib/sys => ../../lib/sys

replace github.com/thxrben/cerium-switchd/lib/platform => ../../lib/platform

replace github.com/thxrben/cerium-switchd/lib/lacp => ../../lib/lacp
