//go:build linux

package netdev

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// runTool runs a system tool and returns its output.
func runTool(name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
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

func parseTimer(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

// McastGroups reads a bridge's group memberships and router ports.
func McastGroups(bridge string) ([]McastEntry, []McastRouterPort, error) {
	raw, err := runTool("bridge", "-j", "-d", "-s", "mdb", "show", "dev", bridge)
	if err != nil {
		return nil, nil, err
	}
	var out []struct {
		MDB []struct {
			Port    string `json:"port"`
			Grp     string `json:"grp"`
			VID     int    `json:"vid"`
			State   string `json:"state"`
			Timer   string `json:"timer"`
			Mode    string `json:"filter_mode"`
			Sources []struct {
				Address string `json:"address"`
			} `json:"source_list"`
		} `json:"mdb"`
		// iproute2 writes the router ports as an object (by bridge) or a
		// list, depending on its version: walked generically.
		Router json.RawMessage `json:"router"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, fmt.Errorf("bridge mdb show: %w", err)
	}
	var es []McastEntry
	var rs []McastRouterPort
	for _, o := range out {
		for _, e := range o.MDB {
			if e.Port == bridge {
				continue // the bridge itself (irb receivers)
			}
			me := McastEntry{Port: e.Port, VID: e.VID, Group: e.Grp, Permanent: e.State == "permanent", Expires: parseTimer(e.Timer), Mode: e.Mode}
			for _, s := range e.Sources {
				me.Sources = append(me.Sources, s.Address)
			}
			es = append(es, me)
		}
		var any interface{}
		if len(o.Router) > 0 && json.Unmarshal(o.Router, &any) == nil {
			walkRouters(any, &rs)
		}
	}
	return es, rs, nil
}

// McastRefresh adds or refreshes a learned (temporary) membership of port
// in group: MC-LAG, the peer's groups on this member's leg (reference 5.5).
// It expires like a learned one when it is no longer refreshed, so a
// receiver behind this member's own leg is never cut by its removal.
func McastRefresh(bridge, port string, vid int, group string) error {
	_, err := runTool("bridge", "mdb", "replace", "dev", bridge, "port", port, "grp", group, "temp", "vid", strconv.Itoa(vid))
	return err
}

// walkRouters collects router port entries (objects with a "port") from
// iproute2's JSON, whatever their nesting.
func walkRouters(v interface{}, out *[]McastRouterPort) {
	switch x := v.(type) {
	case []interface{}:
		for _, e := range x {
			walkRouters(e, out)
		}
	case map[string]interface{}:
		if port, ok := x["port"].(string); ok {
			r := McastRouterPort{Port: port}
			if vid, ok := x["vid"].(float64); ok {
				r.VID = int(vid)
			}
			r.Permanent = x["type"] == "permanent"
			if t, ok := x["timer"].(string); ok {
				r.Expires = parseTimer(t)
			}
			*out = append(*out, r)
			return
		}
		for _, e := range x {
			walkRouters(e, out)
		}
	}
}

// FlushLearned removes the MAC addresses a bridge learned on dev (not
// static or externally installed ones) and returns how many.
func FlushLearned(dev string) (int, error) {
	l, err := netlink.LinkByName(dev)
	if err != nil {
		return 0, err
	}
	neighs, err := netlink.NeighList(l.Attrs().Index, unix.AF_BRIDGE)
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
		if err := netlink.NeighDel(&e); err == nil {
			n++
		}
	}
	return n, nil
}
