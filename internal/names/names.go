// Package names holds the kernel names of the switch's own devices, shared
// by switchd (which creates them) and the daemons (which use them,
// reference 1.9).
package names

import "fmt"

// Bridge is the switch bridge.
const Bridge = "swbr0"

// Tunnel is the kernel name of the stack tunnel to a member.
func Tunnel(member int) string { return fmt.Sprintf("swvc%d", member) }

// TunnelMember returns the member a stack tunnel leads to (0: not a tunnel).
func TunnelMember(name string) int {
	var m int
	if _, err := fmt.Sscanf(name, "swvc%d", &m); err != nil || Tunnel(m) != name {
		return 0
	}
	return m
}

// VXLAN is the kernel name of a member's VXLAN port of a VNI towards the
// remote VTEPs (reference 5.7).
func VXLAN(vni int) string { return fmt.Sprintf("swvx%d", vni) }

// VXLANVNI returns the VNI of a VXLAN port (0: not one).
func VXLANVNI(name string) int {
	var v int
	if _, err := fmt.Sscanf(name, "swvx%d", &v); err != nil || VXLAN(v) != name {
		return 0
	}
	return v
}

// StackTable is the routing table of the stack's internal instance (the
// stack tunnels' underlay, switchd's; cer-ribd leaves it alone).
const StackTable = 999
