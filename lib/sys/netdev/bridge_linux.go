//go:build linux

package netdev

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/lib/sys/nlx"
	"github.com/thxrben/cerium-switchd/lib/sys/sysexec"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// runTool runs a system tool and returns its output.
func runTool(name string, args ...string) ([]byte, error) {
	out, err := sysexec.Output(name, args...)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// McastEntry is one group membership in the bridge (mdb).
type McastEntry struct {
	Port      string // kernel name
	VID       int
	Group     string
	Permanent bool
	Expires   float64  // seconds (learned entries)
	Mode      string   // include|exclude (IGMPv3/MLDv2), "" for v2/v1
	Sources   []string // source filter
}

// McastRouterPort is a multicast-router port of a VLAN.
type McastRouterPort struct {
	Port      string
	VID       int
	Permanent bool    // configured (or a tunnel); else learned from queries
	Expires   float64 // learned ones
}

// McastGroups reads a bridge's group memberships and router ports
// (RTM_GETMDB dump).
func McastGroups(bridge string) ([]McastEntry, []McastRouterPort, error) {
	br, err := nlx.LinkByName(bridge)
	if err != nil {
		return nil, nil, err
	}
	req := nl.NewNetlinkRequest(unix.RTM_GETMDB, unix.NLM_F_DUMP)
	// Every bridge (a bridge index in the request returns nothing on some
	// kernels); the replies are filtered below.
	req.AddData(&brPortMsg{})
	// The kernel answers a dump with RTM_GETMDB messages (not NEWMDB).
	msgs, err := nlx.Execute(req, unix.NETLINK_ROUTE, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("bridge multicast database: %w", err)
	}
	names := map[int]string{}
	name := func(idx int) string {
		if n, ok := names[idx]; ok {
			return n
		}
		n := strconv.Itoa(idx)
		if l, err := nlx.LinkByIndex(idx); err == nil {
			n = l.Attrs().Name
		}
		names[idx] = n
		return n
	}
	var es []McastEntry
	var rs []McastRouterPort
	for _, m := range msgs {
		idx, entries, routers, err := parseMDB(m)
		if err != nil {
			return nil, nil, fmt.Errorf("bridge multicast database: %w", err)
		}
		if idx != br.Attrs().Index {
			continue // another bridge
		}
		for _, e := range entries {
			if e.ifindex == idx {
				continue // the bridge itself (irb receivers)
			}
			es = append(es, McastEntry{Port: name(e.ifindex), VID: e.vid, Group: e.group, Permanent: e.permanent,
				Expires: e.expires, Mode: e.mode, Sources: e.sources})
		}
		for _, r := range routers {
			rs = append(rs, McastRouterPort{Port: name(r.ifindex), VID: r.vid, Permanent: r.permanent, Expires: r.expires})
		}
	}
	return es, rs, nil
}

// brPortMsg is struct br_port_msg (family, padding, bridge ifindex).
type brPortMsg struct{ ifindex uint32 }

func (m *brPortMsg) Len() int { return sizeofBrPortMsg }

func (m *brPortMsg) Serialize() []byte {
	b := make([]byte, sizeofBrPortMsg)
	b[0] = unix.AF_BRIDGE
	nl.NativeEndian().PutUint32(b[4:], m.ifindex)
	return b
}

// McastRefresh adds or refreshes a learned (temporary) membership of port
// in group: MC-LAG, the peer's groups on this member's leg (reference 5.5).
// It expires like a learned one when it is no longer refreshed, so a
// receiver behind this member's own leg is never cut by its removal.
func McastRefresh(bridge, port string, vid int, group string) error {
	_, err := runTool("bridge", "mdb", "replace", "dev", bridge, "port", port, "grp", group, "temp", "vid", strconv.Itoa(vid))
	return err
}

// FlushLearned removes the MAC addresses a bridge learned on dev (not
// static or externally installed ones) and returns how many.
func FlushLearned(dev string) (int, error) {
	l, err := nlx.LinkByName(dev)
	if err != nil {
		return 0, err
	}
	neighs, err := nlx.NeighList(l.Attrs().Index, unix.AF_BRIDGE)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range neighs {
		// Bridge entries name the bridge as master (dumps carry no
		// NTF_MASTER flag); learned ones are neither local, static nor
		// installed from outside.
		if e.MasterIndex == 0 || e.State&(unix.NUD_PERMANENT|unix.NUD_NOARP) != 0 || e.Flags&netlink.NTF_EXT_LEARNED != 0 {
			continue
		}
		e.Flags |= netlink.NTF_MASTER // delete from the bridge's table
		if err := nlx.NeighDel(&e); err == nil {
			n++
		}
	}
	return n, nil
}
