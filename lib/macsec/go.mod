module github.com/thxrben/cerium-switchd/lib/macsec

go 1.27.1

require (
	github.com/thxrben/cerium-switchd/lib/sys v0.0.0-00010101000000-000000000000
	github.com/vishvananda/netlink v1.3.1
	golang.org/x/sys v0.48.0
)

require github.com/vishvananda/netns v0.0.5 // indirect

replace github.com/thxrben/cerium-switchd/lib/sys => ../sys
