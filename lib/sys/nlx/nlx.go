// Package nlx is package netlink with deadlines (hwio): every request to
// the kernel returns within hwio.KernelDeadline, also when a NIC driver
// hangs with the kernel's network configuration lock (rtnl) held. Then the
// request fails, the lock is recorded as stuck, and further requests fail
// at once until it answers again.
package nlx

import (
	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

// Resource is the resource of netlink requests and ethtool ioctls: both
// wait for the kernel's network configuration lock.
const Resource = "rtnl"

// SetSocketTimeout bounds the netlink sockets themselves (a request whose
// answer never comes), so that hung calls do not remain after the kernel
// answers others again. Programs call it at start.
func SetSocketTimeout() { _ = netlink.SetSocketTimeout(hwio.KernelDeadline()) }

func desc(v ...any) string {
	if len(v) == 0 {
		return ""
	}
	switch x := v[0].(type) {
	case string:
		return " " + x
	case netlink.Link:
		if x != nil && x.Attrs() != nil {
			return " " + x.Attrs().Name
		}
	}
	return ""
}

// Execute runs a raw request (req.Execute) with the deadline.
func Execute(req *nl.NetlinkRequest, sockType int, resType uint16) ([][]byte, error) {
	return hwio.Do(Resource, "request", 0, func() ([][]byte, error) { return req.Execute(sockType, resType) })
}
