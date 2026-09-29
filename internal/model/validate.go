package model

import (
	"fmt"
	"sort"
	"strings"
)

// Severity of a validation issue.
type Severity int

const (
	Warning Severity = iota
	Error
)

// Issue is one validation finding.
type Issue struct {
	Severity Severity
	Path     string
	Msg      string
}

func (i Issue) String() string {
	sev := "warning"
	if i.Severity == Error {
		sev = "error"
	}
	if i.Path == "" {
		return fmt.Sprintf("%s: %s", sev, i.Msg)
	}
	return fmt.Sprintf("[edit %s]\n  %s: %s", i.Path, sev, i.Msg)
}

// Issues is a list of findings.
type Issues []Issue

// HasErrors reports whether any issue blocks a commit.
func (is Issues) HasErrors() bool {
	for _, i := range is {
		if i.Severity == Error {
			return true
		}
	}
	return false
}

// String renders all issues.
func (is Issues) String() string {
	var b strings.Builder
	for _, i := range is {
		b.WriteString(i.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// PortInfo holds hardware facts about a physical port.
type PortInfo struct {
	MaxMTU int // 0 = unknown
}

// Inventory supplies hardware facts for validation.
type Inventory interface {
	// Port returns information about a port on a member. ok is false if
	// the member is known but the port does not exist. known is false if
	// nothing is known about the member (e.g. not joined yet).
	Port(member int, linux string) (info PortInfo, ok bool, known bool)
}

func (b *builder) validate() {
	c := b.cfg
	b.validateMembers()
	b.validateInterfaces()
	b.validateMTU()
	b.validateDomains()
	b.validateVXLAN()
	b.validateAnalyzers()
	b.validateRSTP()

	for _, u := range sortedKeys(c.System.Users) {
		usr := c.System.Users[u]
		if usr.PasswordHash == "" && len(usr.SSHKeys) == 0 {
			b.warnf("system login user "+u, "no authentication configured; the user cannot log in")
		}
	}
}

func (b *builder) validateMembers() {
	c := b.cfg
	names := map[string]int{}
	for _, id := range sortedKeys(c.Members) {
		m := c.Members[id]
		path := fmt.Sprintf("stack member %d", id)
		if m.HostName != "" {
			if o, dup := names[m.HostName]; dup {
				b.errorf(path+" host-name", "host-name %q is already used by member %d", m.HostName, o)
			}
			names[m.HostName] = id
		}
		if m.Mgmt.Gateway != "" && m.Mgmt.Address == "" && !m.Mgmt.DHCP {
			b.warnf(path+" management", "gateway without address")
		}
		if m.Mgmt.Interface != "" && m.Mgmt.VLAN != 0 {
			b.errorf(path+" management", "use either a management interface or a management vlan, not both")
		}
		if m.Mgmt.VLAN != 0 {
			if _, ok := c.VLANByID[m.Mgmt.VLAN]; !ok {
				b.errorf(path+" management vlan", "vlan-id %d is not defined under 'vlans'", m.Mgmt.VLAN)
			}
		}
		if m.Mgmt.Interface != "" {
			if i, ok := c.Interfaces[fmt.Sprintf("%d/%s", id, m.Mgmt.Interface)]; ok && (i.Switching || i.Parent != "") {
				b.errorf(path+" management interface", "%s is used as a switch port and cannot be the management interface", i.Name)
			}
		}
		if m.Underlay.Interface != "" {
			if i, ok := c.Interfaces[fmt.Sprintf("%d/%s", id, m.Underlay.Interface)]; ok && (i.Switching || i.Parent != "") {
				b.errorf(path+" underlay interface", "%s is used as a switch port and cannot be the underlay interface", i.Name)
			}
			if m.Underlay.Interface == m.Mgmt.Interface {
				b.warnf(path+" underlay interface", "underlay shares the management interface")
			}
		}
	}
}

func (b *builder) validateInterfaces() {
	c := b.cfg
	// Attach member ports to their aggregated interfaces.
	for _, name := range sortedKeys(c.Interfaces) {
		i := c.Interfaces[name]
		path := "interfaces " + name
		if i.AE {
			if i.Parent != "" || i.FlowControl != nil {
				b.errorf(path+" ether-options", "ether-options are only valid on physical ports")
			}
			continue
		}
		if i.LACP != nil || i.MCLAGID != 0 {
			b.errorf(path+" aggregated-ether-options", "aggregated-ether-options are only valid on ae interfaces")
		}
		m, ok := c.Members[i.Member]
		if !ok {
			b.errorf(path, "stack member %d is not configured", i.Member)
		} else if m.Witness {
			b.errorf(path, "member %d is a witness and has no switch ports", i.Member)
		}
		if b.inv != nil {
			if _, ok, known := b.inv.Port(i.Member, i.Linux); known && !ok {
				b.warnf(path, "port %s does not exist on member %d (configuration applies once it appears)", i.Linux, i.Member)
			}
		}
		if i.Parent == "" {
			continue
		}
		ae, ok := c.Interfaces[i.Parent]
		if !ok {
			b.errorf(path+" ether-options 802.3ad", "%s is not configured under 'interfaces'", i.Parent)
			continue
		}
		if i.Switching {
			b.errorf(path, "a member of %s cannot have 'unit 0 family ethernet-switching'", i.Parent)
		}
		if i.StormControl != (StormControl{}) || i.MACLimit != 0 {
			b.errorf(path, "storm-control and mac-limit must be configured on %s, not on its member ports", i.Parent)
		}
		ae.MemberPorts = append(ae.MemberPorts, name)
	}

	// Aggregated interface checks.
	for _, name := range sortedKeys(c.Interfaces) {
		i := c.Interfaces[name]
		if !i.AE {
			continue
		}
		path := "interfaces " + name
		members := map[int]bool{}
		for _, p := range i.MemberPorts {
			members[c.Interfaces[p].Member] = true
		}
		i.MemberIDs = sortedKeys(members)
		if len(i.MemberPorts) == 0 {
			b.warnf(path, "aggregated interface has no member ports")
		}
		if len(i.MemberPorts) > 0 && i.MinLinks > len(i.MemberPorts) {
			b.warnf(path+" aggregated-ether-options minimum-links", "minimum-links %d exceeds the %d member ports; the bundle can never come up", i.MinLinks, len(i.MemberPorts))
		}
		isPeerLink := false
		for _, d := range c.Domains {
			if d.PeerLink == name {
				isPeerLink = true
			}
		}
		if i.MCLAGID != 0 {
			if i.LACP == nil {
				b.errorf(path+" aggregated-ether-options", "MC-LAG interfaces require 'lacp'")
			}
			if isPeerLink {
				b.errorf(path+" aggregated-ether-options mclag", "the peer-link cannot be an MC-LAG interface")
			}
			if b.domainFor(i.MemberIDs) == nil && len(i.MemberIDs) > 0 {
				b.errorf(path+" aggregated-ether-options mclag", "no mclag domain contains member(s) %s", joinInts(i.MemberIDs))
			}
		}
		switch {
		case len(i.MemberIDs) > 2:
			b.errorf(path, "ports on %d stack members (%s); at most two are possible (MC-LAG)", len(i.MemberIDs), joinInts(i.MemberIDs))
		case len(i.MemberIDs) == 2 && i.MCLAGID == 0 && !isPeerLink:
			b.errorf(path, "ports on members %s require 'aggregated-ether-options mclag id' or use as a peer-link", joinInts(i.MemberIDs))
		}
		if isPeerLink && i.Switching {
			b.warnf(path, "the peer-link carries all VLANs automatically; its ethernet-switching settings are ignored")
		}
	}

	// MC-LAG ids must be unique per domain.
	seen := map[[2]int]string{}
	for _, name := range sortedKeys(c.Interfaces) {
		i := c.Interfaces[name]
		if i.MCLAGID == 0 {
			continue
		}
		d := b.domainFor(i.MemberIDs)
		if d == nil {
			continue
		}
		k := [2]int{d.ID, i.MCLAGID}
		if o, dup := seen[k]; dup {
			b.errorf("interfaces "+name+" aggregated-ether-options mclag id", "mclag id %d is already used by %s", i.MCLAGID, o)
		}
		seen[k] = name
	}
}

// domainFor returns the MC-LAG domain whose members include all ids.
func (b *builder) domainFor(ids []int) *Domain {
	for _, did := range sortedKeys(b.cfg.Domains) {
		d := b.cfg.Domains[did]
		all := true
		for _, id := range ids {
			found := false
			for _, m := range d.Members {
				if m == id {
					found = true
				}
			}
			all = all && found
		}
		if all {
			return d
		}
	}
	return nil
}

func (b *builder) validateMTU() {
	c := b.cfg
	for _, name := range sortedKeys(c.Interfaces) {
		i := c.Interfaces[name]
		path := "interfaces " + name
		// Member ports inherit the MTU of their bundle.
		if i.Parent != "" {
			if ae, ok := c.Interfaces[i.Parent]; ok {
				if i.MTU != 1500 && i.MTU != ae.MTU {
					b.warnf(path+" mtu", "member ports use the MTU of %s (%d); this setting is ignored", i.Parent, ae.MTU)
				}
				i.MTU = ae.MTU
			}
		}
	}
	for _, name := range sortedKeys(c.Interfaces) {
		i := c.Interfaces[name]
		path := "interfaces " + name
		if b.inv != nil && !i.AE {
			if info, ok, known := b.inv.Port(i.Member, i.Linux); known && ok && info.MaxMTU > 0 && i.MTU > info.MaxMTU {
				b.errorf(path+" mtu", "MTU %d exceeds the hardware maximum of %d on %s", i.MTU, info.MaxMTU, i.Linux)
			}
		}
		for _, vid := range i.VLANs {
			v := c.VLANByID[vid]
			if v != nil && v.MTU > i.MTU {
				b.warnf(path+" mtu", "vlan %s allows MTU %d but this port only %d; larger frames are dropped here", v.Name, v.MTU, i.MTU)
			}
		}
	}
	// The peer-link must carry everything its domain's MC-LAG ports carry.
	for _, did := range sortedKeys(c.Domains) {
		d := c.Domains[did]
		pl, ok := c.Interfaces[d.PeerLink]
		if !ok {
			continue
		}
		for _, name := range sortedKeys(c.Interfaces) {
			i := c.Interfaces[name]
			if i.MCLAGID != 0 && b.domainFor(i.MemberIDs) == d && i.MTU > pl.MTU {
				b.errorf("interfaces "+d.PeerLink+" mtu", "peer-link MTU %d is smaller than MTU %d of MC-LAG interface %s", pl.MTU, i.MTU, name)
			}
		}
	}
}

func (b *builder) validateDomains() {
	c := b.cfg
	inDomain := map[int]int{}
	for _, did := range sortedKeys(c.Domains) {
		d := c.Domains[did]
		path := fmt.Sprintf("mclag domain %d", did)
		if len(d.Members) != 2 {
			b.errorf(path+" members", "an MC-LAG domain needs exactly two members, got %d", len(d.Members))
		}
		for _, m := range d.Members {
			mem, ok := c.Members[m]
			if !ok {
				b.errorf(path+" members", "stack member %d is not configured", m)
				continue
			}
			if mem.Witness {
				b.errorf(path+" members", "member %d is a witness", m)
			}
			if o, dup := inDomain[m]; dup {
				b.errorf(path+" members", "member %d is already part of domain %d", m, o)
			}
			inDomain[m] = did
			if mem.Mgmt.Address == "" && !mem.Mgmt.DHCP {
				b.warnf(path, "member %d has no management address; split-brain detection via keepalive is not possible", m)
			}
		}
		if d.PeerLink == "" {
			b.errorf(path, "peer-link is required")
			continue
		}
		pl, ok := c.Interfaces[d.PeerLink]
		if !ok {
			b.errorf(path+" peer-link", "%s is not configured under 'interfaces'", d.PeerLink)
			continue
		}
		if len(d.Members) == 2 {
			want := []int{min(d.Members[0], d.Members[1]), max(d.Members[0], d.Members[1])}
			if len(pl.MemberIDs) != 2 || pl.MemberIDs[0] != want[0] || pl.MemberIDs[1] != want[1] {
				b.errorf(path+" peer-link", "%s must have ports on both members %s", d.PeerLink, joinInts(want))
			}
		}
	}
}

func (b *builder) validateVXLAN() {
	c := b.cfg
	vnis := map[int]string{}
	used := false
	for _, name := range sortedKeys(c.VLANs) {
		v := c.VLANs[name]
		if v.VNI == 0 {
			continue
		}
		used = true
		if o, dup := vnis[v.VNI]; dup {
			b.errorf("vlans "+name+" vxlan vni", "vni %d is already used by vlan %s", v.VNI, o)
		}
		vnis[v.VNI] = name
	}
	if !used {
		return
	}
	for _, id := range sortedKeys(c.Members) {
		m := c.Members[id]
		if !m.Witness && m.VTEPAddress == "" {
			b.errorf(fmt.Sprintf("stack member %d", id), "vtep-address is required when VLANs are extended over VXLAN")
		}
	}
	for vtep, list := range c.Switch.RemoteVTEPs {
		for _, vni := range list {
			if _, ok := vnis[vni]; !ok {
				b.errorf("switch-options vxlan remote-vtep "+vtep, "vni %d is not mapped to any VLAN", vni)
			}
		}
	}
}

func (b *builder) validateAnalyzers() {
	c := b.cfg
	for _, name := range sortedKeys(c.Analyzers) {
		a := c.Analyzers[name]
		path := "forwarding-options analyzer " + name
		if a.Output == "" {
			b.errorf(path, "output interface is required")
		}
		if len(a.IngressIfs)+len(a.EgressIfs)+len(a.IngressVLANs) == 0 {
			b.errorf(path, "at least one input is required")
		}
		members := map[int]bool{}
		check := func(ifname, where string) {
			i, ok := c.Interfaces[ifname]
			if !ok {
				b.errorf(path+" "+where, "%s is not configured under 'interfaces'", ifname)
				return
			}
			if i.Parent != "" {
				b.errorf(path+" "+where, "%s is a member of %s; mirror the aggregated interface instead", ifname, i.Parent)
			}
			if i.AE {
				for _, m := range i.MemberIDs {
					members[m] = true
				}
			} else {
				members[i.Member] = true
			}
		}
		for _, ifn := range a.IngressIfs {
			check(ifn, "input ingress interface")
			if ifn == a.Output {
				b.errorf(path, "%s cannot be both input and output", ifn)
			}
		}
		for _, ifn := range a.EgressIfs {
			check(ifn, "input egress interface")
			if ifn == a.Output {
				b.errorf(path, "%s cannot be both input and output", ifn)
			}
		}
		if a.Output != "" {
			check(a.Output, "output interface")
			if o, ok := c.Interfaces[a.Output]; ok && o.Switching {
				b.warnf(path+" output", "%s also carries switched traffic; mirrored frames are mixed into it", a.Output)
			}
		}
		if len(members) > 1 {
			b.errorf(path, "inputs and output must be on the same stack member (got members %s)", joinInts(sortedKeys(members)))
		}
	}
}

func (b *builder) validateRSTP() {
	c := b.cfg
	if c.RSTP == nil {
		return
	}
	for _, name := range sortedKeys(c.RSTP.Ports) {
		i, ok := c.Interfaces[name]
		path := "protocols rstp interface " + name
		if !ok {
			b.errorf(path, "%s is not configured under 'interfaces'", name)
			continue
		}
		if i.Parent != "" {
			b.errorf(path, "%s is a member of %s; configure RSTP on the aggregated interface", name, i.Parent)
		}
		if !i.Switching {
			b.warnf(path, "%s is not a switch port", name)
		}
	}
}

func sortedKeys[K int | string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func joinInts(ids []int) string {
	s := make([]string, len(ids))
	for i, v := range ids {
		s[i] = fmt.Sprint(v)
	}
	return strings.Join(s, ",")
}
