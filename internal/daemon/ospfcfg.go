package daemon

import (
	"net/netip"
	"slices"

	"github.com/thxrben/cerium-switchd/internal/dataplane"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/ospfd"
	"github.com/thxrben/cerium-switchd/pkg/ospf"
)

// ospfConfig is cer-ospfd's configuration (reference 5.13): every OSPF and
// OSPFv3 of every instance with the units' kernel devices on this member
// and their stack-wide addresses (an address of one member is not OSPF's:
// OSPF runs once for the stack). cer-ospfd reads the rest (interface
// index, link-local address, MTU, carrier, speed) from the kernel.
func ospfConfig(cfg *model.Config, names dataplane.PortNames) ospfd.Config {
	c := ospfd.Config{Policies: cfg.Policies}
	for _, r := range cfg.AllRouting() {
		rid := ospf.IDFrom(cfg.RouterID(r))
		for _, o := range []*model.OSPF{r.OSPF, r.OSPF3} {
			if o == nil || o.Disabled {
				continue
			}
			in := ospfd.Instance{Name: r.Instance, VRF: r.Instance, Version: ospf.V2, RouterID: rid, Export: o.Export,
				ReferenceBW: o.ReferenceBW, Overload: o.Overload, OverloadTimeout: o.OverloadTimeout,
				GracefulRestart: o.GracefulRestart, RestartDuration: o.RestartDuration}
			if o.V3 {
				in.Version = ospf.V3
			}
			areas := make([]netip.Addr, 0, len(o.Areas))
			for a := range o.Areas {
				areas = append(areas, a)
			}
			slices.SortFunc(areas, netip.Addr.Compare)
			for _, aid := range areas {
				a := o.Areas[aid]
				units := make([]string, 0, len(a.Interfaces))
				for u := range a.Interfaces {
					units = append(units, u)
				}
				slices.Sort(units)
				for _, unit := range units {
					oi := a.Interfaces[unit]
					u := cfg.L3[unit]
					if u == nil || u.Disabled || u.Instance != r.Instance {
						continue
					}
					ic := ospfd.Iface{Unit: unit, Area: ospf.IDFrom(aid), P2P: oi.P2P, Passive: oi.Passive, Metric: oi.Metric,
						Priority: oi.Priority, Hello: oi.Hello, Dead: oi.Dead, Retransmit: oi.Retransmit,
						TransitDelay: oi.TransitDelay, Simple: oi.SimplePass, MD5: oi.MD5}
					if dev, here := dataplane.UnitDevice(cfg, unit, names); here {
						ic.Device = dev
					}
					ic.Owners, ic.IRB = unitOwners(cfg, u)
					if b := oi.BFD; b != nil {
						ic.BFD = &ospfd.BFDSpec{IntervalMs: b.IntervalMs, Multiplier: b.Multiplier, AuthType: b.AuthAlg,
							AuthKeyID: b.AuthKeyID, AuthKey: b.AuthKey}
					}
					for _, p := range u.Addrs {
						if _, perMember := u.AddrMember[p]; perMember || p.Addr().Is4() == o.V3 {
							continue
						}
						ic.Prefixes = append(ic.Prefixes, p)
					}
					in.Interfaces = append(in.Interfaces, ic)
				}
			}
			c.Instances = append(c.Instances, in)
		}
	}
	return c
}

// unitOwners returns the members that have a unit's device: a routed
// port's member, a bundle's leg members; an irb is on every member.
func unitOwners(cfg *model.Config, u *model.L3Unit) ([]int, bool) {
	switch {
	case u.IRB():
		return cfg.SwitchMembers(), true
	case u.Member != 0:
		return []int{u.Member}, false
	}
	if i, ok := cfg.Interfaces[u.Parent]; ok && i.AE {
		return slices.Clone(i.MemberIDs), false
	}
	return nil, false
}
