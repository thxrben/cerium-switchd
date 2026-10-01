package model

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strconv"
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
	Linux     string // kernel interface name
	MTU       int    // current kernel (Linux) MTU
	MaxMTU    int    // kernel (Linux) maximum MTU, 0 = unknown
	StackPort bool   // designated stacking port (never a data port)
	// PathMTU is the frame size (Ethernet header included) that probe frames
	// verified on a stacking port's cable; 0: not known.
	PathMTU int
	// Hardware capabilities (reference 1.7); zero values mean unknown.
	MaxSpeedMbps   int
	NoPause        bool // the driver has no pause-frame settings
	VlanChallenged bool // the NIC cannot carry VLAN tags
}

// Inventory supplies hardware facts for validation.
type Inventory interface {
	// Ports returns the ports of a member keyed by interface name
	// ("<member>/<card>/<port>"). known is false if nothing is known about
	// the member (e.g. not joined yet).
	Ports(member int) (ports map[string]PortInfo, known bool)
}

// portsOf returns the ports of a member; known is false if the member is
// unknown.
func (b *builder) portsOf(member int) (map[string]PortInfo, bool) {
	if b.inv == nil {
		return nil, false
	}
	return b.inv.Ports(member)
}

// port looks up one port. known is false if the member is unknown.
func (b *builder) port(member int, name string) (info PortInfo, ok, known bool) {
	if b.inv == nil {
		return PortInfo{}, false, false
	}
	ports, known := b.inv.Ports(member)
	if !known {
		return PortInfo{}, false, false
	}
	info, ok = ports[name]
	return info, ok, true
}

func (b *builder) validate() {
	c := b.cfg
	b.validateInterfaces() // attaches bundle member ports, needed below
	b.checkBundleSpeeds()
	b.validateRouting()
	b.validateMembers()
	b.validateLLDP()
	b.validateMTU()
	b.validateVXLAN()
	b.validateSnooping()
	b.validateAnalyzers()
	b.validateRSTP()

	b.notImplemented()
	b.checkPlainPorts()

	if n := len(c.System.NameServers); n > 3 {
		b.warnf("system name-server", "only the first 3 of %d name servers are used", n)
	}
	if c.System.ConsoleLogin && (c.System.Root == nil || c.System.Root.PasswordHash == "" && len(c.System.Root.SSHKeys) == 0) {
		b.warnf("system ports login-required", "root has no password or key (system root-authentication): root cannot log in on the consoles")
	}
	for _, u := range sortedKeys(c.System.Users) {
		usr := c.System.Users[u]
		if usr.PasswordHash == "" && len(usr.SSHKeys) == 0 {
			b.warnf("system login user "+u, "no authentication configured; the user cannot log in")
		}
	}
}

// validateLLDP warns about LLDP interfaces that are not configured.
func (b *builder) validateLLDP() {
	for _, e := range b.root.Get("protocols", "lldp").Entries("interface") {
		if e.Key != "all" && b.cfg.Interfaces[e.Key] == nil {
			b.warnf("protocols lldp interface "+e.Key, "%s is not configured under 'interfaces'; LLDP runs only on configured ports", e.Key)
		}
	}
}

func (b *builder) validateMembers() {
	c := b.cfg
	names := map[string]int{}
	for _, id := range sortedKeys(c.Members) {
		m := c.Members[id]
		path := fmt.Sprintf("virtual-chassis member %d", id)
		if m.HostName != "" {
			if o, dup := names[m.HostName]; dup {
				b.errorf(path+" host-name", "host-name %q is already used by member %d", m.HostName, o)
			}
			names[m.HostName] = id
		}
	}
}

func (b *builder) validateInterfaces() {
	c := b.cfg
	// Attach member ports to their aggregated interfaces.
	for _, name := range sortedKeys(c.Interfaces) {
		i := c.Interfaces[name]
		path := "interfaces " + name
		if i.AE && i.Management {
			b.errorf(path+" management", "a management port must be a physical port")
		}
		if i.AE {
			if i.Parent != "" || i.FlowControl != nil {
				b.errorf(path+" ether-options", "ether-options are only valid on physical ports")
			}
			continue
		}
		if i.LACP != nil || i.MCLAG {
			b.errorf(path+" aggregated-ether-options", "aggregated-ether-options are only valid on ae interfaces")
		}
		m, ok := c.Members[i.Member]
		if !ok {
			b.errorf(path, "virtual-chassis member %d is not configured", i.Member)
		} else if m.Witness {
			b.errorf(path, "member %d is a witness and has no switch ports", i.Member)
		}
		info, present, known := b.port(i.Member, name)
		switch {
		case known && !present:
			b.warnf(path, "port %s does not exist on member %d (configuration applies once it appears)", name, i.Member)
		case present && info.StackPort:
			b.errorf(path, "%s is a stacking port of member %d and cannot be configured as a data port", name, i.Member)
		}
		if present {
			b.checkCapabilities(i, info, path)
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
		switch {
		case len(i.MemberIDs) > 2:
			b.errorf(path, "ports on %d stack members (%s); a bundle spans at most two members (MC-LAG)", len(i.MemberIDs), joinInts(i.MemberIDs))
		case len(i.MemberIDs) == 2:
			i.MCLAG = true
			if i.LACP == nil {
				b.errorf(path+" aggregated-ether-options", "ports on members %s make %s an MC-LAG, which needs 'lacp'", joinInts(i.MemberIDs), name)
			}
			id := PairID(i.MemberIDs[0], i.MemberIDs[1])
			p := c.Pairs[id]
			if p == nil {
				p = &Pair{ID: id, Members: [2]int{i.MemberIDs[0], i.MemberIDs[1]}}
				c.Pairs[id] = p
			}
			p.Bundles = append(p.Bundles, name)
		}
	}
	// A member has one MC-LAG peer.
	peers := map[int]map[int][]string{}
	for _, id := range sortedKeys(c.Pairs) {
		p := c.Pairs[id]
		for _, m := range p.Members {
			if peers[m] == nil {
				peers[m] = map[int][]string{}
			}
			peers[m][p.Peer(m)] = p.Bundles
		}
	}
	for _, m := range sortedKeys(peers) {
		if ps := peers[m]; len(ps) > 1 {
			var parts []string
			for _, peer := range sortedKeys(ps) {
				parts = append(parts, fmt.Sprintf("%s with member %d", strings.Join(ps[peer], ", "), peer))
			}
			b.errorf("interfaces", "member %d has MC-LAG bundles with different members (%s); all MC-LAG bundles of a member must have the same peer",
				m, strings.Join(parts, "; "))
		}
	}
}

func (b *builder) validateMTU() {
	c := b.cfg
	for _, name := range sortedKeys(c.Interfaces) {
		i := c.Interfaces[name]
		path := "interfaces " + name
		// Member ports inherit the MTU of their bundle.
		if i.Parent != "" {
			if ae, ok := c.Interfaces[i.Parent]; ok {
				if i.MTU != DefaultMTU && i.MTU != ae.MTU {
					b.warnf(path+" mtu", "member ports use the MTU of %s (%d); this setting is ignored", i.Parent, ae.MTU)
				}
				i.MTU = ae.MTU
			}
		}
	}
	for _, name := range sortedKeys(c.Interfaces) {
		i := c.Interfaces[name]
		path := "interfaces " + name
		if !i.AE {
			if info, ok, _ := b.port(i.Member, i.Name); ok && info.MaxMTU > 0 && LinuxMTU(i.MTU) > info.MaxMTU {
				b.errorf(path+" mtu", "MTU %d exceeds the hardware maximum of %d on %s", i.MTU, info.MaxMTU+EthHeader, i.Name)
			}
		}
		for _, vid := range i.VLANs {
			v := c.VLANByID[vid]
			if v != nil && v.MTU > i.MTU {
				b.warnf(path+" mtu", "vlan %s allows MTU %d but this port only %d; larger frames are dropped here", v.Name, v.MTU, i.MTU)
			}
		}
	}
	b.validateStackMTU()
}

// validateStackMTU checks that the stacking ports of every member carry the
// largest data frame through the stack tunnels (reference 5.2, stack MTU).
func (b *builder) validateStackMTU() {
	c := b.cfg
	if len(c.SwitchMembers()) < 2 {
		return
	}
	mtu, where := c.MaxDataMTU()
	for _, m := range c.SwitchMembers() {
		ports, known := b.portsOf(m)
		if !known {
			continue
		}
		for _, name := range sortedKeys(ports) {
			p := ports[name]
			if !p.StackPort || p.MaxMTU <= 0 {
				continue
			}
			if max := min(p.MaxMTU, MaxStackPortMTU) + EthHeader; mtu+StackOverhead > max {
				b.errorf(where, "frames of %d bytes need %d on the stacking links, but stacking port %s of member %d carries at most %d; the largest mtu the stack can carry is %d",
					mtu, mtu+StackOverhead, name, m, max, max-StackOverhead)
			} else if p.PathMTU > 0 && mtu+StackOverhead > p.PathMTU {
				b.warnf(where, "frames of %d bytes need %d on the stacking links, but the cable at stacking port %s of member %d carries only %d (verified with probe frames); larger frames are lost. Check media converters, bridges and switches in between, or lower the mtu to %d",
					mtu, mtu+StackOverhead, name, m, p.PathMTU, p.PathMTU-StackOverhead)
			}
		}
	}
}

func (b *builder) validateVXLAN() {
	c := b.cfg
	vnis := map[int]string{}
	maxMTU := 0
	var first string
	for _, name := range sortedKeys(c.VLANs) {
		v := c.VLANs[name]
		if v.VNI == 0 {
			continue
		}
		if first == "" {
			first = name
		}
		if o, dup := vnis[v.VNI]; dup {
			b.errorf("vlans "+name+" vxlan vni", "vni %d is already used by vlan %s", v.VNI, o)
		}
		vnis[v.VNI] = name
		maxMTU = max(maxMTU, cmp.Or(v.MTU, DefaultMTU))
	}
	src := c.Switch.VTEPSource
	path := "switch-options vxlan"
	if src == "" {
		if first != "" {
			b.errorf("vlans "+first+" vxlan vni", "VXLAN needs the stack's VTEP address: switch-options vxlan source-address")
		}
	} else if a, err := netip.ParseAddr(src); err == nil {
		for _, n := range sortedKeys(c.L3) {
			for _, p := range c.L3[n].Addrs {
				if p.Addr() == a {
					b.errorf(path+" source-address", "%s is also the address of %s; the VTEP address must be its own", src, n)
				}
			}
		}
	}
	for _, vtep := range sortedKeys(c.Switch.RemoteVTEPs) {
		list := c.Switch.RemoteVTEPs[vtep]
		rp := path + " remote-vtep " + vtep
		if vtep == src {
			b.errorf(rp, "%s is the stack's own VTEP address", vtep)
		}
		if len(list) == 0 {
			b.warnf(rp, "no vni listed; the VTEP receives nothing")
		}
		for _, vni := range list {
			if _, ok := vnis[vni]; !ok {
				b.errorf(rp+" vni", "vni %d is not mapped to any VLAN", vni)
			}
		}
	}
	if len(vnis) == 0 || src == "" {
		return
	}
	// The routed path to the remote VTEPs needs the largest VXLAN frame plus
	// 50 bytes (reference 5.7): every routed interface of the default
	// instance could be that path.
	need := maxMTU + 50
	for _, n := range sortedKeys(c.L3) {
		u := c.L3[n]
		if u.Instance != "" || u.CME() {
			continue
		}
		mtu, where := DefaultMTU, "interfaces "+u.Parent+" mtu"
		if u.IRB() {
			where = "interfaces irb unit " + strconv.Itoa(u.Unit)
			if v := c.VLANByID[u.VLAN]; v != nil && v.MTU != 0 {
				mtu, where = v.MTU, "vlans "+v.Name+" mtu"
			}
		} else if i := c.Interfaces[u.Parent]; i != nil {
			mtu = i.MTU
		}
		if mtu < need {
			b.warnf(where, "%s may carry VXLAN to remote VTEPs: frames of %d bytes need %d there (VXLAN adds 50); larger frames are dropped", n, maxMTU, need)
		}
	}
}

// validateSnooping checks igmp-snooping and mld-snooping (reference 5.5).
func (b *builder) validateSnooping() {
	// The bridge snoops IGMP and MLD together.
	for _, id := range sortedKeys(b.cfg.VLANByID) {
		_, igmp := b.cfg.IGMP.VLAN(id)
		_, mld := b.cfg.MLD.VLAN(id)
		if igmp != mld {
			b.errorf("protocols", "vlan %s: igmp-snooping and mld-snooping must both be on or both off (the switch snoops both together)", b.cfg.VLANByID[id].Name)
		}
	}
	for _, x := range []struct {
		name string
		s    *Snooping
	}{{"igmp-snooping", b.cfg.IGMP}, {"mld-snooping", b.cfg.MLD}} {
		for _, n := range sortedKeys(x.s.Ports) {
			path := "protocols " + x.name + " interface " + n
			i, ok := b.cfg.Interfaces[n]
			switch {
			case !ok:
				b.errorf(path, "%s is not configured under 'interfaces'", n)
			case i.Parent != "":
				b.errorf(path, "%s is a member of %s; configure %s", n, i.Parent, i.Parent)
			case !i.Switching:
				b.warnf(path, "%s is not a switch port", n)
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
		// Mirroring happens on the output's member. Bundles spanning two
		// members (MC-LAG) contribute their local leg there.
		outMember := 0
		if o, ok := c.Interfaces[a.Output]; ok {
			if o.AE {
				if len(o.MemberIDs) == 1 {
					outMember = o.MemberIDs[0]
				} else if len(o.MemberIDs) > 1 {
					b.errorf(path+" output", "%s spans several members and cannot be a mirror output", a.Output)
				}
			} else {
				outMember = o.Member
			}
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
			switch {
			case !i.AE:
				members[i.Member] = true
			case outMember != 0 && slices.Contains(i.MemberIDs, outMember):
				members[outMember] = true
			default:
				for _, m := range i.MemberIDs {
					members[m] = true
				}
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
			b.errorf(path, "inputs must have ports on the output's member %d (got members %s)", outMember, joinInts(sortedKeys(members)))
		}
	}
}

func (b *builder) validateRSTP() {
	c := b.cfg
	for _, name := range c.BPDUBlock.Interfaces {
		i, ok := c.Interfaces[name]
		path := "protocols layer2-control bpdu-block interface"
		if !ok {
			b.errorf(path, "%s is not configured under 'interfaces'", name)
			continue
		}
		if i.Parent != "" {
			b.errorf(path, "%s is a member of %s; protect the aggregated interface", name, i.Parent)
		}
		if c.RSTP != nil {
			if p, ok := c.RSTP.Ports[name]; ok && !p.Edge && !p.Disabled {
				b.warnf(path, "%s runs RSTP as a non-edge port; any BPDU from a neighbouring switch will shut it down", name)
			}
		}
	}
	if c.RSTP == nil {
		return
	}
	// IEEE 802.1D timer relation: 2*(fwd-1) >= max-age >= 2*(hello+1).
	r := c.RSTP
	if r.MaxAge > 2*(r.ForwardDelay-1) || r.MaxAge < 2*(r.HelloTime+1) {
		b.errorf("protocols rstp", "timers violate 2*(forward-delay-1) >= max-age >= 2*(hello-time+1): forward-delay %d, max-age %d, hello-time %d",
			r.ForwardDelay, r.MaxAge, r.HelloTime)
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

// checkCapabilities compares a port's configuration with what its NIC can
// do (reference 1.7).
func (b *builder) checkCapabilities(i *Interface, info PortInfo, path string) {
	if info.NoPause && i.FlowControl != nil {
		b.warnf(path+" ether-options", "%s has no pause-frame support; flow-control settings have no effect", i.Name)
	}
	if info.VlanChallenged {
		switch {
		case i.Switching && i.Mode == "trunk":
			b.errorf(path+" unit 0 family ethernet-switching interface-mode", "the NIC of %s cannot carry VLAN tags; it can only be an access port", i.Name)
		case i.VlanTagging:
			b.errorf(path+" vlan-tagging", "the NIC of %s cannot carry VLAN tags", i.Name)
		}
	}
}

// checkBundleSpeeds warns about bundles mixing port speeds.
func (b *builder) checkBundleSpeeds() {
	for _, name := range sortedKeys(b.cfg.Interfaces) {
		i := b.cfg.Interfaces[name]
		if !i.AE || len(i.MemberPorts) < 2 {
			continue
		}
		speeds := map[int][]string{}
		unknown := false
		for _, p := range i.MemberPorts {
			pi := b.cfg.Interfaces[p]
			if pi == nil {
				continue
			}
			info, ok, _ := b.port(pi.Member, p)
			if !ok || info.MaxSpeedMbps == 0 {
				unknown = true // no check for this bundle
				break
			}
			speeds[info.MaxSpeedMbps] = append(speeds[info.MaxSpeedMbps], p)
		}
		if !unknown && len(speeds) > 1 {
			var parts []string
			for _, sp := range sortedKeys(speeds) {
				parts = append(parts, fmt.Sprintf("%s: %s", speedName(sp), strings.Join(speeds[sp], ", ")))
			}
			b.warnf("interfaces "+name, "member ports have different maximum speeds (%s); traffic is hashed evenly, so the slower ports limit their share", strings.Join(parts, "; "))
		}
	}
}

func speedName(mbps int) string {
	if mbps >= 1000 && mbps%1000 == 0 {
		return fmt.Sprintf("%dG", mbps/1000)
	}
	return fmt.Sprintf("%dM", mbps)
}

// notImplemented warns about statements that are accepted but have no
// effect yet (reference 8).
func (b *builder) notImplemented() {
	r := b.root
	if len(b.cfg.BPDUBlock.Interfaces) > 0 {
		b.warnf("protocols layer2-control bpdu-block", "bpdu-block is not implemented yet: the listed ports are not protected")
	}
	if r.Get("system", "services", "web-management") != nil {
		b.warnf("system services web-management", "web-management is not implemented yet: the statement has no effect")
	}
}

// checkPlainPorts warns about physical ports that are configured but
// neither switched, routed, a bundle member, a management port nor a
// mirror output (reference 5.3.2, "plain port"): they are up and carry
// nothing, and they run no RSTP. That is rarely what was meant.
func (b *builder) checkPlainPorts() {
	c := b.cfg
	used := map[string]bool{}
	for _, a := range c.Analyzers {
		used[a.Output] = true
	}
	for _, u := range c.L3 {
		used[u.Parent] = true
	}
	for _, name := range sortedKeys(c.Interfaces) {
		i := c.Interfaces[name]
		if i.AE || i.Switching || i.Parent != "" || i.Management || i.VlanTagging || i.Disabled || used[name] {
			continue
		}
		b.warnf("interfaces "+name, "%s is a plain port: up, but neither switched, routed nor a bundle member, so it carries no traffic and runs no RSTP (add 'unit 0 family ethernet-switching' to switch on it)", name)
	}
}
