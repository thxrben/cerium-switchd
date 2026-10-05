module github.com/thxrben/cerium-switchd/apps/cer-bgpd

go 1.27.1

require (
	github.com/thxrben/cerium-switchd/lib/bfd v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/bgp v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/conf v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/platform v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/rib v0.0.0-00010101000000-000000000000
	golang.org/x/sys v0.48.0
)

require (
	github.com/osrg/gobgp/v3 v3.37.0 // indirect
	github.com/thxrben/cerium-switchd/lib/lacp v0.0.0-00010101000000-000000000000 // indirect
	github.com/thxrben/cerium-switchd/lib/sys v0.0.0-00010101000000-000000000000 // indirect
	github.com/vishvananda/netlink v1.3.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	golang.org/x/net v0.59.0 // indirect
)

replace github.com/thxrben/cerium-switchd/lib/sys => ../../lib/sys

replace github.com/thxrben/cerium-switchd/lib/platform => ../../lib/platform

replace github.com/thxrben/cerium-switchd/lib/conf => ../../lib/conf

replace github.com/thxrben/cerium-switchd/lib/lacp => ../../lib/lacp

replace github.com/thxrben/cerium-switchd/lib/bfd => ../../lib/bfd

replace github.com/thxrben/cerium-switchd/lib/bgp => ../../lib/bgp

replace github.com/thxrben/cerium-switchd/lib/rib => ../../lib/rib
