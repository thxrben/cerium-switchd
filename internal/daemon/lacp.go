package daemon

import (
	"crypto/sha256"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/schema"
	"github.com/thxrben/cerium-switchd/pkg/lacp"
)

// lacpSystemMAC is the stack's LACP system id (reference 5.3.2): one for
// every bundle of every member, derived from the stack id, so it never
// changes with ports, restarts or mastership, and a bundle that gains ports
// on a second member (MC-LAG) keeps its identity. Without a stack id
// (dry-run) it is derived from the machine id.
func lacpSystemMAC(stackID string) [6]byte {
	seed := "stack\x00" + stackID
	if stackID == "" {
		id, err := os.ReadFile("/etc/machine-id")
		if err != nil || len(strings.TrimSpace(string(id))) == 0 {
			id, _ = os.ReadFile("/proc/sys/kernel/random/boot_id")
		}
		seed = "machine\x00" + strings.TrimSpace(string(id))
	}
	sum := sha256.Sum256([]byte("ceros lacp system\x00" + seed))
	var mac [6]byte
	copy(mac[:], sum[:6])
	mac[0] = mac[0]&^0x01 | 0x02 // unicast, locally administered
	return mac
}

// lacpPortNumber is unique in the stack (reference 5.1.3):
// member × 1024 + card × 64 + port.
func lacpPortNumber(name string) uint16 {
	p, ok := schema.ParsePhysical(name)
	if !ok {
		return 0
	}
	return uint16(p.Member*1024 + (p.Card%16)*64 + p.Port%64)
}

// lacpSpecs lists this member's LACP bundles.
func lacpSpecs(cfg *model.Config, member int, linux func(string) (string, bool), sysMAC [6]byte) []lacp.BundleSpec {
	var out []lacp.BundleSpec
	for _, i := range cfg.Interfaces {
		if !i.AE || i.LACP == nil || i.Disabled {
			continue
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(i.Name, "ae"))
		spec := lacp.BundleSpec{Name: i.Name, MinLinks: i.MinLinks, Config: lacp.Config{
			System: lacp.SystemID{Priority: uint16(i.LACP.SystemPriority), MAC: sysMAC},
			Key:    uint16(n + 1), Active: i.LACP.Active, Fast: i.LACP.Fast,
		}}
		for _, p := range cfg.Interfaces {
			if p.Parent != i.Name || p.Member != member || p.Disabled {
				continue
			}
			l, ok := linux(p.Name)
			if !ok {
				continue // not plugged in
			}
			spec.Ports = append(spec.Ports, lacp.PortSpec{Linux: l, Name: p.Name, Number: lacpPortNumber(p.Name), Priority: 32768})
		}
		if len(spec.Ports) > 0 {
			sort.Slice(spec.Ports, func(a, b int) bool { return spec.Ports[a].Number < spec.Ports[b].Number })
			out = append(out, spec)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}
