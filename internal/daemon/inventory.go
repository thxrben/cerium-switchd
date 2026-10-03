package daemon

import (
	"github.com/thxrben/cerium-switchd/internal/inventory"
	"github.com/thxrben/cerium-switchd/internal/stack"

	"github.com/thxrben/cerium-switchd/internal/dataplane"
	"github.com/thxrben/cerium-switchd/internal/model"
)

// kernelInventory reports this member's ports from the kernel. Other
// members are unknown until stacking exists.
type kernelInventory struct {
	kernel dataplane.Kernel
	names  *inventory.Naming
	member int
	vc     *stack.Manager
}

func (k *kernelInventory) Ports(member int) (map[string]model.PortInfo, bool) {
	if member != k.member {
		return nil, false
	}
	st, err := k.kernel.Read()
	if err != nil {
		return nil, false
	}
	out := map[string]model.PortInfo{}
	for _, p := range k.names.Ports() {
		caps := inventory.ReadCaps("/sys", p.Linux)
		info := model.PortInfo{Linux: p.Linux, StackPort: k.vc != nil && k.vc.IsPort(p.Linux), MaxSpeedMbps: caps.MaxSpeedMbps,
			NoPause: caps.Pause == inventory.No, VlanChallenged: caps.Features["vlan-challenged"] == "on"}
		if l := st.Links[p.Linux]; l != nil {
			info.MTU, info.MaxMTU = l.MTU, l.MaxMTU
		}
		if info.StackPort {
			for _, sp := range k.vc.Ports() {
				if sp.Linux == p.Linux && sp.PathMTU > 0 {
					info.PathMTU = sp.PathMTU + model.EthHeader
				}
			}
		}
		out[p.Name] = info
	}
	return out, true
}

// hasIP reports whether a link has addresses other than IPv6 link-local.
