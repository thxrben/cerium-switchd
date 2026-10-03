package daemon

import (
	"net/netip"
	"slices"

	"github.com/thxrben/cerium-switchd/internal/dataplane"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/ribd"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

// ribConfig is cer-ribd's configuration for member (reference 5.8, 1.9):
// every routing instance with its units' devices on this member and the
// connected, static and DHCP routes. The management instance is only on
// the master (reference 1.8). leases are cer-dhcpcd's, by device.
func ribConfig(cfg *model.Config, member int, names dataplane.PortNames, master bool, leases map[string]dataplane.DHCPLease) ribd.Config {
	c := ribd.Config{Instances: map[string]ribd.Instance{}}
	insts := []string{""}
	for n := range cfg.Instances {
		if n == cfg.System.MgmtInstance && !master {
			continue
		}
		insts = append(insts, n)
	}
	slices.Sort(insts)
	units := slices.Sorted(func(yield func(string) bool) {
		for n := range cfg.L3 {
			if !yield(n) {
				return
			}
		}
	})
	for _, inst := range insts {
		in := ribd.Instance{VRF: inst, Devices: map[string]string{}}
		defaulted := false
		for _, name := range units {
			u := cfg.L3[name]
			if u.Instance != inst || u.Disabled {
				continue
			}
			dev, here := dataplane.UnitDevice(cfg, name, names)
			if here {
				in.Devices[name] = dev
			}
			// Connected routes: the stack-wide addresses (an address of one
			// member belongs to that member).
			for _, p := range u.Addrs {
				if _, perMember := u.AddrMember[p]; perMember {
					continue
				}
				nh := []rib.NextHop{{Interface: name}}
				in.Routes = append(in.Routes, rib.Route{Prefix: p.Masked(), Protocol: rib.Direct, Preference: rib.PrefDirect, NextHops: nh},
					rib.Route{Prefix: netip.PrefixFrom(p.Addr(), p.Addr().BitLen()), Protocol: rib.Local, Preference: rib.PrefLocal, NextHops: nh})
			}
			// A lease's subnet and default route (one per instance).
			if le, ok := leases[dev]; ok && here && u.DHCP {
				nh := []rib.NextHop{{Interface: name}}
				in.Routes = append(in.Routes, rib.Route{Prefix: le.Addr.Masked(), Protocol: rib.Direct, Preference: rib.PrefDirect, NextHops: nh})
				if le.Router.IsValid() && !defaulted {
					defaulted = true
					in.Routes = append(in.Routes, rib.Route{Prefix: ribd.DefaultRoute, Protocol: rib.DHCP, Preference: rib.PrefDHCP,
						NextHops: []rib.NextHop{{Gateway: le.Router, Interface: name}}})
				}
			}
		}
		routes := cfg.Routes
		if inst != "" {
			routes = cfg.Instances[inst].Routes
		}
		for _, r := range routes {
			rt := rib.Route{Prefix: r.Prefix, Protocol: rib.Static, Preference: r.Preference, Discard: r.Discard}
			if rt.Preference == 0 {
				rt.Preference = rib.PrefStatic
			}
			for _, h := range r.NextHops {
				rt.NextHops = append(rt.NextHops, rib.NextHop{Gateway: h, Interface: unitFor(cfg, inst, h)})
			}
			in.Routes = append(in.Routes, rt)
		}
		c.Instances[inst] = in
	}
	return c
}

// unitFor returns the unit of the instance whose subnet contains a ("":
// none, the kernel resolves the gateway).
func unitFor(cfg *model.Config, inst string, a netip.Addr) string {
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range cfg.L3 {
			if !yield(n) {
				return
			}
		}
	}) {
		u := cfg.L3[name]
		if u.Instance != inst {
			continue
		}
		for _, p := range u.Addrs {
			if p.Masked().Contains(a) {
				return name
			}
		}
	}
	return ""
}
