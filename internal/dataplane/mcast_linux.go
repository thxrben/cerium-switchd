//go:build linux

package dataplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thxrben/cerium-switchd/pkg/netdev"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// The bridge's multicast options are set with iproute2: the netlink library
// does not cover the per-VLAN multicast contexts.

func runTool(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

func b2i(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// SyncMulticast converges IGMP/MLD snooping (reference 5.5). Router ports
// and VLAN settings come first, so that switching snooping on never cuts
// group traffic to another member; without a querier the bridge floods.
func (k *Netlink) SyncMulticast(m *Multicast) (bool, error) {
	if m == nil {
		return false, nil
	}
	changed := false
	var errs []error
	set := func(name string, args ...string) {
		if _, err := runTool(name, args...); err != nil {
			errs = append(errs, err)
			return
		}
		changed = true
	}

	// Per port and VLAN: the router ports.
	raw, err := runTool("bridge", "-d", "-j", "vlan", "show")
	if err != nil {
		return false, err
	}
	var ports []struct {
		Ifname string `json:"ifname"`
		VLANs  []struct {
			VLAN   int  `json:"vlan"`
			Router *int `json:"mcast_router"`
		} `json:"vlans"`
	}
	if err := json.Unmarshal(raw, &ports); err != nil {
		return false, fmt.Errorf("bridge vlan show: %w", err)
	}
	for _, p := range ports {
		if p.Ifname == BridgeName {
			continue
		}
		want := 1 // learn from queries
		if m.Router[p.Ifname] {
			want = 2 // permanent
		}
		for _, v := range p.VLANs {
			if v.Router != nil && *v.Router != want {
				set("bridge", "vlan", "set", "dev", p.Ifname, "vid", strconv.Itoa(v.VLAN), "mcast_router", strconv.Itoa(want))
			}
		}
	}

	// Per port: immediate leave.
	raw, err = runTool("bridge", "-d", "-j", "link", "show")
	if err == nil {
		var links []struct {
			Ifname    string `json:"ifname"`
			Master    string `json:"master"`
			FastLeave *bool  `json:"fastleave"`
		}
		if json.Unmarshal(raw, &links) == nil {
			for _, l := range links {
				if l.Master != BridgeName || l.FastLeave == nil || *l.FastLeave == m.FastLeave[l.Ifname] {
					continue
				}
				on := "off"
				if m.FastLeave[l.Ifname] {
					on = "on"
				}
				set("bridge", "link", "set", "dev", l.Ifname, "fastleave", on)
			}
		}
	}

	// The bridge: snooping per VLAN (both switches on while any VLAN
	// snoops; the per-VLAN switch decides).
	raw, err = runTool("ip", "-d", "-j", "link", "show", "dev", BridgeName)
	if err != nil {
		return changed, errors.Join(append(errs, err)...)
	}
	var br []struct {
		Info struct {
			Data struct {
				Snooping     int `json:"mcast_snooping"`
				VLANSnooping int `json:"mcast_vlan_snooping"`
				Querier      int `json:"mcast_querier"`
			} `json:"info_data"`
		} `json:"linkinfo"`
	}
	if json.Unmarshal(raw, &br) != nil || len(br) != 1 {
		return changed, errors.Join(append(errs, fmt.Errorf("ip link show %s: unexpected output", BridgeName))...)
	}
	d := br[0].Info.Data
	if want := b2i(m.On); strconv.Itoa(d.Snooping) != want || strconv.Itoa(d.VLANSnooping) != want || d.Querier != 0 {
		// The VLAN settings below exist once VLAN snooping is on.
		set("ip", "link", "set", "dev", BridgeName, "type", "bridge", "mcast_vlan_snooping", want, "mcast_snooping", want, "mcast_querier", "0")
	}
	if !m.On {
		return changed, errors.Join(errs...)
	}
	raw, err = runTool("bridge", "-j", "vlan", "global", "show", "dev", BridgeName)
	if err != nil {
		return changed, errors.Join(append(errs, err)...)
	}
	var gv []struct {
		VLANs []struct {
			VLAN     int `json:"vlan"`
			VLANEnd  int `json:"vlanEnd"`
			Snooping int `json:"mcast_snooping"`
			Querier  int `json:"mcast_querier"`
			IGMP     int `json:"mcast_igmp_version"`
			MLD      int `json:"mcast_mld_version"`
		} `json:"vlans"`
	}
	if err := json.Unmarshal(raw, &gv); err != nil {
		return changed, errors.Join(append(errs, fmt.Errorf("bridge vlan global show: %w", err))...)
	}
	have := map[uint16]McastVLAN{}
	for _, g := range gv {
		for _, v := range g.VLANs {
			for id := v.VLAN; id <= max(v.VLAN, v.VLANEnd); id++ {
				have[uint16(id)] = McastVLAN{Snooping: v.Snooping == 1, Querier: v.Querier == 1, IGMPVersion: v.IGMP, MLDVersion: v.MLD}
			}
		}
	}
	for _, vid := range slices.Sorted(maps.Keys(m.VLANs)) {
		w, h := m.VLANs[vid], have[vid]
		if _, ok := have[vid]; !ok {
			continue // not on the bridge (yet)
		}
		w.Querier = w.Querier && w.Snooping
		if w == h {
			continue
		}
		set("bridge", "vlan", "global", "set", "dev", BridgeName, "vid", strconv.Itoa(int(vid)),
			"mcast_snooping", b2i(w.Snooping), "mcast_querier", b2i(w.Querier),
			"mcast_igmp_version", strconv.Itoa(w.IGMPVersion), "mcast_mld_version", strconv.Itoa(w.MLDVersion))
	}
	// MLD queries need a link-local source on the bridge.
	if m.Querier() {
		if c, err := writeSysctl("/proc/sys/net/ipv6/conf/"+BridgeName+"/disable_ipv6", "0"); err == nil && c {
			changed = true
		}
	}
	return changed, errors.Join(errs...)
}

// McastEntry is one group membership in the bridge (mdb).
type McastEntry = netdev.McastEntry

// McastRouterPort is a multicast-router port of a VLAN.
type McastRouterPort = netdev.McastRouterPort

// McastGroups reads the switch bridge's group memberships and router ports.
func McastGroups() ([]McastEntry, []McastRouterPort, error) { return netdev.McastGroups(BridgeName) }

// McastRefresh refreshes a learned membership (netdev.McastRefresh).
func McastRefresh(port string, vid int, group string) error {
	return netdev.McastRefresh(BridgeName, port, vid, group)
}
