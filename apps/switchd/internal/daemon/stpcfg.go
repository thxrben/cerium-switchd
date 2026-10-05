package daemon

import (
	"slices"

	"github.com/thxrben/cerium-switchd/lib/conf/model"
	"github.com/thxrben/cerium-switchd/lib/platform/api/stpapi"
)

// stpConfig is cer-rstpd's configuration for member (reference 5.5): the
// stack's RSTP ports (switch ports that are not bundle members, and
// aggregated interfaces), with this member's devices.
func stpConfig(cfg *model.Config, member int, linux func(string) (string, bool), stackID string) stpapi.Config {
	c := stpapi.Config{Member: member, StackID: stackID, SwitchMembers: cfg.SwitchMembers(), Ports: map[string]stpapi.Port{}}
	// bpdu-block works with and without RSTP.
	for _, n := range cfg.BPDUBlock.Interfaces {
		i := cfg.Interfaces[n]
		if i == nil {
			continue
		}
		switch {
		case i.AE && slices.Contains(i.MemberIDs, member):
			c.BPDUBlock = setKey(c.BPDUBlock, n, n)
		case !i.AE && i.Member == member:
			if dev, ok := linux(n); ok {
				c.BPDUBlock = setKey(c.BPDUBlock, n, dev)
			}
		}
	}
	c.BPDUTimeout = cfg.BPDUBlock.DisableTimeout
	if cfg.RSTP == nil {
		return c
	}
	c.On = true
	c.Bridge = stpapi.BridgeConfig{BridgePriority: cfg.RSTP.BridgePriority, HelloTime: cfg.RSTP.HelloTime,
		MaxAge: cfg.RSTP.MaxAge, ForwardDelay: cfg.RSTP.ForwardDelay}
	for n, i := range cfg.Interfaces {
		if !i.Switching || i.Disabled || i.Parent != "" {
			continue
		}
		pc := cfg.RSTP.Ports[n]
		if pc != nil && pc.Disabled {
			continue
		}
		p := stpapi.Port{AE: i.AE, LACP: i.LACP != nil}
		if pc != nil {
			p.Config = &stpapi.PortConfig{Cost: pc.Cost, Priority: pc.Priority, Edge: pc.Edge, RootGuard: pc.RootGuard, PointToPnt: pc.PointToPnt}
		}
		switch {
		case i.AE:
			p.Members = append([]int(nil), i.MemberIDs...)
			for _, m := range cfg.Interfaces {
				if m.Parent == n && m.Member == member {
					if l, ok := linux(m.Name); ok {
						p.Legs = append(p.Legs, l)
					}
				}
			}
			for _, id := range i.MemberIDs {
				if id == member {
					p.Device = n
				}
			}
		case i.Member > 0:
			p.Members = []int{i.Member}
			if i.Member == member {
				p.Device, _ = linux(n)
			}
		default:
			continue
		}
		c.Ports[n] = p
	}
	return c
}

func setKey(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}
