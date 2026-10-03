// Package netdev reads the state of network devices (link, carrier,
// addresses) for the switch's programs. It only reads; switchd is the one
// program that changes devices (reference 1.9).
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
