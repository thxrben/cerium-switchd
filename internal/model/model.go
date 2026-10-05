// Package model converts a configuration tree into typed structures used by
// the data plane and validates it (commit check).
package model

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/memslots"
	"github.com/thxrben/cerium-switchd/internal/schema"
)

// Config is the typed view of a committed configuration.
type Config struct {
	System     System
	Members    map[int]*Member
	Interfaces map[string]*Interface
	VLANs      map[string]*VLAN // by name
	VLANByID   map[int]*VLAN
	RSTP       *RSTP // nil when not configured or disabled
	LLDP       *LLDP
	// Pairs are the MC-LAG pairs (reference 5.6), derived from the bundles
	// with ports on two members, by Pair.ID.
	Pairs     map[int]*Pair
	MCLAG     MCLAGOptions
	Switch    SwitchOptions
	Analyzers map[string]*Analyzer
	BPDUBlock BPDUBlock
	// IGMP and MLD snooping (reference 5.5): never nil.
	IGMP, MLD *Snooping
	StackBFD  BFD
	L3        map[string]*L3Unit // routed interfaces by unit name ("irb.10", "1/0/6.100")
	Routes    []StaticRoute      // default instance
	Instances map[string]*RoutingInstance
	// Routing is the routing protocol configuration of the default
	// instance (nil: none); Policies is policy-options (never nil).
	Routing  *Routing
	Policies *Policies
	// MACsec is security macsec and virtual-chassis macsec (reference
	// 5.15, 5.2).
	MACsec MACsec
}

type System struct {
	HostName string
	// MgmtInstance names the management routing instance (reference 1.8;
	// "": none).
	MgmtInstance string
	DomainName   string
	TimeZone     string
	NameServers  []string
	NTPServers   []NTPServer
	Syslog       []SyslogHost
	LogBuffer    int
	Users        map[string]*User
	// Root: system root-authentication (nil: not configured).
	Root        *User
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
	Timeouts     Timeouts
	Archival     *Archival // nil: none
	// Memory is system memory (no allocation: dynamic memory).
	Memory memslots.Config
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

// WebService is system services web-management: the REST API (reference
// 5.1).
type WebService struct {
	// Enabled: configured and not disabled.
	Enabled  bool
	Port     int
	CertFile string
	KeyFile  string
	// UploadLimit is the largest bundle an upload may have (bytes).
	UploadLimit uint64
	// Tokens are the API tokens by name.
	Tokens map[string]APIToken
}

// APIToken is an api-token (reference 5.1 web-management).
type APIToken struct {
	User string
	Hash string // SHA-256 of the token, hex
}

// DefaultUploadLimit is upload-limit's default (1g).
const DefaultUploadLimit = 1 << 30

type CommitPolicy struct {
	ConfirmRequired bool
	TimeoutMinutes  int
}

type Console struct {
	Device   string
	Speed    int
	Disabled bool
}

// Archival is system archival configuration (reference 5.1).
type Archival struct {
	OnCommit bool
	Interval int // minutes (0: none)
	Sites    []ArchiveSite
}

// ArchiveSite is one place for the copies.
type ArchiveSite struct {
	URL      string
	Password string
}

// Timeouts are system timeouts (reference 5.1): how long cerOS waits for
// disks, the kernel and the steps of a software update.
type Timeouts struct {
	DiskOperation    time.Duration `json:"disk_operation"`
	KernelCall       time.Duration `json:"kernel_call"`
	SlotWrite        time.Duration `json:"slot_write"`
	SoftwareTransfer time.Duration `json:"software_transfer"`
	SoftwareInstall  time.Duration `json:"software_install"`
	MemberUpdate     time.Duration `json:"member_update"`
	HealthCheck      time.Duration `json:"health_check"`
	ConfigCheck      time.Duration `json:"config_check"`
}

// DefaultTimeouts are the timeouts without configuration.
var DefaultTimeouts = Timeouts{DiskOperation: 10 * time.Second, KernelCall: 5 * time.Second, SlotWrite: time.Minute,
	SoftwareTransfer: 10 * time.Minute, SoftwareInstall: 15 * time.Minute, MemberUpdate: 10 * time.Minute,
	HealthCheck: 5 * time.Minute, ConfigCheck: 2 * time.Minute}

type OffloadPolicy struct {
	Enabled           bool
	WatchdogInterval  int
	WatchdogThreshold int
	WatchdogAlarmOnly bool
}

type Member struct {
	ID       int
	HostName string
	Priority int
	Witness  bool
}

// DefaultMTU is the default interface MTU: a standard Ethernet frame of
// 1500 bytes payload plus the 14 byte header (Junos convention).
const DefaultMTU = 1514

// The highest card and port of an LACP bundle's port: the LACP port number
// member × 1024 + card × 64 + port has 16 bits.
const (
	MaxLACPCard = 15
	MaxLACPPort = 63
)

// EthHeader is the Ethernet header length included in configured MTUs.
const EthHeader = 14

// The stack tunnels (reference 5.2): a frame between members needs
// StackOverhead bytes more on a stacking link (tunnel 50, the VLAN tag
// inside the tunnel 4, one more tag of the frame 4), without MACsec
// (Config.StackPortOverhead adds it). switchd sets stacking ports to their
// NIC maximum, at most MaxStackPortMTU (kernel MTU).
const (
	StackOverhead   = 58
	MaxStackPortMTU = 16044
)

// MemberHostName is member id's own name (reference 5.1, 5.2): its
// virtual-chassis member host-name, else system host-name ("": none). It is
// the operating system's host name and the name in the member's logs.
func (c *Config) MemberHostName(id int) string {
	if m := c.Members[id]; m != nil && m.HostName != "" {
		return m.HostName
	}
	return c.System.HostName
}

// ChassisName is the name the stack presents to the outside as one system
// (LLDP; reference 5.1): system host-name, else member id's own name.
func (c *Config) ChassisName(id int) string {
	if c.System.HostName != "" {
		return c.System.HostName
	}
	return c.MemberHostName(id)
}

// SwitchMembers returns the stack members that switch traffic (not
// witnesses), sorted; a switch without virtual-chassis members is member 1.
func (c *Config) SwitchMembers() []int {
	var out []int
	for id, m := range c.Members {
		if !m.Witness {
			out = append(out, id)
		}
	}
	if len(c.Members) == 0 {
		out = []int{1}
	}
	slices.Sort(out)
	return out
}

// MaxDataMTU returns the largest frame (mtu) any switched interface or VLAN
// allows, and the statement that sets it.
func (c *Config) MaxDataMTU() (int, string) {
	mtu, where := DefaultMTU, ""
	for _, name := range slices.Sorted(maps.Keys(c.Interfaces)) {
		i := c.Interfaces[name]
		if i.Switching && i.Parent == "" && i.MTU > mtu {
			mtu, where = i.MTU, "interfaces "+name+" mtu"
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.VLANs)) {
		if v := c.VLANs[name]; v.MTU > mtu {
			mtu, where = v.MTU, "vlans "+name+" mtu"
		}
	}
	if where == "" {
		where = "virtual-chassis"
	}
	return mtu, where
}

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
	MinLinks     int
	HashPolicy   string
	MCLAG        bool     // bundle is an MC-LAG: its ports are on two members (reference 5.6)
	MemberPorts  []string // physical ports with 802.3ad pointing here
	MemberIDs    []int    // stack members hosting MemberPorts
	StormControl StormControl
	MACLimit     int
	NoOffload    bool
	VlanTagging  bool // routed subinterfaces
	// Management: a management port (reference 5.3.4), carries only cme.
	Management bool
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
	L3          string // l3-interface ("irb.10")
}

// LLDP is protocols lldp (reference 5.5); nil when not configured or
// disabled.
type LLDP struct {
	Interval, Hold int
	// Only: the listed interfaces (nil: all); Off: excluded ones.
	Only, Off map[string]bool
}

// Runs reports whether LLDP runs on interface name (a port, or the bundle
// its port is in).
func (l *LLDP) Runs(name, bundle string) bool {
	if l == nil || l.Off[name] || (bundle != "" && l.Off[bundle]) {
		return false
	}
	return l.Only == nil || l.Only[name] || (bundle != "" && l.Only[bundle])
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

// Snooping is protocols igmp-snooping or mld-snooping.
type Snooping struct {
	Disabled bool
	// VLANs by VLAN id; All applies to VLANs without an entry.
	VLANs map[int]SnoopVLAN
	All   SnoopVLAN
	Ports map[string]SnoopPort
}

// SnoopVLAN is the snooping of one VLAN.
type SnoopVLAN struct {
	Disabled bool
	Querier  bool
	Version  int
}

// SnoopPort is the snooping setting of one interface.
type SnoopPort struct {
	ImmediateLeave bool
	Router         bool // multicast-router-interface
}

// VLAN returns the settings of VLAN id (on: snooping runs there).
func (s *Snooping) VLAN(id int) (v SnoopVLAN, on bool) {
	v, ok := s.VLANs[id]
	if !ok {
		v = s.All
	}
	return v, !s.Disabled && !v.Disabled
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

// Pair is the two members of MC-LAG bundles (reference 5.6).
type Pair struct {
	ID      int    // PairID(Members[0], Members[1])
	Members [2]int // sorted
	Bundles []string
}

// PairID identifies the pair of members a and b (in either order).
func PairID(a, b int) int { return 32*min(a, b) + max(a, b) }

// Peer returns the other member of the pair (0 if m is not in it).
func (p *Pair) Peer(m int) int {
	switch m {
	case p.Members[0]:
		return p.Members[1]
	case p.Members[1]:
		return p.Members[0]
	}
	return 0
}

// PairOf returns the MC-LAG pair of member m (nil: m has no MC-LAG).
func (c *Config) PairOf(m int) *Pair {
	for _, id := range sortedKeys(c.Pairs) {
		if p := c.Pairs[id]; p.Peer(m) != 0 {
			return p
		}
	}
	return nil
}

// MCLAGOptions are the stack-wide MC-LAG settings.
type MCLAGOptions struct {
	DelayRestore int // seconds
}

type SwitchOptions struct {
	MACAging int
	// VTEPSource is the stack's VTEP address (reference 5.7; "": none).
	VTEPSource  string
	VXLANPort   int
	RemoteVTEPs map[string][]int // remote VTEP -> VNIs
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
		L3:         map[string]*L3Unit{},
		Instances:  map[string]*RoutingInstance{},
		Interfaces: map[string]*Interface{},
		VLANs:      map[string]*VLAN{},
		VLANByID:   map[int]*VLAN{},
		Pairs:      map[int]*Pair{},
		Analyzers:  map[string]*Analyzer{},
	}
	b.cfg = c

	// System.
	sys := r.Child("system")
	s := &c.System
	s.HostName = sys.Leaf("host-name")
	s.MgmtInstance = sys.Leaf("management-instance")
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
	if ra := sys.Get("root-authentication"); sys.Has("root-authentication") {
		s.Root = &User{Name: "root", UID: 0, Class: "super-user", PasswordHash: ra.Leaf("encrypted-password"), SSHKeys: ra.List("ssh-key")}
	}
	s.Banner = sys.Leaf("login", "message")
	s.SSH = SSHService{Configured: sys.Has("services", "ssh"), Port: atoi(sys.Leaf("services", "ssh", "port"), 22), RootLogin: orDefault(sys.Leaf("services", "ssh", "root-login"), "deny")}
	web := sys.Get("services", "web-management")
	s.Web = WebService{Enabled: sys.Has("services", "web-management") && !web.Has("disable"), Port: atoi(web.Leaf("port"), 443),
		CertFile: web.Leaf("certificate"), KeyFile: web.Leaf("key"), UploadLimit: DefaultUploadLimit}
	if l, err := schema.ParseSize(web.Leaf("upload-limit")); err == nil {
		s.Web.UploadLimit = l
	}
	s.Web.Tokens = map[string]APIToken{}
	for _, e := range web.Entries("api-token") {
		s.Web.Tokens[e.Key] = APIToken{User: e.Leaf("user"), Hash: e.Leaf("hash")}
	}
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
	mem := sys.Get("memory")
	s.Memory = memslots.Config{UpdateSize: memslots.DefaultUpdateSize, MgmtReserve: memslots.DefaultMgmtReserve}
	if v, err := schema.ParseSize(mem.Leaf("update-size")); err == nil {
		s.Memory.UpdateSize = v
	}
	if v, err := schema.ParseSize(mem.Leaf("management-reserve")); err == nil {
		s.Memory.MgmtReserve = v
	}
	total := 0
	for _, e := range mem.Entries("allocation") {
		a := memslots.Amount{Percent: atoi(e.Leaf("percent"), 0), Slots: atoi(e.Leaf("slots"), 0)}
		if a.Percent == 0 && a.Slots == 0 {
			b.errorf("system memory allocation "+e.Key, "give percent or slots")
			continue
		}
		total += a.Percent
		if s.Memory.Alloc == nil {
			s.Memory.Alloc = map[memslots.Purpose]memslots.Amount{}
		}
		s.Memory.Alloc[memslots.Purpose(e.Key)] = a
	}
	if total > 100 {
		b.errorf("system memory allocation", "the percentages add up to %d %%", total)
	}
	if ar := sys.Get("archival", "configuration"); ar != nil {
		a := &Archival{OnCommit: ar.Has("transfer-on-commit"), Interval: atoi(ar.Leaf("transfer-interval"), 0)}
		for _, e := range ar.Entries("archive-sites") {
			a.Sites = append(a.Sites, ArchiveSite{URL: e.Key, Password: e.Leaf("password")})
			if !ArchiveURLOK(e.Key) {
				b.errorf("system archival configuration archive-sites "+e.Key, "unsupported site %q (expecting ftp://, sftp://, scp://, http:// or https:// with a host)", e.Key)
			}
		}
		if !a.OnCommit && a.Interval == 0 {
			b.errorf("system archival configuration", "set transfer-on-commit or transfer-interval")
		}
		s.Archival = a
	}
	to := sys.Get("timeouts")
	sec := func(name string, def time.Duration) time.Duration {
		return time.Duration(atoi(to.Leaf(name), int(def/time.Second))) * time.Second
	}
	d := DefaultTimeouts
	s.Timeouts = Timeouts{DiskOperation: sec("disk-operation", d.DiskOperation), KernelCall: sec("kernel-call", d.KernelCall),
		SlotWrite: sec("slot-write", d.SlotWrite), SoftwareTransfer: sec("software-transfer", d.SoftwareTransfer),
		SoftwareInstall: sec("software-install", d.SoftwareInstall), MemberUpdate: sec("member-update", d.MemberUpdate),
		HealthCheck: sec("health-check", d.HealthCheck), ConfigCheck: sec("config-check", d.ConfigCheck)}

	// VLANs.
	for _, e := range r.Entries("vlans") {
		v := &VLAN{Name: e.Key, ID: atoi(e.Leaf("vlan-id"), 0), Description: e.Leaf("description"), MTU: atoi(e.Leaf("mtu"), 0), VNI: atoi(e.Leaf("vxlan", "vni"), 0),
			L3: e.Leaf("l3-interface")}
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
	for _, e := range r.Get("virtual-chassis").Entries("member") {
		id := atoi(e.Key, 0)
		m := &Member{
			ID:       id,
			HostName: e.Leaf("host-name"),
			Priority: atoi(e.Leaf("mastership-priority"), 128),
			Witness:  e.Leaf("role") == "witness",
		}
		c.Members[id] = m
	}
	if len(c.Members) == 0 {
		c.Members[1] = &Member{ID: 1, Priority: 128}
	}

	// Interfaces: explicit entries merged with interface-range templates.
	for _, e := range b.effectiveInterfaces() {
		if e.Key == "irb" || e.Key == schema.CME {
			b.buildIRB(e)
			continue
		}
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
		sc := e.Get("storm-control")
		i.StormControl = StormControl{Broadcast: atoi(sc.Leaf("broadcast"), 0), Multicast: atoi(sc.Leaf("multicast"), 0)}
		i.MACLimit = atoi(e.Leaf("mac-limit"), 0)
		i.NoOffload = e.Has("offload", "disable")
		i.Management = e.Has("management")
		if i.Management {
			for _, k := range e.Kids {
				switch k.Schema.Name {
				case "management", "description", "disable", "mtu":
				default:
					b.errorf("interfaces "+i.Name+" "+k.Schema.Name, "a management port carries only cme; only description, disable and mtu are allowed")
				}
			}
		}
		c.Interfaces[i.Name] = i
		b.buildSwitching(i, e)
		b.buildUnits(i.Name, e, i)
	}
	c.Routes = b.buildRoutes(r.Get("routing-options"), "routing-options")
	b.buildInstances()
	b.buildPolicies()
	c.Routing = b.buildRouting("", "", r.Get("routing-options"), r.Get("protocols"))
	for _, e := range r.Entries("routing-instances") {
		if in := c.Instances[e.Key]; in != nil {
			in.Routing = b.buildRouting(e.Key, "routing-instances "+e.Key, e.Get("routing-options"), e.Get("protocols"))
		}
	}
	b.validateRoutingProtocols()

	// LLDP.
	if l := r.Get("protocols", "lldp"); l != nil && !l.Has("disable") {
		c.LLDP = &LLDP{Interval: atoi(l.Leaf("advertisement-interval"), 30), Hold: atoi(l.Leaf("hold-multiplier"), 4), Off: map[string]bool{}}
		for _, e := range l.Entries("interface") {
			if e.Has("disable") {
				c.LLDP.Off[e.Key] = true
				continue
			}
			if e.Key == "all" {
				continue
			}
			if c.LLDP.Only == nil {
				c.LLDP.Only = map[string]bool{}
			}
			c.LLDP.Only[e.Key] = true
		}
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

	// MC-LAG (the pairs follow from the bundles, validateInterfaces).
	c.MCLAG = MCLAGOptions{DelayRestore: atoi(r.Leaf("mclag", "delay-restore"), 300)}

	// Switch options.
	so := r.Get("switch-options")
	c.Switch = SwitchOptions{
		MACAging:    atoi(so.Leaf("mac-table-aging-time"), 300),
		VTEPSource:  so.Leaf("vxlan", "source-address"),
		VXLANPort:   atoi(so.Leaf("vxlan", "udp-port"), 4789),
		RemoteVTEPs: map[string][]int{},
	}
	for _, e := range so.Get("vxlan").Entries("remote-vtep") {
		var vnis []int
		for _, v := range e.List("vni") {
			vnis = append(vnis, atoi(v, 0))
		}
		c.Switch.RemoteVTEPs[e.Key] = vnis
	}

	c.StackBFD = buildBFD(r.Get("virtual-chassis", "bfd"), 100)

	// IGMP and MLD snooping.
	c.IGMP = b.buildSnooping(r.Get("protocols", "igmp-snooping"), "protocols igmp-snooping", 2)
	c.MLD = b.buildSnooping(r.Get("protocols", "mld-snooping"), "protocols mld-snooping", 1)

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
	b.buildMACsec()
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

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// buildSnooping builds igmp-snooping or mld-snooping (absent: on with the
// defaults).
func (b *builder) buildSnooping(n *config.Node, path string, version int) *Snooping {
	s := &Snooping{Disabled: n.Has("disable"), VLANs: map[int]SnoopVLAN{}, Ports: map[string]SnoopPort{},
		All: SnoopVLAN{Version: version}}
	for _, e := range n.Entries("vlan") {
		v := SnoopVLAN{Disabled: e.Has("disable"), Querier: e.Has("querier"), Version: atoi(e.Leaf("version"), version)}
		if e.Key == "all" {
			s.All = v
			continue
		}
		ids, err := b.resolveVLANRef(e.Key)
		if err != nil {
			b.errorf(path+" vlan "+e.Key, "%v", err)
			continue
		}
		s.VLANs[ids[0]] = v
	}
	for _, e := range n.Entries("interface") {
		s.Ports[e.Key] = SnoopPort{ImmediateLeave: e.Has("immediate-leave"), Router: e.Has("multicast-router-interface")}
	}
	return s
}

// ArchiveURLOK reports whether an archive site is a supported URL.
func ArchiveURLOK(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "ftp", "sftp", "scp", "http", "https":
		return true
	}
	return false
}
