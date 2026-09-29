package model

import (
	"fmt"
	"mclag/internal/schema"
	"slices"
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
	Linux     string // kernel interface name
	MTU       int    // current kernel (Linux) MTU
	MaxMTU    int    // kernel (Linux) maximum MTU, 0 = unknown
	StackPort bool   // designated stacking port (never a data port)
	// HasIP: the operating system has configured IP addresses on the port
	// (typically the installer's management NIC).
	HasIP bool
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
	b.validateMTU()
	b.validateDomains()
	b.validateVXLAN()
	b.validateAnalyzers()
	b.validateRSTP()

	if n := len(c.System.NameServers); n > 3 {
		b.warnf("system name-server", "only the first 3 of %d name servers are used", n)
	}
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
		path := fmt.Sprintf("virtual-chassis member %d", id)
		if m.HostName != "" {
			if o, dup := names[m.HostName]; dup {
				b.errorf(path+" host-name", "host-name %q is already used by member %d", m.HostName, o)
			}
			names[m.HostName] = id
		}
		b.validateL3(id, path+" underlay", m.Underlay)
		if m.Underlay.VLAN != 0 {
			if v := c.VLANByID[m.Underlay.VLAN]; v != nil && v.VNI != 0 {
				b.errorf(path+" underlay vlan", "vlan %s is extended over VXLAN and cannot carry the VXLAN underlay", v.Name)
			}
		}
	}
}

// validateL3 checks a management or underlay IP interface.
func (b *builder) validateL3(member int, path string, l L3Interface) {
	c := b.cfg
	if !l.Configured() {
		if l.HasAddress() || len(l.Gateways) > 0 {
			b.errorf(path, "addresses require 'vlan' or 'interface'")
		}
		return
	}
	if !l.HasAddress() {
		b.warnf(path, "no address configured")
	}
	v4 := 0
	for _, a := range l.Addresses {
		if !strings.Contains(a, ":") {
			v4++
		}
	}
	if l.DHCP && v4 > 0 {
		b.errorf(path, "use either 'dhcp' or a static IPv4 address, not both")
	}
	gw := map[bool]int{}
	for _, g := range l.Gateways {
		v6 := strings.Contains(g, ":")
		gw[v6]++
		hasFamily := l.DHCP && !v6
		for _, a := range l.Addresses {
			if strings.Contains(a, ":") == v6 {
				hasFamily = true
			}
		}
		if !hasFamily {
			b.warnf(path+" gateway", "gateway %s has no address of its family on this interface", g)
		}
	}
	if gw[false] > 1 || gw[true] > 1 {
		b.errorf(path+" gateway", "at most one gateway per address family")
	}
	if l.Interface != "" {
		name := l.Interface
		if p, ok := schema.ParsePhysical(name); ok && p.Member != member {
			b.errorf(path+" interface", "%s belongs to member %d, not %d", name, p.Member, member)
		}
		if i, ok := c.Interfaces[name]; ok && (i.Switching || i.Parent != "") {
			b.errorf(path+" interface", "%s is used as a switch port and cannot carry an IP interface", i.Name)
		}
		if info, ok, _ := b.port(member, l.Interface); ok && info.StackPort {
			b.errorf(path+" interface", "%s is a stacking port and never carries IP", l.Interface)
		}
	}
	if l.VLAN != 0 && !b.memberHasVLAN(member, l.VLAN) {
		b.warnf(path+" vlan", "no switch port of member %d carries vlan-id %d; the address is unreachable", member, l.VLAN)
	}
}

// memberHasVLAN reports whether any switch port of a member (or an MC-LAG
// bundle / peer-link with ports on it) carries the VLAN.
func (b *builder) memberHasVLAN(member, vid int) bool {
	for _, i := range b.cfg.Interfaces {
		if !i.Switching {
			continue
		}
		on := i.Member == member
		for _, p := range i.MemberPorts {
			if pi := b.cfg.Interfaces[p]; pi != nil && pi.Member == member {
				on = true
			}
		}
		if !on {
			continue
		}
		for _, v := range i.VLANs {
			if v == vid {
				return true
			}
		}
	}
	return false
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
		case present && info.HasIP && !b.reservedPort(i.Member, name):
			b.warnf(path, "%s (%s) has IP addresses configured by the operating system (management port?); managing it may cut access to member %d", name, info.Linux, i.Member)
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
		isPeerLink := false
		for _, d := range c.Domains {
			if d.PeerLink == name {
				isPeerLink = true
			}
		}
		if i.MCLAG {
			if i.LACP == nil {
				b.errorf(path+" aggregated-ether-options", "MC-LAG interfaces require 'lacp'")
			}
			if isPeerLink {
				b.errorf(path+" aggregated-ether-options mclag", "the peer-link cannot be an MC-LAG interface")
			}
			if d := b.domainFor(i.MemberIDs); d == nil && len(i.MemberIDs) > 0 {
				b.errorf(path+" aggregated-ether-options mclag", "no mclag domain contains member(s) %s", joinInts(i.MemberIDs))
			} else if d != nil && i.LACPPriSet && i.LACP != nil && i.LACP.SystemPriority != d.SystemPriority {
				b.warnf(path+" aggregated-ether-options lacp system-priority", "ignored on MC-LAG interfaces; mclag domain %d system-priority %d is used", d.ID, d.SystemPriority)
			}
		}
		switch {
		case len(i.MemberIDs) > 2:
			b.errorf(path, "ports on %d stack members (%s); at most two are possible (MC-LAG)", len(i.MemberIDs), joinInts(i.MemberIDs))
		case len(i.MemberIDs) == 2 && !i.MCLAG && !isPeerLink:
			b.errorf(path, "ports on members %s require 'aggregated-ether-options mclag' or use as a peer-link", joinInts(i.MemberIDs))
		}
		if isPeerLink && i.Switching {
			b.warnf(path, "the peer-link carries all VLANs automatically; its ethernet-switching settings are ignored")
		}
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
	// The peer-link must carry everything its domain's MC-LAG ports carry.
	for _, did := range sortedKeys(c.Domains) {
		d := c.Domains[did]
		pl, ok := c.Interfaces[d.PeerLink]
		if !ok {
			continue
		}
		for _, name := range sortedKeys(c.Interfaces) {
			i := c.Interfaces[name]
			if i.MCLAG && b.domainFor(i.MemberIDs) == d && i.MTU > pl.MTU {
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
				b.errorf(path+" members", "virtual-chassis member %d is not configured", m)
				continue
			}
			if mem.Witness {
				b.errorf(path+" members", "member %d is a witness", m)
			}
			if o, dup := inDomain[m]; dup {
				b.errorf(path+" members", "member %d is already part of domain %d", m, o)
			}
			inDomain[m] = did
			if len(c.MgmtAddrs(m)) == 0 {
				b.warnf(path, "member %d has no management address; the BFD split-brain heartbeat is not possible", m)
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
			b.errorf(fmt.Sprintf("virtual-chassis member %d", id), "vtep-address is required when VLANs are extended over VXLAN")
		}
	}
	// The underlay must carry the largest extended frame plus encapsulation.
	maxMTU := DefaultMTU
	for _, v := range c.VLANs {
		if v.VNI != 0 && v.MTU > maxMTU {
			maxMTU = v.MTU
		}
	}
	for _, id := range sortedKeys(c.Members) {
		m := c.Members[id]
		if !m.Underlay.Configured() {
			continue
		}
		overhead := 50 // IPv4 + UDP + VXLAN + inner Ethernet
		if strings.Contains(m.VTEPAddress, ":") {
			overhead = 70
		}
		if c.Switch.VXLANEncrypt {
			overhead += 80 // WireGuard over IPv6 worst case
		}
		var mtu int
		var where string
		if m.Underlay.Interface != "" {
			where = fmt.Sprintf("interfaces %s mtu", m.Underlay.Interface)
			if i, ok := c.Interfaces[m.Underlay.Interface]; ok {
				mtu = i.MTU
			} else if info, ok, _ := b.port(id, m.Underlay.Interface); ok && info.MTU > 0 {
				mtu = info.MTU + EthHeader // unmanaged port: its current MTU
			} else {
				continue
			}
		} else {
			v := c.VLANByID[m.Underlay.VLAN]
			if v == nil || v.MTU == 0 {
				continue
			}
			where, mtu = "vlans "+v.Name+" mtu", v.MTU
		}
		if mtu < maxMTU+overhead {
			b.warnf(where, "underlay MTU %d is below %d (largest VXLAN VLAN MTU %d + %d bytes encapsulation); larger frames are dropped at the tunnel", mtu, maxMTU+overhead, maxMTU, overhead)
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
		for _, p := range i.MemberPorts {
			pi := b.cfg.Interfaces[p]
			if pi == nil {
				continue
			}
			info, ok, _ := b.port(pi.Member, p)
			if !ok || info.MaxSpeedMbps == 0 {
				return // unknown: no check
			}
			speeds[info.MaxSpeedMbps] = append(speeds[info.MaxSpeedMbps], p)
		}
		if len(speeds) > 1 {
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
