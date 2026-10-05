module github.com/thxrben/cerium-switchd/lib/platform

go 1.27.1

require (
	github.com/thxrben/cerium-switchd/lib/bfd v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/bgp v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/conf v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/lacp v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/ospf v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/rib v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/rstp v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/software v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/sys v0.0.0-00010101000000-000000000000
	golang.org/x/sys v0.48.0
)

require (
	github.com/osrg/gobgp/v3 v3.37.0 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/vishvananda/netlink v1.3.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	golang.org/x/net v0.59.0 // indirect
)

replace github.com/thxrben/cerium-switchd/lib/sys => ../sys

replace github.com/thxrben/cerium-switchd/lib/conf => ../conf

replace github.com/thxrben/cerium-switchd/lib/software => ../software

replace github.com/thxrben/cerium-switchd/lib/lacp => ../lacp

replace github.com/thxrben/cerium-switchd/lib/rstp => ../rstp

replace github.com/thxrben/cerium-switchd/lib/bfd => ../bfd

replace github.com/thxrben/cerium-switchd/lib/ospf => ../ospf

replace github.com/thxrben/cerium-switchd/lib/bgp => ../bgp

replace github.com/thxrben/cerium-switchd/lib/rib => ../rib
