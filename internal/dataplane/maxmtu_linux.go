//go:build linux

package dataplane

import (
	"encoding/binary"
	"strings"

	"github.com/thxrben/cerium-switchd/pkg/nlx"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// maxMTUs returns IFLA_MAX_MTU per link name (the netlink library does not
// expose it). Links without the attribute are left out.
func maxMTUs() map[string]int {
	out := map[string]int{}
	req := nl.NewNetlinkRequest(unix.RTM_GETLINK, unix.NLM_F_DUMP)
	req.AddData(nl.NewIfInfomsg(unix.AF_UNSPEC))
	msgs, err := nlx.Execute(req, unix.NETLINK_ROUTE, unix.RTM_NEWLINK)
	if err != nil {
		return out
	}
	for _, m := range msgs {
		info := nl.DeserializeIfInfomsg(m)
		attrs, err := nl.ParseRouteAttr(m[info.Len():])
		if err != nil {
			continue
		}
		name, max := "", 0
		for _, a := range attrs {
			switch a.Attr.Type {
			case unix.IFLA_IFNAME:
				name = strings.TrimRight(string(a.Value), "\x00")
			case unix.IFLA_MAX_MTU:
				if len(a.Value) >= 4 {
					max = int(binary.NativeEndian.Uint32(a.Value))
				}
			}
		}
		if name != "" && max > 0 {
			out[name] = max
		}
	}
	return out
}

// StackPortMTU returns the kernel MTU for a stacking port: its NIC maximum,
// at most MaxStackPortMTU (0 if unknown).
func StackPortMTU(linux string) int {
	if m := maxMTUs()[linux]; m > 0 {
		return min(m, MaxStackPortMTU)
	}
	return 0
}
