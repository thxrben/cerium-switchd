package dataplane

import (
	"maps"
	"slices"

	"github.com/thxrben/cerium-switchd/internal/model"
)

// MirrorPort is what one device of this member mirrors, and where to
// (reference 5.x, forwarding-options analyzer): output devices per kind of
// traffic.
type MirrorPort struct {
	Ingress  []string         // every received frame
	Untagged []string         // received untagged frames (access or native VLAN)
	Tagged   map[int][]string // received frames tagged with the VLAN
	Egress   []string         // every sent frame
}

func (p *MirrorPort) empty() bool {
	return len(p.Ingress)+len(p.Untagged)+len(p.Tagged)+len(p.Egress) == 0
}

func addOut(list []string, out string) []string {
	if slices.Contains(list, out) {
		return list
	}
	return slices.Sorted(slices.Values(append(list, out)))
}

// ComputeMirrors returns member m's mirror filters by kernel device: the
// analyzers whose output is on m (inputs elsewhere are refused at commit).
func ComputeMirrors(cfg *model.Config, m int, names PortNames) map[string]*MirrorPort {
	out := map[string]*MirrorPort{}
	dev := func(name string) (string, bool) {
		i := cfg.Interfaces[name]
		if i == nil || i.Parent != "" {
			return "", false
		}
		if i.AE {
			if !slices.Contains(i.MemberIDs, m) {
				return "", false
			}
			return name, true
		}
		if i.Member != m {
			return "", false
		}
		return names(name)
	}
	port := func(d string) *MirrorPort {
		if out[d] == nil {
			out[d] = &MirrorPort{Tagged: map[int][]string{}}
		}
		return out[d]
	}
	for _, an := range slices.Sorted(maps.Keys(cfg.Analyzers)) {
		a := cfg.Analyzers[an]
		to, ok := dev(a.Output)
		if !ok {
			continue
		}
		for _, n := range a.IngressIfs {
			if d, ok := dev(n); ok && d != to {
				p := port(d)
				p.Ingress = addOut(p.Ingress, to)
			}
		}
		for _, n := range a.EgressIfs {
			if d, ok := dev(n); ok && d != to {
				p := port(d)
				p.Egress = addOut(p.Egress, to)
			}
		}
		for _, v := range a.IngressVLANs {
			// Frames received in the VLAN on any switch port of this member.
			for _, n := range slices.Sorted(maps.Keys(cfg.Interfaces)) {
				i := cfg.Interfaces[n]
				if !i.Switching || i.Disabled || n == a.Output {
					continue
				}
				d, ok := dev(n)
				if !ok || d == to {
					continue
				}
				switch {
				case i.Mode == "trunk":
					if slices.Contains(i.VLANs, v) && i.NativeVLAN != v {
						p := port(d)
						p.Tagged[v] = addOut(p.Tagged[v], to)
					}
					if i.NativeVLAN == v {
						p := port(d)
						p.Untagged = addOut(p.Untagged, to)
					}
				case i.AccessVLAN == v:
					p := port(d)
					p.Untagged = addOut(p.Untagged, to)
				}
			}
		}
	}
	for d, p := range out {
		if len(p.Tagged) == 0 {
			p.Tagged = nil
		}
		if p.empty() {
			delete(out, d)
		}
	}
	return out
}
