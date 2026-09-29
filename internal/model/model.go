// Package model converts a configuration tree into typed structures used by
// the data plane and validates it (commit check).
package model

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"mclag/internal/config"
	"mclag/internal/schema"
)

// Config is the typed view of a committed configuration.
type Config struct {
	System     System
	Members    map[int]*Member
	Interfaces map[string]*Interface
	VLANs      map[string]*VLAN // by name
	VLANByID   map[int]*VLAN
	RSTP       *RSTP // nil when not configured or disabled
	Domains    map[int]*Domain
	Switch     SwitchOptions
	Analyzers  map[string]*Analyzer
	BPDUBlock  BPDUBlock
	StackBFD   BFD
}

type System struct {
	HostName    string
	DomainName  string
	TimeZone    string
	NameServers []string
	NTPServers  []NTPServer
	Syslog      []SyslogHost
	LogBuffer   int
	Users       map[string]*User
	Banner      string
	SSH         SSHService
	Web         WebService
	Commit      CommitPolicy
	Consoles    map[string]*Console
	AutoConsole bool
	// ConsoleLogin: local consoles ask for credentials (default: autologin
	// as root into the CLI).
	ConsoleLogin bool
	Offload      OffloadPolicy
}

type NTPServer struct {
	Host   string
	Prefer bool
}

type SyslogHost struct {
	Host      string
	Port      int
	Transport string
	Facility  string
	Severity  string
	CAFile    string
}

type User struct {
	Name         string
	UID          int
	Class        string
	FullName     string
	PasswordHash string
	SSHKeys      []string
}

type SSHService struct {
	// Configured: 'system services ssh' is present (otherwise switchd
	// leaves the SSH server configuration alone).
	Configured bool
	Port       int
	RootLogin  string
}

type WebService struct {
	Enabled  bool
	Port     int
	CertFile string
	KeyFile  string
}

type CommitPolicy struct {
	ConfirmRequired bool
	TimeoutMinutes  int
}

type Console struct {
	Device   string
	Speed    int
	Disabled bool
}

type OffloadPolicy struct {
	Enabled           bool
	WatchdogInterval  int
	WatchdogThreshold int
	WatchdogAlarmOnly bool
}

type Member struct {
	ID          int
	HostName    string
	Priority    int
	Witness     bool
	Mgmt        L3Interface
	VTEPAddress string
	Underlay    L3Interface
}

// L3Interface is an IP interface of a member, attached either to a VLAN of
// the bridge (IRB-like) or to a dedicated non-switched port.
type L3Interface struct {
	VLAN      int    // resolved VLAN id (0 = not VLAN based)
	Interface string // dedicated port (<member>/<card>/<port>)
	Addresses []string
	DHCP      bool
	Gateways  []string
}

// Configured reports whether the interface is attached anywhere.
func (l L3Interface) Configured() bool { return l.VLAN != 0 || l.Interface != "" }

// HasAddress reports whether the interface gets any address.
func (l L3Interface) HasAddress() bool { return len(l.Addresses) > 0 || l.DHCP }

// DefaultMTU is the default interface MTU: a standard Ethernet frame of
// 1500 bytes payload plus the 14 byte header (Junos convention).
const DefaultMTU = 1514

// EthHeader is the Ethernet header length included in configured MTUs.
const EthHeader = 14

// LinuxMTU converts a configured (frame size) MTU into the kernel MTU.
func LinuxMTU(mtu int) int { return mtu - EthHeader }

// Interface is a physical port or aggregated interface.
type Interface struct {
	Name        string
	Member      int // physical ports only
	AE          bool
	Description string
	Disabled    bool
	MTU         int    // frame size incl. Ethernet header (see LinuxMTU)
	Range       string // interface-range that contributed configuration
	Explicit    bool   // listed under 'interfaces' itself
	// Physical port options.
	Parent      string // ae this port belongs to
	FlowControl *bool
	// Aggregated interface options.
	LACP         *LACP
	LACPPriSet   bool // system-priority explicitly configured
	MinLinks     int
	HashPolicy   string
	MCLAG        bool     // bundle is an MC-LAG (spans or may span both domain members)
	MemberPorts  []string // physical ports with 802.3ad pointing here
	MemberIDs    []int    // stack members hosting MemberPorts
	StormControl StormControl
	MACLimit     int
	NoOffload    bool
	// Switching.
	Switching  bool
	Mode       string // access or trunk
	VLANs      []int  // resolved VLAN ids (sorted)
	NativeVLAN int    // untagged VLAN on trunks (0 = none)
	AccessVLAN int
}

type LACP struct {
	Active         bool
	Fast           bool
	SystemPriority int
}

type StormControl struct {
	Broadcast, Multicast int
}

type VLAN struct {
	Name        string
	ID          int
	Description string
	MTU         int // 0 = unrestricted by VLAN
	VNI         int
}

type RSTP struct {
	BridgePriority int
	HelloTime      int
	MaxAge         int
	ForwardDelay   int
	Ports          map[string]*RSTPPort
}

type RSTPPort struct {
	Name       string
	Cost       int
	Priority   int
	Edge       bool
	RootGuard  bool
	PointToPnt *bool
	Disabled   bool
}

type BPDUBlock struct {
	Interfaces     []string
	DisableTimeout int // seconds, 0 = never re-enable automatically
}

// BFD holds BFD session timers.
type BFD struct {
	IntervalMS int
	Multiplier int
}

// DetectionMS is the time until a silent peer is declared down.
func (b BFD) DetectionMS() int { return b.IntervalMS * b.Multiplier }

func buildBFD(n *config.Node, interval int) BFD {
	return BFD{IntervalMS: atoi(n.Leaf("minimum-interval"), interval), Multiplier: atoi(n.Leaf("multiplier"), 3)}
}

type Domain struct {
	ID             int
	Members        []int
	PeerLink       string
	SystemMAC      string
	SystemPriority int
	AnycastVTEP    string
	Heartbeat      BFD
	PeerLinkBFD    BFD
	DelayRestore   int
}

type SwitchOptions struct {
	MACAging     int
	VXLANMode    string
	VXLANPort    int
	RemoteVTEPs  map[string][]int
	VXLANEncrypt bool
}

type Analyzer struct {
	Name         string
	IngressIfs   []string
	EgressIfs    []string
	IngressVLANs []int
	Output       string
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// Build converts a tree into the typed model and runs all validation.
// Inactive statements are ignored as if deleted. inv may be nil; it
// supplies hardware facts (e.g. maximum MTU).
func Build(t *config.Tree, inv Inventory) (*Config, Issues) {
	b := &builder{root: t.Active().Root, inv: inv, rangeOf: map[string]string{}}
	b.build()
	b.validate()
	sort.SliceStable(b.issues, func(i, j int) bool { return b.issues[i].Severity > b.issues[j].Severity })
	return b.cfg, b.issues
}

type builder struct {
	root    *config.Node
	inv     Inventory
	cfg     *Config
	issues  Issues
	rangeOf map[string]string // interface -> interface-range name
}

func (b *builder) errorf(path string, format string, args ...any) {
	b.issues = append(b.issues, Issue{Severity: Error, Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (b *builder) warnf(path string, format string, args ...any) {
	b.issues = append(b.issues, Issue{Severity: Warning, Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (b *builder) build() {
	r := b.root
	c := &Config{
		Members:    map[int]*Member{},
		Interfaces: map[string]*Interface{},
		VLANs:      map[string]*VLAN{},
		VLANByID:   map[int]*VLAN{},
		Domains:    map[int]*Domain{},
		Analyzers:  map[string]*Analyzer{},
	}
	b.cfg = c

	// System.
	sys := r.Child("system")
	s := &c.System
	s.HostName = sys.Leaf("host-name")
	s.DomainName = sys.Leaf("domain-name")
	s.TimeZone = sys.Leaf("time-zone")
	s.NameServers = sys.List("name-server")
	for _, e := range sys.Get("ntp").Entries("server") {
		s.NTPServers = append(s.NTPServers, NTPServer{Host: e.Key, Prefer: e.Has("prefer")})
	}
	for _, e := range sys.Get("syslog").Entries("host") {
		h := SyslogHost{
			Host:      e.Key,
			Transport: orDefault(e.Leaf("transport"), "udp"),
			Facility:  orDefault(e.Leaf("facility"), "any"),
			Severity:  orDefault(e.Leaf("severity"), "info"),
			CAFile:    e.Leaf("ca-certificate"),
		}
		def := 514
		if h.Transport == "tls" {
			def = 6514
		}
		h.Port = atoi(e.Leaf("port"), def)
		s.Syslog = append(s.Syslog, h)
	}
	s.LogBuffer = atoi(sys.Leaf("syslog", "local-buffer-size"), 5000)
	s.Users = map[string]*User{}
	for _, e := range sys.Get("login").Entries("user") {
		s.Users[e.Key] = &User{
			Name:         e.Key,
			UID:          atoi(e.Leaf("uid"), 0),
			Class:        orDefault(e.Leaf("class"), "read-only"),
			FullName:     e.Leaf("full-name"),
			PasswordHash: e.Leaf("authentication", "encrypted-password"),
			SSHKeys:      e.List("authentication", "ssh-key"),
		}
	}
	s.Banner = sys.Leaf("login", "message")
	s.SSH = SSHService{Configured: sys.Has("services", "ssh"), Port: atoi(sys.Leaf("services", "ssh", "port"), 22), RootLogin: orDefault(sys.Leaf("services", "ssh", "root-login"), "deny")}
	web := sys.Get("services", "web-management")
	s.Web = WebService{Enabled: !web.Has("disable"), Port: atoi(web.Leaf("port"), 443), CertFile: web.Leaf("certificate"), KeyFile: web.Leaf("key")}
	conf := sys.Get("commit", "confirmation")
	s.Commit = CommitPolicy{ConfirmRequired: conf.Leaf("mode") != "optional", TimeoutMinutes: atoi(conf.Leaf("timeout"), 10)}
	s.AutoConsole = !sys.Has("ports", "no-auto-detect")
	s.ConsoleLogin = sys.Has("ports", "login-required")
	s.Consoles = map[string]*Console{}
	for _, e := range sys.Get("ports").Entries("console") {
		s.Consoles[e.Key] = &Console{Device: e.Key, Speed: atoi(e.Leaf("speed"), 115200), Disabled: e.Has("disable")}
	}
	off := sys.Get("offload")
	s.Offload = OffloadPolicy{
		Enabled:           off.Leaf("mode") != "disable",
		WatchdogInterval:  atoi(off.Leaf("watchdog", "interval"), 5),
		WatchdogThreshold: atoi(off.Leaf("watchdog", "threshold"), 100),
		WatchdogAlarmOnly: off.Has("watchdog", "alarm-only"),
	}

	// VLANs.
	for _, e := range r.Entries("vlans") {
		v := &VLAN{Name: e.Key, ID: atoi(e.Leaf("vlan-id"), 0), Description: e.Leaf("description"), MTU: atoi(e.Leaf("mtu"), 0), VNI: atoi(e.Leaf("vxlan", "vni"), 0)}
		c.VLANs[v.Name] = v
		path := "vlans " + v.Name
		if v.ID == 0 {
			b.errorf(path, "vlan-id is required")
			continue
		}
		if o, dup := c.VLANByID[v.ID]; dup {
			b.errorf(path, "vlan-id %d is already used by vlan %s", v.ID, o.Name)
			continue
		}
		c.VLANByID[v.ID] = v
	}

	// Stack members. Without explicit members the node is member 1.
	for _, e := range r.Get("stack").Entries("member") {
		id := atoi(e.Key, 0)
		m := &Member{
			ID:          id,
			HostName:    e.Leaf("host-name"),
			Priority:    atoi(e.Leaf("priority"), 128),
			Witness:     e.Leaf("role") == "witness",
			VTEPAddress: e.Leaf("vtep-address"),
		}
		path := fmt.Sprintf("stack member %d", id)
		m.Mgmt = b.buildL3(e.Get("management"), path+" management")
		m.Underlay = b.buildL3(e.Get("underlay"), path+" underlay")
		c.Members[id] = m
	}
	if len(c.Members) == 0 {
		c.Members[1] = &Member{ID: 1, Priority: 128}
	}

	// Interfaces: explicit entries merged with interface-range templates.
	for _, e := range b.effectiveInterfaces() {
		i := &Interface{Name: e.Key, AE: schema.IsAE(e.Key)}
		if !i.AE {
			if p, ok := schema.ParsePhysical(e.Key); ok {
				i.Member = p.Member
			}
		}
		i.Description = e.Leaf("description")
		i.Disabled = e.Has("disable")
		i.MTU = atoi(e.Leaf("mtu"), DefaultMTU)
		i.Range = b.rangeOf[i.Name]
		i.Explicit = r.Entry("interfaces", i.Name) != nil
		i.Parent = e.Leaf("ether-options", "802.3ad")
		if e.Has("ether-options", "flow-control") {
			t := true
			i.FlowControl = &t
		} else if e.Has("ether-options", "no-flow-control") {
			f := false
			i.FlowControl = &f
		}
		agg := e.Get("aggregated-ether-options")
		if l := agg.Get("lacp"); l != nil {
			i.LACP = &LACP{Active: !l.Has("passive"), Fast: l.Leaf("periodic") != "slow", SystemPriority: atoi(l.Leaf("system-priority"), 32768)}
		}
		i.MinLinks = atoi(agg.Leaf("minimum-links"), 1)
		i.HashPolicy = orDefault(agg.Leaf("hash-policy"), "layer3+4")
		i.MCLAG = agg.Has("mclag")
		i.LACPPriSet = agg.Leaf("lacp", "system-priority") != ""
		sc := e.Get("storm-control")
		i.StormControl = StormControl{Broadcast: atoi(sc.Leaf("broadcast"), 0), Multicast: atoi(sc.Leaf("multicast"), 0)}
		i.MACLimit = atoi(e.Leaf("mac-limit"), 0)
		i.NoOffload = e.Has("offload", "disable")
		c.Interfaces[i.Name] = i
		b.buildSwitching(i, e)
	}

	// RSTP.
	if rs := r.Get("protocols", "rstp"); rs != nil && !rs.Has("disable") {
		c.RSTP = &RSTP{
			BridgePriority: atoi(rs.Leaf("bridge-priority"), 32768),
			HelloTime:      atoi(rs.Leaf("hello-time"), 2),
			MaxAge:         atoi(rs.Leaf("max-age"), 20),
			ForwardDelay:   atoi(rs.Leaf("forward-delay"), 15),
			Ports:          map[string]*RSTPPort{},
		}
		for _, e := range rs.Entries("interface") {
			p := &RSTPPort{Name: e.Key, Cost: atoi(e.Leaf("cost"), 0), Priority: atoi(e.Leaf("priority"), 128),
				Edge: e.Has("edge"), RootGuard: e.Has("no-root-port"), Disabled: e.Has("disable")}
			if m := e.Leaf("mode"); m != "" {
				v := m == "point-to-point"
				p.PointToPnt = &v
			}
			c.RSTP.Ports[p.Name] = p
		}
	}

	// MC-LAG domains.
	for _, e := range r.Get("mclag").Entries("domain") {
		d := &Domain{
			ID:             atoi(e.Key, 0),
			PeerLink:       e.Leaf("peer-link"),
			SystemMAC:      e.Leaf("system-mac"),
			SystemPriority: atoi(e.Leaf("system-priority"), 32768),
			AnycastVTEP:    e.Leaf("anycast-vtep"),
			Heartbeat:      buildBFD(e.Get("heartbeat"), 300),
			PeerLinkBFD:    buildBFD(e.Get("peer-link-bfd"), 100),
			DelayRestore:   atoi(e.Leaf("delay-restore"), 300),
		}
		for _, m := range e.List("members") {
			d.Members = append(d.Members, atoi(m, 0))
		}
		c.Domains[d.ID] = d
	}

	// Switch options.
	so := r.Get("switch-options")
	c.Switch = SwitchOptions{
		MACAging:     atoi(so.Leaf("mac-table-aging-time"), 300),
		VXLANMode:    orDefault(so.Leaf("vxlan", "mode"), "control-plane"),
		VXLANPort:    atoi(so.Leaf("vxlan", "udp-port"), 4789),
		RemoteVTEPs:  map[string][]int{},
		VXLANEncrypt: so.Has("vxlan", "encryption"),
	}
	for _, e := range so.Get("vxlan").Entries("remote-vtep") {
		var vnis []int
		for _, v := range e.List("vni") {
			vnis = append(vnis, atoi(v, 0))
		}
		c.Switch.RemoteVTEPs[e.Key] = vnis
	}

	c.StackBFD = buildBFD(r.Get("stack", "bfd"), 100)

	// BPDU protection.
	bb := r.Get("protocols", "layer2-control", "bpdu-block")
	c.BPDUBlock = BPDUBlock{Interfaces: bb.List("interface"), DisableTimeout: atoi(bb.Leaf("disable-timeout"), 0)}

	// Analyzers.
	for _, e := range r.Get("forwarding-options").Entries("analyzer") {
		a := &Analyzer{
			Name:       e.Key,
			IngressIfs: e.List("input", "ingress", "interface"),
			EgressIfs:  e.List("input", "egress", "interface"),
			Output:     e.Leaf("output", "interface"),
		}
		for _, v := range e.List("input", "ingress", "vlan") {
			ids, err := b.resolveVLANRef(v)
			if err != nil {
				b.errorf("forwarding-options analyzer "+a.Name+" input ingress vlan", "%v", err)
				continue
			}
			a.IngressVLANs = append(a.IngressVLANs, ids...)
		}
		c.Analyzers[a.Name] = a
	}
}

func (b *builder) buildSwitching(i *Interface, e *config.Node) {
	es := e.Get("unit", "0", "family", "ethernet-switching")
	path := "interfaces " + i.Name
	native := e.Leaf("native-vlan-id")
	if es == nil {
		if native != "" {
			b.errorf(path, "native-vlan-id requires 'unit 0 family ethernet-switching'")
		}
		return
	}
	i.Switching = true
	i.Mode = orDefault(es.Leaf("interface-mode"), "access")
	seen := map[int]bool{}
	for _, ref := range es.List("vlan", "members") {
		ids, err := b.resolveVLANRef(ref)
		if err != nil {
			b.errorf(path+" unit 0 family ethernet-switching vlan members", "%v", err)
			continue
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				i.VLANs = append(i.VLANs, id)
			}
		}
	}
	sort.Ints(i.VLANs)
	if native != "" {
		ids, err := b.resolveVLANRef(native)
		switch {
		case err != nil:
			b.errorf(path+" native-vlan-id", "%v", err)
		case len(ids) != 1:
			b.errorf(path+" native-vlan-id", "must reference exactly one VLAN")
		default:
			i.NativeVLAN = ids[0]
		}
	}
	switch i.Mode {
	case "access":
		if native != "" {
			b.errorf(path+" native-vlan-id", "native-vlan-id is only valid in trunk mode")
		}
		switch len(i.VLANs) {
		case 0:
			b.warnf(path, "access port is not a member of any VLAN; untagged traffic will be dropped")
		case 1:
			i.AccessVLAN = i.VLANs[0]
		default:
			b.errorf(path+" unit 0 family ethernet-switching vlan members", "access port must be a member of exactly one VLAN, got %d", len(i.VLANs))
		}
	case "trunk":
		if i.NativeVLAN != 0 && !seen[i.NativeVLAN] {
			i.VLANs = append(i.VLANs, i.NativeVLAN)
			sort.Ints(i.VLANs)
		}
		if len(i.VLANs) == 0 {
			b.warnf(path, "trunk port carries no VLANs")
		}
	}
}

// resolveVLANRef turns a VLAN reference (name, id, range or "all") into ids
// of configured VLANs.
func (b *builder) resolveVLANRef(ref string) ([]int, error) {
	if ref == "all" {
		ids := make([]int, 0, len(b.cfg.VLANByID))
		for id := range b.cfg.VLANByID {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		return ids, nil
	}
	if lo, hi, ok := schema.ParseVlanRange(ref); ok {
		var ids, missing []int
		for id := lo; id <= hi; id++ {
			if _, ok := b.cfg.VLANByID[id]; ok {
				ids = append(ids, id)
			} else {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("vlan-id %s is not defined under 'vlans'", compactRanges(missing))
		}
		return ids, nil
	}
	v, ok := b.cfg.VLANs[ref]
	if !ok {
		return nil, fmt.Errorf("vlan %q is not defined", ref)
	}
	if v.ID == 0 {
		return nil, fmt.Errorf("vlan %q has no vlan-id", ref)
	}
	return []int{v.ID}, nil
}

func compactRanges(ids []int) string {
	var parts []string
	for i := 0; i < len(ids); {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(ids[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ids[i], ids[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// buildL3 builds a management or underlay IP interface.
func (b *builder) buildL3(n *config.Node, path string) L3Interface {
	l := L3Interface{
		Interface: n.Leaf("interface"),
		Addresses: n.List("address"),
		DHCP:      n.Has("dhcp"),
		Gateways:  n.List("gateway"),
	}
	if ref := n.Leaf("vlan"); ref != "" {
		ids, err := b.resolveVLANRef(ref)
		if err != nil {
			b.errorf(path+" vlan", "%v", err)
		} else {
			l.VLAN = ids[0]
		}
	}
	return l
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
