package daemon

import (
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/inventory"
	"github.com/thxrben/cerium-switchd/lib/conf/model"
	"github.com/thxrben/cerium-switchd/lib/lldp"
	"github.com/thxrben/cerium-switchd/lib/platform/version"
)

// lldpConfig returns what LLDP announces and the ports it runs on for
// member (reference 5.5 protocols lldp): the stack presents itself as one
// system (chassis MAC and chassis name are the same on every member).
func lldpConfig(cfg *model.Config, member int, names *inventory.Naming, chassisMAC net.HardwareAddr, stackPort func(string) bool) (lldp.System, []lldp.PortSpec) {
	l := cfg.LLDP
	sys := lldp.System{ChassisMAC: chassisMAC, Name: cfg.ChassisName(member), Desc: "cerOS " + version.Version, Caps: lldp.CapBridge | lldp.CapRouter}
	if l == nil {
		return sys, nil
	}
	sys.Interval, sys.Hold = l.Interval, l.Hold
	for _, i := range cfg.Interfaces {
		if i.Switching || i.Parent != "" {
			sys.Enabled |= lldp.CapBridge
		}
	}
	for _, u := range cfg.L3 {
		if u.Instance == "" || u.Instance != cfg.System.MgmtInstance {
			sys.Enabled |= lldp.CapRouter
		}
	}
	if u := cfg.L3["cme.0"]; u != nil {
		for _, p := range u.Addrs {
			sys.Mgmt = append(sys.Mgmt, p.Addr())
		}
	}
	slices.SortFunc(sys.Mgmt, func(a, b netip.Addr) int { return a.Compare(b) })
	var ports []lldp.PortSpec
	for _, i := range cfg.Interfaces {
		if i.AE || i.Member != member || i.Disabled || !l.Runs(i.Name, i.Parent) {
			continue
		}
		linux, ok := names.Linux(i.Name)
		if !ok || (stackPort != nil && stackPort(linux)) {
			continue
		}
		p := lldp.PortSpec{Linux: linux, Name: i.Name, Desc: i.Description, MaxFrame: uint16(min(i.MTU, 65535))}
		sw := i // the switching settings: a bundle member's are its ae's
		if ae := cfg.Interfaces[i.Parent]; ae != nil {
			sw = ae
		}
		switch {
		case sw.AccessVLAN != 0:
			p.PVID = uint16(sw.AccessVLAN)
		case sw.NativeVLAN != 0:
			p.PVID = uint16(sw.NativeVLAN)
		}
		if i.Parent != "" {
			p.Bundle = i.Parent
			if ae := cfg.Interfaces[i.Parent]; ae != nil {
				p.MaxFrame = uint16(min(ae.MTU, 65535))
				if ae.Description != "" && p.Desc == "" {
					p.Desc = i.Parent + ": " + ae.Description
				}
			}
			// Not the kernel's ifindex: it differs between the members of
			// an MC-LAG, which must look like one bundle.
			n, _ := strconv.Atoi(strings.TrimPrefix(i.Parent, "ae"))
			p.BundleIndex = uint32(n + 1)
		}
		ports = append(ports, p)
	}
	slices.SortFunc(ports, func(a, b lldp.PortSpec) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return sys, ports
}
