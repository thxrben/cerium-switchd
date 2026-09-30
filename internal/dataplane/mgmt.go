package dataplane

import (
	"net"
	"net/netip"
	"slices"

	"mclag/internal/model"
	"mclag/internal/schema"
)

// CMEName is the kernel name of the chassis management interface.
const CMEName = schema.CME

// CMEIf is the chassis management interface (reference 1.8): a device of
// its own on the master's active management port, with the stack-wide
// management MAC and the addresses of cme.0.
type CMEIf struct {
	Parent string // kernel name of the management port
	MAC    net.HardwareAddr
	Addrs  []netip.Prefix
	VRF    string
	Up     bool
}

// Management applies member m's management role to its desired state s
// (reference 1.8): the management instance's addresses and routes exist
// only on the master, and the master puts cme on the first of its
// management ports (by name) whose link is up. Management ports and the
// ports in unconfigured carry no addresses. carrier reports whether a
// kernel port has a link.
func Management(s *State, cfg *model.Config, m int, names PortNames, master bool, carrier func(linux string) bool, mac net.HardwareAddr, unconfigured []string) {
	if s.L3 == nil {
		s.L3 = &L3{}
	}
	l := s.L3
	mi := cfg.System.MgmtInstance
	if !master && mi != "" {
		l.Ifs = slices.DeleteFunc(l.Ifs, func(i L3If) bool { return i.VRF == mi })
		l.Routes = slices.DeleteFunc(l.Routes, func(r Route) bool { return r.VRF == mi })
	}
	var ports []string
	for _, p := range cfg.MgmtPorts(m) {
		if linux, ok := names(p); ok {
			ports = append(ports, linux)
			u := cfg.L3[CMEName+".0"]
			if !master || l.CME != nil || u == nil || mi == "" || u.Instance != mi || cfg.Interfaces[p].Disabled || !carrier(linux) {
				continue
			}
			l.CME = &CMEIf{Parent: linux, MAC: slices.Clone(mac), Addrs: slices.Clone(u.Addrs), VRF: mi, Up: !u.Disabled}
		}
	}
	l.Bare = append(ports, unconfigured...)
	slices.Sort(l.Bare)
}

// CMEMAC derives the stack-wide MAC address of cme from the stack id.
func CMEMAC(stackID string) net.HardwareAddr {
	return derivedMAC("ceros cme mac\x00" + stackID)
}
