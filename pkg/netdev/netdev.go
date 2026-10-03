// Package netdev is the switch programs' access to network devices: their
// state (link, carrier) and low-level operations (team devices). Which
// program may change what follows the ownership table of reference 1.9:
// switchd creates devices, cer-lacpd enables team ports.
package netdev

import (
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Carrier reports whether a device's link is up (IFF_LOWER_UP).
func Carrier(name string) bool {
	ln, err := netlink.LinkByName(name)
	return err == nil && ln.Attrs().RawFlags&unix.IFF_LOWER_UP != 0
}

// Exists reports whether a device exists.
func Exists(name string) bool {
	_, err := netlink.LinkByName(name)
	return err == nil
}
