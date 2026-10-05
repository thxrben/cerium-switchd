package daemon

import (
	"fmt"
	"slices"
	"sort"

	"github.com/thxrben/cerium-switchd/internal/api/mclagapi"
	"github.com/thxrben/cerium-switchd/internal/model"
)

// bundleFacts is what both members must agree on for an MC-LAG bundle
// (reference 5.6, consistency checks).
func bundleFacts(cfg *model.Config, name string) string {
	i := cfg.Interfaces[name]
	if i == nil {
		return ""
	}
	vlans := slices.Clone(i.VLANs)
	slices.Sort(vlans)
	mode := "static"
	if i.LACP != nil {
		mode = "passive"
		if i.LACP.Active {
			mode = "active"
		}
		if i.LACP.Fast {
			mode += ",fast"
		} else {
			mode += ",slow"
		}
	}
	sw := "no switching"
	switch {
	case i.Switching && i.Mode == "trunk":
		sw = fmt.Sprintf("trunk vlans %v native %d", vlans, i.NativeVLAN)
	case i.Switching:
		sw = fmt.Sprintf("access vlan %d", i.AccessVLAN)
	}
	return fmt.Sprintf("%s, mtu %d, lacp %s", sw, i.MTU, mode)
}

// mclagConfig is cer-mclagd's configuration for member (reference 5.6).
func mclagConfig(cfg *model.Config, member int) mclagapi.Config {
	c := mclagapi.Config{Member: member, SwitchMembers: cfg.SwitchMembers(), DelayRestore: cfg.MCLAG.DelayRestore,
		Priority: map[int]int{}, Facts: map[string]string{}}
	for id, m := range cfg.Members {
		c.Priority[id] = m.Priority
	}
	if d := cfg.PairOf(member); d != nil {
		bundles := slices.Clone(d.Bundles)
		sort.Strings(bundles)
		c.Pair = &mclagapi.Pair{ID: d.ID, Members: d.Members, Bundles: bundles}
		for _, b := range bundles {
			c.Facts[b] = bundleFacts(cfg, b)
		}
	}
	return c
}
