package model

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"mclag/internal/config"
	"mclag/internal/schema"
)

// The routing configuration (reference 5.8, 5.11-5.14).

// Routing is the routing configuration of one instance (the default
// instance is Name "").
type Routing struct {
	Instance string
	RouterID netip.Addr // invalid: chosen automatically
	AS       uint32
	OSPF     *OSPF // nil: not configured
	OSPF3    *OSPF
	BGP      *BGP
}

// NeighborBFD is bfd-liveness-detection of a routing protocol neighbour
// (reference 5.12).
type NeighborBFD struct {
	IntervalMs int
	Multiplier int
	AuthAlg    string // keyed-sha-1, keyed-md5, "" none
	AuthKey    string
	AuthKeyID  int
}

// OSPF is protocols ospf or ospf3 of one instance.
type OSPF struct {
	V3              bool
	Areas           map[netip.Addr]*OSPFArea // by area id (dotted as an IPv4 address)
	Export          []string
	ReferenceBW     uint64 // bit/s
	Overload        bool
	OverloadTimeout int // seconds after start (0: always)
	GracefulRestart bool
	RestartDuration int
	Disabled        bool
	InterfaceArea   map[string]netip.Addr // unit -> area
	ABR             bool
}

// OSPFArea is one area.
type OSPFArea struct {
	ID         netip.Addr
	Interfaces map[string]*OSPFInterface // by unit name
}

// OSPFInterface is an interface of an area.
type OSPFInterface struct {
	Unit         string
	Passive      bool
	Metric       int // 0: from the reference bandwidth
	P2P          bool
	Priority     int
	Hello        int
	Dead         int
	Retransmit   int
	TransitDelay int
	SimplePass   string
	MD5          map[int]string // key id -> key
	BFD          *NeighborBFD
}

// BGP is protocols bgp of one instance.
type BGP struct {
	Groups   map[string]*BGPGroup
	Disabled bool
}

// BGPGroup is a neighbour group; its neighbours have every setting
// resolved (group values inherited).
type BGPGroup struct {
	Name       string
	Internal   bool
	Multipath  bool
	MultipleAS bool
	Cluster    netip.Addr
	Neighbors  map[netip.Addr]*BGPNeighbor
}

// BGPNeighbor is one neighbour with its effective settings.
type BGPNeighbor struct {
	Addr            netip.Addr
	Group           string
	Description     string
	PeerAS          uint32
	LocalAddress    netip.Addr
	LocalAS         uint32 // effective local AS (instance AS unless local-as)
	AuthKey         string
	HoldTime        int // -1: default
	Passive         bool
	Multihop        bool
	TTL             int
	IPv4, IPv6      bool // families
	Import, Export  []string
	RemovePrivate   bool
	BFD             *NeighborBFD
	GracefulRestart bool
	RestartTime     int
	StaleTime       int
	Disabled        bool
	Internal        bool
	RouteReflector  bool // the neighbour is a client of this switch (cluster id set)
	Cluster         netip.Addr
	Multipath       bool
	MultipleAS      bool
}

// ---- policy-options ----

// Policies is policy-options.
type Policies struct {
	PrefixLists map[string][]netip.Prefix
	Communities map[string][]string
	ASPaths     map[string]string
	Statements  map[string]*PolicyStatement
}

// PolicyStatement is a routing policy.
type PolicyStatement struct {
	Name  string
	Terms []*PolicyTerm
	// Final is the then directly under the policy: "accept", "reject" or "".
	Final string
}

// PolicyTerm is one term.
type PolicyTerm struct {
	Name string
	From PolicyFrom
	Then PolicyThen
}

// PolicyFrom are the match conditions (all must match; empty: any).
type PolicyFrom struct {
	Protocols         []string
	RouteFilters      []RouteFilter
	PrefixLists       []string
	PrefixListFilters []PrefixListFilter
	Communities       []string
	ASPaths           []string
	Neighbors         []netip.Addr
	Areas             []netip.Addr
	Family            string // inet, inet6, ""
	Tag               *uint32
}

// RouteFilter is route-filter <prefix> <match>.
type RouteFilter struct {
	Prefix netip.Prefix
	Match  string // exact, orlonger, longer, upto, range
	Lo, Hi int    // prefix lengths for upto/range
}

// PrefixListFilter is prefix-list-filter <name> match <m>.
type PrefixListFilter struct {
	List  string
	Match string
}

// PolicyThen are the actions.
type PolicyThen struct {
	Flow            string // accept, reject, "next term", "next policy", "" (= next term)
	Metric          *uint32
	MetricAdd       *uint32
	LocalPref       *uint32
	Preference      *int
	CommunityAdd    []string
	CommunityDelete []string
	CommunitySet    []string
	SetCommunity    bool
	ASPathPrepend   []uint32
	NextHop         string // self, discard, <ip>, ""
	ExternalType    int    // 1, 2, 0
	Tag             *uint32
}

// ---- building ----

func (b *builder) buildBFD(n *config.Node) *NeighborBFD {
	if n == nil {
		return nil
	}
	return &NeighborBFD{IntervalMs: atoi(n.Leaf("minimum-interval"), 300), Multiplier: atoi(n.Leaf("multiplier"), 3),
		AuthAlg: n.Leaf("authentication", "algorithm"), AuthKey: n.Leaf("authentication", "key"),
		AuthKeyID: atoi(n.Leaf("authentication", "key-id"), 1)}
}

func u32(s string) uint32 {
	n, _ := strconv.ParseUint(s, 10, 32)
	return uint32(n)
}

func u32p(s string) *uint32 {
	if s == "" {
		return nil
	}
	v := u32(s)
	return &v
}

// buildRouting builds the routing protocols of an instance; ro is its
// routing-options node, pr its protocols node.
func (b *builder) buildRouting(inst, base string, ro, pr *config.Node) *Routing {
	r := &Routing{Instance: inst}
	if id := ro.Leaf("router-id"); id != "" {
		r.RouterID, _ = netip.ParseAddr(id)
	}
	r.AS = u32(ro.Leaf("autonomous-system"))
	protoBase := "protocols"
	if inst != "" {
		protoBase = base + " protocols"
	}
	if n := pr.Get("ospf"); n != nil {
		r.OSPF = b.buildOSPF(inst, protoBase+" ospf", n, false)
	}
	if n := pr.Get("ospf3"); n != nil {
		r.OSPF3 = b.buildOSPF(inst, protoBase+" ospf3", n, true)
	}
	if n := pr.Get("bgp"); n != nil {
		r.BGP = b.buildBGP(r, protoBase+" bgp", n)
	}
	if r.OSPF == nil && r.OSPF3 == nil && r.BGP == nil && !r.RouterID.IsValid() && r.AS == 0 {
		return nil
	}
	return r
}

func (b *builder) buildOSPF(inst, path string, n *config.Node, v3 bool) *OSPF {
	o := &OSPF{V3: v3, Areas: map[netip.Addr]*OSPFArea{}, InterfaceArea: map[string]netip.Addr{},
		Export: n.List("export"), Disabled: n.Has("disable"),
		Overload: n.Has("overload"), OverloadTimeout: atoi(n.Leaf("overload", "timeout"), 0),
		GracefulRestart: !n.Has("graceful-restart", "disable"), RestartDuration: atoi(n.Leaf("graceful-restart", "restart-duration"), 120)}
	o.ReferenceBW, _ = schema.ParseBandwidth(orDefault(n.Leaf("reference-bandwidth"), "100g"))
	fam := "inet"
	if v3 {
		fam = "inet6"
	}
	for _, ae := range n.Entries("area") {
		id, _ := netip.ParseAddr(ae.Key)
		area := &OSPFArea{ID: id, Interfaces: map[string]*OSPFInterface{}}
		o.Areas[id] = area
		for _, ie := range ae.Entries("interface") {
			ipath := path + " area " + ae.Key + " interface " + ie.Key
			hello := atoi(ie.Leaf("hello-interval"), 10)
			oi := &OSPFInterface{Unit: ie.Key, Passive: ie.Has("passive"), Metric: atoi(ie.Leaf("metric"), 0),
				P2P: ie.Leaf("interface-type") == "p2p", Priority: atoi(ie.Leaf("priority"), 128),
				Hello: hello, Dead: atoi(ie.Leaf("dead-interval"), 4*hello),
				Retransmit: atoi(ie.Leaf("retransmit-interval"), 5), TransitDelay: atoi(ie.Leaf("transit-delay"), 1),
				SimplePass: ie.Leaf("authentication", "simple-password"), BFD: b.buildBFD(ie.Get("bfd-liveness-detection"))}
			for _, k := range ie.Get("authentication").Entries("md5") {
				if oi.MD5 == nil {
					oi.MD5 = map[int]string{}
				}
				oi.MD5[atoi(k.Key, 0)] = k.Leaf("key")
				if k.Leaf("key") == "" {
					b.errorf(ipath+" authentication md5 "+k.Key, "needs a key")
				}
			}
			if oi.Dead <= oi.Hello {
				b.errorf(ipath+" dead-interval", "the dead interval (%d s) must be larger than the hello interval (%d s)", oi.Dead, oi.Hello)
			}
			if prev, ok := o.InterfaceArea[ie.Key]; ok {
				b.errorf(ipath, "%s is already in area %s", ie.Key, prev)
				continue
			}
			o.InterfaceArea[ie.Key] = id
			area.Interfaces[ie.Key] = oi
			b.checkProtocolUnit(inst, ipath, ie.Key, fam)
		}
	}
	if len(o.Areas) > 1 {
		o.ABR = true
		if _, ok := o.Areas[netip.AddrFrom4([4]byte{})]; !ok {
			b.errorf(path, "interfaces in several areas make this switch an area border router, which needs an interface in area 0 (the backbone)")
		}
	}
	return o
}

// checkProtocolUnit checks that a unit used by a routing protocol exists,
// is in the instance and has an address of the family.
func (b *builder) checkProtocolUnit(inst, path, unit, fam string) {
	u := b.cfg.L3[unit]
	switch {
	case u == nil:
		b.errorf(path, "%s is not a routed interface (it needs family %s)", unit, fam)
		return
	case u.CME():
		b.errorf(path, "routing protocols do not run on the management interface")
		return
	case u.Instance != inst:
		where := "the default instance"
		if u.Instance != "" {
			where = "routing instance " + u.Instance
		}
		b.errorf(path, "%s is in %s", unit, where)
		return
	}
	has, shared := false, false
	for _, p := range u.Addrs {
		if p.Addr().Is4() == (fam == "inet") {
			has = true
			if _, one := u.AddrMember[p]; !one {
				shared = true
			}
		}
	}
	if !has && !(fam == "inet" && u.DHCP) {
		b.errorf(path, "%s has no %s address", unit, map[string]string{"inet": "IPv4", "inet6": "IPv6"}[fam])
		return
	}
	if u.IRB() && has && !shared {
		b.errorf(path, "%s has only member addresses; the routing protocols run once for the stack, on the master's irb, which needs an address without 'member'", unit)
	}
}

func (b *builder) buildBGP(r *Routing, path string, n *config.Node) *BGP {
	g := &BGP{Groups: map[string]*BGPGroup{}, Disabled: n.Has("disable")}
	seen := map[netip.Addr]string{}
	for _, ge := range n.Entries("group") {
		gpath := path + " group " + ge.Key
		grp := &BGPGroup{Name: ge.Key, Internal: ge.Leaf("type") == "internal", Multipath: ge.Has("multipath"),
			MultipleAS: ge.Has("multipath", "multiple-as"), Neighbors: map[netip.Addr]*BGPNeighbor{}}
		if c := ge.Leaf("cluster"); c != "" {
			grp.Cluster, _ = netip.ParseAddr(c)
			if !grp.Internal {
				b.errorf(gpath+" cluster", "route reflection is for internal groups")
			}
		}
		if ge.Leaf("type") == "" {
			b.errorf(gpath, "needs 'type internal' or 'type external'")
		}
		for _, ne := range ge.Entries("neighbor") {
			npath := gpath + " neighbor " + ne.Key
			a, _ := netip.ParseAddr(ne.Key)
			if prev, ok := seen[a]; ok {
				b.errorf(npath, "neighbor %s is already in group %s", a, prev)
				continue
			}
			seen[a] = ge.Key
			pick := func(name string) string {
				if v := ne.Leaf(name); v != "" {
					return v
				}
				return ge.Leaf(name)
			}
			list := func(name string) []string {
				if v := ne.List(name); len(v) > 0 {
					return v
				}
				return ge.List(name)
			}
			has := func(p ...string) bool { return ne.Has(p...) || ge.Has(p...) }
			nb := &BGPNeighbor{Addr: a, Group: ge.Key, Description: pick("description"), PeerAS: u32(pick("peer-as")),
				AuthKey: pick("authentication-key"), HoldTime: atoi(pick("hold-time"), -1), Passive: has("passive"),
				Multihop: has("multihop"), TTL: atoi(pick("ttl"), 0), Import: list("import"), Export: list("export"),
				RemovePrivate: has("remove-private"), Disabled: ne.Has("disable") || ge.Has("disable"),
				Internal: grp.Internal, Cluster: grp.Cluster, RouteReflector: grp.Cluster.IsValid(),
				Multipath: grp.Multipath, MultipleAS: grp.MultipleAS,
				GracefulRestart: !has("graceful-restart", "disable")}
			if ne.Get("multihop") != nil {
				nb.TTL = atoi(ne.Leaf("multihop", "ttl"), 0)
			} else if ge.Get("multihop") != nil {
				nb.TTL = atoi(ge.Leaf("multihop", "ttl"), 0)
			}
			gr := ne.Get("graceful-restart")
			if gr == nil {
				gr = ge.Get("graceful-restart")
			}
			nb.RestartTime, nb.StaleTime = atoi(gr.Leaf("restart-time"), 120), atoi(gr.Leaf("stale-routes-time"), 300)
			bfd := ne.Get("bfd-liveness-detection")
			if bfd == nil {
				bfd = ge.Get("bfd-liveness-detection")
			}
			nb.BFD = b.buildBFD(bfd)
			if la := pick("local-address"); la != "" {
				nb.LocalAddress, _ = netip.ParseAddr(la)
			}
			nb.LocalAS = r.AS
			if la := pick("local-as"); la != "" {
				nb.LocalAS = u32(la)
			}
			fam := ne.Get("family")
			if fam == nil {
				fam = ge.Get("family")
			}
			nb.IPv4, nb.IPv6 = fam.Has("inet", "unicast"), fam.Has("inet6", "unicast")
			if !nb.IPv4 && !nb.IPv6 {
				nb.IPv4, nb.IPv6 = a.Is4(), !a.Is4()
			}
			switch {
			case nb.LocalAS == 0:
				b.errorf(npath, "no autonomous system: set routing-options autonomous-system or local-as")
			case grp.Internal && nb.PeerAS != 0 && nb.PeerAS != nb.LocalAS:
				b.errorf(npath+" peer-as", "an internal neighbour is in the own AS (%d), not %d", nb.LocalAS, nb.PeerAS)
			case !grp.Internal && nb.PeerAS == 0:
				b.errorf(npath, "an external neighbour needs peer-as")
			case !grp.Internal && nb.PeerAS == nb.LocalAS:
				b.errorf(npath+" peer-as", "peer-as %d is the own AS: use 'type internal'", nb.PeerAS)
			}
			if grp.Internal && nb.PeerAS == 0 {
				nb.PeerAS = nb.LocalAS
			}
			if nb.LocalAddress.IsValid() && nb.LocalAddress.Is4() != a.Is4() {
				b.errorf(npath+" local-address", "the local address must be of the neighbour's address family")
			}
			grp.Neighbors[a] = nb
		}
		g.Groups[ge.Key] = grp
	}
	return g
}

// buildPolicies builds policy-options.
func (b *builder) buildPolicies() {
	n := b.root.Get("policy-options")
	p := &Policies{PrefixLists: map[string][]netip.Prefix{}, Communities: map[string][]string{},
		ASPaths: map[string]string{}, Statements: map[string]*PolicyStatement{}}
	b.cfg.Policies = p
	for _, e := range n.Entries("prefix-list") {
		var ps []netip.Prefix
		for _, s := range e.List("prefix") {
			if pr, err := netip.ParsePrefix(s); err == nil {
				ps = append(ps, pr)
			}
		}
		p.PrefixLists[e.Key] = ps
	}
	for _, e := range n.Entries("community") {
		p.Communities[e.Key] = e.List("members")
		if len(e.List("members")) == 0 {
			b.errorf("policy-options community "+e.Key, "needs members")
		}
	}
	for _, e := range n.Entries("as-path") {
		path := "policy-options as-path " + e.Key
		re := e.Leaf("path")
		if re == "" {
			b.errorf(path, "needs a path expression")
			continue
		}
		if _, err := CompileASPath(re); err != nil {
			b.errorf(path+" path", "%v", err)
		}
		p.ASPaths[e.Key] = re
	}
	for _, e := range n.Entries("policy-statement") {
		path := "policy-options policy-statement " + e.Key
		ps := &PolicyStatement{Name: e.Key}
		switch {
		case e.Has("then", "accept"):
			ps.Final = "accept"
		case e.Has("then", "reject"):
			ps.Final = "reject"
		}
		unreachable := false
		for _, te := range e.Entries("term") {
			tpath := path + " term " + te.Key
			t := &PolicyTerm{Name: te.Key}
			f := te.Get("from")
			t.From = PolicyFrom{Protocols: f.List("protocol"), PrefixLists: f.List("prefix-list"),
				Communities: f.List("community"), ASPaths: f.List("as-path"), Family: f.Leaf("family"), Tag: u32p(f.Leaf("tag"))}
			for _, s := range f.List("neighbor") {
				if a, err := netip.ParseAddr(s); err == nil {
					t.From.Neighbors = append(t.From.Neighbors, a)
				}
			}
			for _, s := range f.List("area") {
				if a, err := netip.ParseAddr(s); err == nil {
					t.From.Areas = append(t.From.Areas, a)
				}
			}
			for _, rf := range f.Entries("route-filter") {
				pr, _ := netip.ParsePrefix(rf.Key)
				r := RouteFilter{Prefix: pr, Match: "exact"}
				bits := pr.Addr().BitLen()
				switch {
				case rf.Has("orlonger"):
					r.Match = "orlonger"
				case rf.Has("longer"):
					r.Match = "longer"
				case rf.Leaf("upto") != "":
					r.Match, r.Lo, r.Hi = "range", pr.Bits(), atoi(strings.TrimPrefix(rf.Leaf("upto"), "/"), 0)
				case rf.Leaf("prefix-length-range") != "":
					a, z, _ := strings.Cut(rf.Leaf("prefix-length-range"), "-")
					r.Match, r.Lo, r.Hi = "range", atoi(strings.TrimPrefix(a, "/"), 0), atoi(strings.TrimPrefix(z, "/"), 0)
				}
				if r.Match == "range" && (r.Lo < pr.Bits() || r.Hi < r.Lo || r.Hi > bits) {
					b.errorf(tpath+" from route-filter "+rf.Key, "the prefix lengths must be between /%d and /%d, in order", pr.Bits(), bits)
				}
				t.From.RouteFilters = append(t.From.RouteFilters, r)
			}
			for _, pf := range f.Entries("prefix-list-filter") {
				t.From.PrefixListFilters = append(t.From.PrefixListFilters, PrefixListFilter{List: pf.Key, Match: orDefault(pf.Leaf("match"), "exact")})
			}
			th := te.Get("then")
			t.Then = PolicyThen{Metric: u32p(th.Leaf("metric")), MetricAdd: u32p(th.Leaf("metric-add")),
				LocalPref: u32p(th.Leaf("local-preference")), CommunityAdd: th.List("community", "add"),
				CommunityDelete: th.List("community", "delete"), CommunitySet: th.List("community", "set"),
				SetCommunity: th.Has("community", "set"), NextHop: th.Leaf("next-hop"), ExternalType: atoi(th.Leaf("external-type"), 0),
				Tag: u32p(th.Leaf("tag"))}
			if v := th.Leaf("preference"); v != "" {
				pv := atoi(v, 0)
				t.Then.Preference = &pv
			}
			for _, a := range strings.Fields(th.Leaf("as-path-prepend")) {
				t.Then.ASPathPrepend = append(t.Then.ASPathPrepend, u32(a))
			}
			switch {
			case th.Has("accept"):
				t.Then.Flow = "accept"
			case th.Has("reject"):
				t.Then.Flow = "reject"
			case th.Leaf("next") != "":
				t.Then.Flow = "next " + th.Leaf("next")
			}
			if unreachable {
				b.warnf(tpath, "never reached: an earlier term without 'from' always ends the evaluation")
			}
			if f == nil && (t.Then.Flow == "accept" || t.Then.Flow == "reject") {
				unreachable = true
			}
			ps.Terms = append(ps.Terms, t)
		}
		p.Statements[e.Key] = ps
	}
}

// validateRoutingProtocols checks the references between protocols and policies
// and the router id / AS rules.
func (b *builder) validateRoutingProtocols() {
	c := b.cfg
	p := c.Policies
	used := map[string]bool{}
	ref := func(path string, names []string) {
		for _, n := range names {
			used[n] = true
			if p.Statements[n] == nil {
				b.errorf(path, "policy %s is not defined (policy-options policy-statement %s)", n, n)
			}
		}
	}
	for _, r := range c.AllRouting() {
		base := "protocols"
		if r.Instance != "" {
			base = "routing-instances " + r.Instance + " protocols"
			if r.Instance == c.System.MgmtInstance && (r.OSPF != nil || r.OSPF3 != nil || r.BGP != nil) {
				b.errorf(base, "routing protocols do not run in the management instance")
			}
		}
		for _, o := range []*OSPF{r.OSPF, r.OSPF3} {
			if o == nil {
				continue
			}
			name := "ospf"
			if o.V3 {
				name = "ospf3"
			}
			ref(base+" "+name+" export", o.Export)
		}
		if r.BGP != nil {
			for _, g := range sortedKeys(r.BGP.Groups) {
				grp := r.BGP.Groups[g]
				for _, a := range sortedAddrs(grp.Neighbors) {
					nb := grp.Neighbors[a]
					np := fmt.Sprintf("%s bgp group %s neighbor %s", base, g, a)
					ref(np+" import", nb.Import)
					ref(np+" export", nb.Export)
				}
			}
		}
		if (r.OSPF != nil || r.OSPF3 != nil || r.BGP != nil) && !r.RouterID.IsValid() && c.autoRouterID(r.Instance) == (netip.Addr{}) {
			b.warnf(strings.TrimSuffix(base, " protocols")+" routing-options router-id",
				"no router id can be chosen (no IPv4 address in the instance): the routing protocols wait until there is one")
		}
	}
	for name, ps := range p.Statements {
		for _, t := range ps.Terms {
			tp := "policy-options policy-statement " + name + " term " + t.Name + " from"
			for _, l := range t.From.PrefixLists {
				if _, ok := p.PrefixLists[l]; !ok {
					b.errorf(tp+" prefix-list", "prefix list %s is not defined", l)
				}
			}
			for _, l := range t.From.PrefixListFilters {
				if _, ok := p.PrefixLists[l.List]; !ok {
					b.errorf(tp+" prefix-list-filter", "prefix list %s is not defined", l.List)
				}
			}
			for _, cm := range append(append(append(append([]string{}, t.From.Communities...), t.Then.CommunityAdd...), t.Then.CommunityDelete...), t.Then.CommunitySet...) {
				if _, ok := p.Communities[cm]; !ok {
					b.errorf("policy-options policy-statement "+name+" term "+t.Name, "community %s is not defined", cm)
				}
			}
			for _, a := range t.From.ASPaths {
				if _, ok := p.ASPaths[a]; !ok {
					b.errorf(tp+" as-path", "AS path %s is not defined", a)
				}
			}
		}
		if !used[name] {
			b.warnf("policy-options policy-statement "+name, "the policy is not used")
		}
	}
}

// AllRouting returns the routing configuration of every instance with
// routing (the default instance first).
func (c *Config) AllRouting() []*Routing {
	var out []*Routing
	if c.Routing != nil {
		out = append(out, c.Routing)
	}
	for _, n := range sortedKeys(c.Instances) {
		if r := c.Instances[n].Routing; r != nil {
			out = append(out, r)
		}
	}
	return out
}

// autoRouterID is the router id chosen when none is configured: the
// lowest IPv4 address of a member-less routed unit of the instance.
func (c *Config) autoRouterID(inst string) netip.Addr {
	var best netip.Addr
	for _, u := range c.L3 {
		if u.Instance != inst || u.CME() || u.Disabled {
			continue
		}
		for _, p := range u.Addrs {
			if _, one := u.AddrMember[p]; one || !p.Addr().Is4() {
				continue
			}
			if !best.IsValid() || p.Addr().Less(best) {
				best = p.Addr()
			}
		}
	}
	return best
}

// RouterID returns the configured or automatic router id of r.
func (c *Config) RouterID(r *Routing) netip.Addr {
	if r.RouterID.IsValid() {
		return r.RouterID
	}
	return c.autoRouterID(r.Instance)
}

func sortedAddrs[V any](m map[netip.Addr]V) []netip.Addr {
	out := make([]netip.Addr, 0, len(m))
	for a := range m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}

// ---- AS path expressions ----

// CompileASPath compiles a Junos AS path expression into a regular
// expression over the AS path written as " 65000 65001 " (every AS number
// with a space on both sides): "." is one AS number, the operators apply to
// whole AS numbers.
func CompileASPath(expr string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	anchoredStart := false
	toks := tokenizeASPath(expr)
	for i, t := range toks {
		switch {
		case t == "^":
			if i != 0 {
				return nil, fmt.Errorf("'^' only at the start of %q", expr)
			}
			anchoredStart = true
		case t == "$":
			if i != len(toks)-1 {
				return nil, fmt.Errorf("'$' only at the end of %q", expr)
			}
		case t == ".":
			b.WriteString(`(?: [0-9]+)`)
		case t == "(" || t == ")" || t == "|" || t == "*" || t == "+" || t == "?":
			if t == "(" {
				b.WriteString("(?:")
			} else {
				b.WriteString(t)
			}
		case strings.HasPrefix(t, "{"):
			b.WriteString(t)
		case strings.HasPrefix(t, "["):
			// [65000-65010] or [65000 65001]: a set of AS numbers.
			inner := strings.Trim(t, "[]")
			var alts []string
			for _, f := range strings.Fields(inner) {
				if lo, hi, ok := strings.Cut(f, "-"); ok {
					l, e1 := strconv.ParseUint(lo, 10, 32)
					h, e2 := strconv.ParseUint(hi, 10, 32)
					if e1 != nil || e2 != nil || h < l || h-l > 4096 {
						return nil, fmt.Errorf("invalid AS range %q", f)
					}
					for n := l; n <= h; n++ {
						alts = append(alts, strconv.FormatUint(n, 10))
					}
					continue
				}
				if _, err := strconv.ParseUint(f, 10, 32); err != nil {
					return nil, fmt.Errorf("invalid AS number %q", f)
				}
				alts = append(alts, f)
			}
			b.WriteString(`(?: (?:` + strings.Join(alts, "|") + `))`)
		default:
			if _, err := strconv.ParseUint(t, 10, 32); err != nil {
				return nil, fmt.Errorf("invalid element %q in AS path expression %q", t, expr)
			}
			b.WriteString(`(?: ` + t + `)`)
		}
	}
	re := b.String()
	if !anchoredStart {
		re = "^(?: [0-9]+)*" + re[1:]
	}
	if len(toks) == 0 || toks[len(toks)-1] != "$" {
		re += `(?: [0-9]+)*`
	}
	re += " ?$"
	return regexp.Compile(re)
}

func tokenizeASPath(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ':
			i++
		case c == '[' || c == '{':
			close := byte(']')
			if c == '{' {
				close = '}'
			}
			j := strings.IndexByte(s[i:], close)
			if j < 0 {
				out = append(out, s[i:])
				return out
			}
			out = append(out, s[i:i+j+1])
			i += j + 1
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			out = append(out, s[i:j])
			i = j
		default:
			out = append(out, string(c))
			i++
		}
	}
	return out
}

// MatchASPath reports whether the AS path matches the compiled expression.
func MatchASPath(re *regexp.Regexp, path []uint32) bool {
	var b strings.Builder
	for _, a := range path {
		b.WriteString(" ")
		b.WriteString(strconv.FormatUint(uint64(a), 10))
	}
	b.WriteString(" ")
	return re.MatchString(b.String())
}
