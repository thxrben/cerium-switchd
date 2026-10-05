module github.com/thxrben/cerium-switchd/apps/switchd

go 1.27.1

require (
	github.com/godbus/dbus/v5 v5.2.2
	github.com/hashicorp/go-hclog v1.6.3
	github.com/hashicorp/raft v1.8.0
	github.com/hashicorp/raft-boltdb/v2 v2.4.2
	github.com/thxrben/cerium-switchd/lib/bfd v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/bgp v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/conf v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/dhcp v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/lacp v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/lldp v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/macsec v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/ntp v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/ospf v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/platform v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/rib v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/software v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/sys v0.0.0-00010101000000-000000000000
	github.com/thxrben/cerium-switchd/lib/syslog v0.0.0-00010101000000-000000000000
	github.com/vishvananda/netlink v1.3.1
	golang.org/x/sys v0.48.0
)

require (
	github.com/boltdb/bolt v1.3.1 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/hashicorp/go-immutable-radix v1.3.1 // indirect
	github.com/hashicorp/go-metrics v0.7.0 // indirect
	github.com/hashicorp/go-msgpack/v2 v2.1.5 // indirect
	github.com/hashicorp/golang-lru v1.0.2 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/osrg/gobgp/v3 v3.37.0 // indirect
	github.com/thxrben/cerium-switchd/lib/rstp v0.0.0-00010101000000-000000000000 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.etcd.io/bbolt v1.4.1 // indirect
	golang.org/x/net v0.59.0 // indirect
)

replace github.com/thxrben/cerium-switchd/lib/sys => ../../lib/sys

replace github.com/thxrben/cerium-switchd/lib/platform => ../../lib/platform

replace github.com/thxrben/cerium-switchd/lib/conf => ../../lib/conf

replace github.com/thxrben/cerium-switchd/lib/software => ../../lib/software

replace github.com/thxrben/cerium-switchd/lib/lacp => ../../lib/lacp

replace github.com/thxrben/cerium-switchd/lib/rstp => ../../lib/rstp

replace github.com/thxrben/cerium-switchd/lib/lldp => ../../lib/lldp

replace github.com/thxrben/cerium-switchd/lib/bfd => ../../lib/bfd

replace github.com/thxrben/cerium-switchd/lib/ospf => ../../lib/ospf

replace github.com/thxrben/cerium-switchd/lib/bgp => ../../lib/bgp

replace github.com/thxrben/cerium-switchd/lib/rib => ../../lib/rib

replace github.com/thxrben/cerium-switchd/lib/dhcp => ../../lib/dhcp

replace github.com/thxrben/cerium-switchd/lib/ntp => ../../lib/ntp

replace github.com/thxrben/cerium-switchd/lib/syslog => ../../lib/syslog

replace github.com/thxrben/cerium-switchd/lib/macsec => ../../lib/macsec
