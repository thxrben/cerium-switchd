//go:build ospfwip

// Work in progress: excluded from the build until the neighbour state
// machine, flooding, origination and SPF exist.

package ospf

import (
	"log/slog"
	"net/netip"
	"slices"
	"time"
)

// IO sends packets. dst is a multicast group (AllSPFRouters, AllDRouters)
// or a neighbour's address; the IO layer uses the interface's primary
// address as source, TTL 1 and the internetwork-control TOS.
type IO interface {
	Send(iface string, dst netip.Addr, pkt []byte)
}

// IfaceConfig is an OSPF interface (reference 5.13).
type IfaceConfig struct {
	Name   string     // unit name ("irb.10", "1/0/5.0")
	Area   netip.Addr // area id
	Prefix netip.Prefix
	// Secondary subnets of the unit, announced as stub networks.
	Secondary    []netip.Prefix
	P2P          bool
	Passive      bool
	Cost         uint16
	Priority     uint8
	Hello        uint16 // seconds
	Dead         uint32
	Retransmit   uint16
	TransitDelay uint16
	MTU          uint16 // 0: not checked
	Auth         *Auth
	// Up: the interface has carrier and its address.
	Up bool
}

// External is a route redistributed into OSPF (export policy applied).
type External struct {
	Prefix  netip.Prefix
	Metric  uint32
	Type1   bool
	Tag     uint32
	Forward netip.Addr // invalid: this router
}

// Config is the configuration of a router (one instance, OSPFv2).
type Config struct {
	RouterID   netip.Addr
	Interfaces []IfaceConfig
	Externals  []External
	Overload   bool
}

// PathType is the type of an OSPF route (RFC 2328 §11).
type PathType uint8

const (
	IntraArea PathType = iota
	InterArea
	External1
	External2
)

func (t PathType) String() string {
	return [...]string{"Intra", "Inter", "Ext1", "Ext2"}[t&3]
}

// NextHop is a next hop of an OSPF route.
type NextHop struct {
	Iface   string
	Gateway netip.Addr // invalid: directly connected
}

// Route is a route computed by SPF.
type Route struct {
	Prefix   netip.Prefix
	Type     PathType
	Area     netip.Addr
	Cost     uint32 // for Ext2: the cost to the ASBR (tie breaker)
	Cost2    uint32 // Ext2: the external metric
	Tag      uint32
	NextHops []NextHop
	// Direct: the network is attached to this router (the RIB has it as a
	// direct route; it is not installed).
	Direct bool
}

// Router is an OSPFv2 router: one routing instance. It is not safe for
// concurrent use; the owner serialises Configure, Receive and Tick.
type Router struct {
	io  IO
	Log *slog.Logger
	// OnRoutes receives the complete routing table after every change.
	OnRoutes func([]Route)

	cfg    Config
	rid    netip.Addr
	now    time.Time
	areas  map[netip.Addr]*area
	ifaces map[string]*iface
	ext    *LSDB // AS-scope (type 5)
	// Self-originated LSAs: last origination time (MinLSInterval).
	origAt map[LSRef]time.Time
	// maxAgeFlooded: MaxAge LSAs already flooded (aged out naturally).
	maxAgeFlooded map[LSRef]bool

	dirty      bool // re-originate own LSAs
	spfPending bool
	spfAt      time.Time
	routes     map[netip.Prefix]*Route
	cryptoSeq  uint32

	Stats Stats
}

// Stats are counters for show ospf statistics/overview.
type Stats struct {
	Rx, Tx       [6]uint64 // by packet type
	RxErrors     uint64
	AuthFailures uint64
	SPFRuns      uint64
	LastSPF      time.Time
	SPFDuration  time.Duration
}

type area struct {
	id     netip.Addr
	db     *LSDB
	ifaces []*iface
	// SPF results: routers reachable in the area (for summaries and
	// externals).
	routers map[netip.Addr]*spfRouter
}

// spfDelay is the delay between a database change and SPF (changes in
// quick succession are computed once).
const spfDelay = 200 * time.Millisecond

// New returns a router; Configure starts it.
func New(io IO, log *slog.Logger, now time.Time) *Router {
	if log == nil {
		log = slog.Default()
	}
	return &Router{io: io, Log: log, now: now, areas: map[netip.Addr]*area{}, ifaces: map[string]*iface{},
		ext: NewLSDB(), origAt: map[LSRef]time.Time{}, maxAgeFlooded: map[LSRef]bool{}, routes: map[netip.Prefix]*Route{},
		cryptoSeq: uint32(now.Unix())}
}

// RouterID returns the router id in use.
func (r *Router) RouterID() netip.Addr { return r.rid }

// Configure applies a configuration hitlessly: interfaces that did not
// change keep their adjacencies. A router id change restarts everything.
func (r *Router) Configure(cfg Config, now time.Time) {
	r.now = now
	if cfg.RouterID != r.rid {
		for _, i := range r.ifaces {
			i.down()
		}
		r.ifaces = map[string]*iface{}
		r.areas = map[netip.Addr]*area{}
		r.ext = NewLSDB()
		r.origAt = map[LSRef]time.Time{}
		r.rid = cfg.RouterID
	}
	r.cfg = cfg
	want := map[string]IfaceConfig{}
	for _, c := range cfg.Interfaces {
		want[c.Name] = c
	}
	// Removed interfaces, and interfaces that moved to another area or
	// changed their addressing or type: down (their neighbours go away).
	for name, i := range r.ifaces {
		c, ok := want[name]
		if !ok || c.Area != i.cfg.Area || c.Prefix != i.cfg.Prefix || c.P2P != i.cfg.P2P || c.Passive != i.cfg.Passive {
			i.down()
			r.removeIface(i)
		}
	}
	for _, c := range cfg.Interfaces {
		a := r.areas[c.Area]
		if a == nil {
			a = &area{id: c.Area, db: NewLSDB()}
			r.areas[c.Area] = a
		}
		i := r.ifaces[c.Name]
		if i == nil {
			i = &iface{r: r, area: a, cfg: c, nbrs: map[netip.Addr]*neighbor{}}
			r.ifaces[c.Name] = i
			a.ifaces = append(a.ifaces, i)
			if c.Up {
				i.up()
			}
			continue
		}
		old := i.cfg
		i.cfg = c
		switch {
		case c.Up && i.state == IfDown:
			i.up()
		case !c.Up && i.state != IfDown:
			i.down()
		case i.state != IfDown && old.Priority != c.Priority && !c.P2P:
			i.neighborChange()
		}
		if old.Hello != c.Hello && i.state != IfDown {
			i.helloAt = now
		}
	}
	// Areas without interfaces are gone (their LSAs with them).
	for id, a := range r.areas {
		if len(a.ifaces) == 0 {
			delete(r.areas, id)
		}
	}
	r.dirty = true
	r.scheduleSPF()
	r.settle()
}

func (r *Router) removeIface(i *iface) {
	delete(r.ifaces, i.cfg.Name)
	i.area.ifaces = slices.DeleteFunc(i.area.ifaces, func(x *iface) bool { return x == i })
}

// SetExternals replaces the redistributed routes.
func (r *Router) SetExternals(ext []External, now time.Time) {
	r.now = now
	r.cfg.Externals = ext
	r.dirty = true
	r.settle()
}

// Receive processes a packet received on an interface (src: the IP
// source, dst: the IP destination).
func (r *Router) Receive(ifname string, src, dst netip.Addr, raw []byte, now time.Time) {
	r.now = now
	defer r.settle()
	i := r.ifaces[ifname]
	if i == nil || i.state == IfDown || i.cfg.Passive {
		return
	}
	p, err := Decode(raw)
	if err != nil {
		r.Stats.RxErrors++
		return
	}
	if int(p.Type) < len(r.Stats.Rx) {
		r.Stats.Rx[p.Type]++
	}
	// RFC 2328 §8.2.
	switch {
	case p.AreaID != i.area.id, p.RouterID == r.rid:
		return
	case dst == AllDRouters && i.state != IfDR && i.state != IfBackup:
		return
	case !i.cfg.P2P && !i.cfg.Prefix.Contains(src):
		return
	}
	if !i.cfg.Auth.Verify(raw, p) {
		r.Stats.AuthFailures++
		return
	}
	if p.Type == TypeHello {
		i.receiveHello(src, p)
		return
	}
	n := i.neighborFor(src, p.RouterID)
	if n == nil {
		return
	}
	if p.AuType == AuthCrypto {
		if n.cryptoSeqSeen && p.CryptoSeq < n.cryptoSeq {
			r.Stats.AuthFailures++
			return
		}
		n.cryptoSeq, n.cryptoSeqSeen = p.CryptoSeq, true
	}
	switch p.Type {
	case TypeDD:
		n.receiveDD(p.DD)
	case TypeLSR:
		n.receiveLSR(p.LSR)
	case TypeLSU:
		n.receiveLSU(p.LSU)
	case TypeLSAck:
		n.receiveAck(p.Ack)
	}
}

// Tick runs the timers; the owner calls it often (every 100 ms).
func (r *Router) Tick(now time.Time) {
	r.now = now
	defer r.settle()
	for _, i := range r.sortedIfaces() {
		i.tick()
	}
	r.age()
}

// settle finishes an event: own LSAs are re-originated when something
// changed, and SPF runs when it is due.
func (r *Router) settle() {
	if r.dirty {
		r.dirty = false
		r.originateAll()
	}
	if r.spfPending && !r.now.Before(r.spfAt) {
		r.spfPending = false
		r.runSPF()
		// Summaries depend on the routes.
		r.originateAll()
	}
}

func (r *Router) scheduleSPF() {
	if !r.spfPending {
		r.spfPending = true
		r.spfAt = r.now.Add(spfDelay)
	}
}

func (r *Router) sortedIfaces() []*iface {
	out := make([]*iface, 0, len(r.ifaces))
	for _, i := range r.ifaces {
		out = append(out, i)
	}
	slices.SortFunc(out, func(a, b *iface) int {
		if a.cfg.Name < b.cfg.Name {
			return -1
		}
		if a.cfg.Name > b.cfg.Name {
			return 1
		}
		return 0
	})
	return out
}

func (r *Router) sortedAreas() []*area {
	out := make([]*area, 0, len(r.areas))
	for _, a := range r.areas {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b *area) int { return a.id.Compare(b.id) })
	return out
}

// isABR: interfaces in more than one area, one of them the backbone.
func (r *Router) isABR() bool {
	if len(r.areas) < 2 {
		return false
	}
	_, ok := r.areas[netip.IPv4Unspecified()]
	return ok
}

func (r *Router) isASBR() bool { return len(r.cfg.Externals) > 0 }

// send encodes and sends a packet on an interface.
func (r *Router) send(i *iface, dst netip.Addr, p *Packet) {
	p.RouterID, p.AreaID = r.rid, i.area.id
	if i.cfg.Auth != nil && i.cfg.Auth.Type == AuthCrypto {
		if s := uint32(r.now.Unix()); s > r.cryptoSeq {
			r.cryptoSeq = s
		} else {
			r.cryptoSeq++
		}
		p.CryptoSeq = r.cryptoSeq
	}
	if int(p.Type) < len(r.Stats.Tx) {
		r.Stats.Tx[p.Type]++
	}
	r.io.Send(i.cfg.Name, dst, p.Encode(i.cfg.Auth))
}

// dbFor returns the database an LSA belongs to (nil: unknown type).
func (r *Router) dbFor(a *area, t uint8) *LSDB {
	switch t {
	case LSARouter, LSANetwork, LSASummaryNet, LSASummaryASBR:
		return a.db
	case LSAExternal:
		return r.ext
	}
	return nil
}

// age handles LSAs that reached MaxAge (RFC 2328 §14): they are flooded
// once and removed when no neighbour still needs them; own LSAs are
// refreshed before that.
func (r *Router) age() {
	scopes := []struct {
		db *LSDB
		a  *area
	}{{r.ext, nil}}
	for _, a := range r.sortedAreas() {
		scopes = append(scopes, struct {
			db *LSDB
			a  *area
		}{a.db, a})
	}
	for _, s := range scopes {
		for _, ref := range s.db.MaxAged(r.now) {
			l := s.db.Get(ref, r.now)
			if !r.maxAgeFlooded[ref] {
				r.maxAgeFlooded[ref] = true
				r.flood(l, s.a, nil, nil)
				r.scheduleSPF()
			}
			if !r.maxAgeBusy(ref) {
				s.db.Delete(ref)
				delete(r.maxAgeFlooded, ref)
			}
		}
	}
	r.refreshOwn()
}

// maxAgeBusy: an LSA is still on a retransmission list, or a neighbour is
// exchanging databases (RFC 2328 §14).
func (r *Router) maxAgeBusy(ref LSRef) bool {
	for _, i := range r.ifaces {
		for _, n := range i.nbrs {
			if n.state == NbrExchange || n.state == NbrLoading {
				return true
			}
			if _, ok := n.retrans[ref]; ok {
				return true
			}
		}
	}
	return false
}
