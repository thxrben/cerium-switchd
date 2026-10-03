package ospf

import (
	"cmp"
	"log/slog"
	"net/netip"
	"slices"
	"time"
)

// Router is an OSPF router of one version in one routing instance. It is
// not safe for concurrent use; the owner serialises Configure,
// SetExternals, Receive and Tick (and the show functions).
type Router struct {
	v   Version
	io  IO
	Log *slog.Logger
	// OnRoutes receives the complete routing table after every change.
	OnRoutes func([]Route)

	cfg    Config
	rid    ID
	now    time.Time
	areas  map[ID]*area
	ifaces map[string]*iface
	as     *scope // AS-scope LSAs (externals)

	// origAt: when an own LSA was last originated (MinLSInterval);
	// deferred: own LSAs waiting for it.
	origAt   map[LSRef]time.Time
	hold     map[LSRef]time.Duration
	deferred map[LSRef]bool
	// extIDs: the LS ids of external (and v3 inter-area) prefixes.
	prefixIDs map[prefixKey]ID

	dirty      bool // re-originate own LSAs
	spfPending bool
	spfAt      time.Time
	routes     []Route
	cryptoSeq  uint32
	started    time.Time

	Stats Stats
}

type prefixKey struct {
	t LSType
	p netip.Prefix
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

// scope is a flooding scope: a database and where its LSAs are flooded.
type scope struct {
	db   *LSDB
	area *area  // area scope
	ifc  *iface // link scope
	// maxAgeFlooded: LSAs that reached MaxAge and were flooded once.
	maxAgeFlooded map[LSRef]bool
}

func newScope(a *area, i *iface) *scope {
	return &scope{db: NewLSDB(), area: a, ifc: i, maxAgeFlooded: map[LSRef]bool{}}
}

type area struct {
	id     ID
	sc     *scope
	ifaces []*iface
	// SPF results of the area: the reachable routers (ABRs and ASBRs).
	spf *spfResult
	// transit: some router of the area announced a full adjacency (for
	// summaries: whether the area has any routers besides this one).
}

// spfDelay is the delay between a database change and SPF (changes in
// quick succession are computed once).
const spfDelay = 200 * time.Millisecond

// New returns a router of version v; Configure starts it.
func New(v Version, io IO, log *slog.Logger, now time.Time) *Router {
	if log == nil {
		log = slog.Default()
	}
	r := &Router{v: v, io: io, Log: log, now: now, started: now}
	r.reset()
	r.cryptoSeq = uint32(now.Unix())
	return r
}

func (r *Router) reset() {
	r.areas = map[ID]*area{}
	r.ifaces = map[string]*iface{}
	r.as = newScope(nil, nil)
	r.origAt = map[LSRef]time.Time{}
	r.hold = map[LSRef]time.Duration{}
	r.deferred = map[LSRef]bool{}
	r.prefixIDs = map[prefixKey]ID{}
	r.routes = nil
}

// Version returns the router's OSPF version.
func (r *Router) Version() Version { return r.v }

// RouterID returns the router id in use.
func (r *Router) RouterID() ID { return r.rid }

// Configure applies a configuration hitlessly: interfaces that did not
// change keep their adjacencies. A router id change restarts everything.
func (r *Router) Configure(cfg Config, now time.Time) {
	r.now = now
	cfg.Version = r.v
	if cfg.RouterID != r.rid {
		for _, i := range r.sortedIfaces() {
			i.down()
		}
		r.reset()
		r.rid = cfg.RouterID
	}
	r.cfg = cfg
	want := map[string]IfaceConfig{}
	for _, c := range cfg.Interfaces {
		want[c.Name] = c
	}
	// Removed interfaces, and interfaces that moved to another area or
	// changed their addressing or type: down (their neighbours go away).
	for _, i := range r.sortedIfaces() {
		c, ok := want[i.cfg.Name]
		if !ok || c.Area != i.cfg.Area || c.Addr != i.cfg.Addr || c.ID != i.cfg.ID || c.P2P != i.cfg.P2P ||
			c.Passive != i.cfg.Passive || c.InstanceID != i.cfg.InstanceID ||
			(r.v == V2 && len(c.Prefixes) > 0 && len(i.cfg.Prefixes) > 0 && c.Prefixes[0] != i.cfg.Prefixes[0]) {
			i.down()
			r.removeIface(i)
		}
	}
	for _, c := range cfg.Interfaces {
		a := r.areas[c.Area]
		if a == nil {
			a = &area{id: c.Area}
			a.sc = newScope(a, nil)
			r.areas[c.Area] = a
		}
		i := r.ifaces[c.Name]
		if i == nil {
			i = &iface{r: r, area: a, cfg: c, nbrs: map[ID]*neighbor{}}
			i.sc = newScope(a, i)
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

// SetOverload switches the overload announcement (RFC 6987).
func (r *Router) SetOverload(on bool, now time.Time) {
	r.now = now
	if r.cfg.Overload != on {
		r.cfg.Overload = on
		r.dirty = true
	}
	r.settle()
}

// Receive processes a packet received on an interface (src: the IP
// source, dst: the IP destination).
func (r *Router) Receive(ifname string, src, dst netip.Addr, raw []byte, now time.Time) {
	r.now = now
	defer r.settle()
	i := r.ifaces[ifname]
	if i == nil || i.state == IfDown || i.state == IfPassive {
		return
	}
	p, err := Decode(r.v, raw)
	if err != nil {
		r.Stats.RxErrors++
		return
	}
	if int(p.Type) < len(r.Stats.Rx) {
		r.Stats.Rx[p.Type]++
	}
	// RFC 2328 §8.2 / RFC 5340 §4.2.2.
	switch {
	case p.AreaID != i.area.id, p.RouterID == r.rid:
		return
	case r.v == V3 && p.InstanceID != i.cfg.InstanceID:
		return
	case dst == r.v.AllDR() && i.state != IfDR && i.state != IfBackup:
		return
	case r.v == V2 && !i.cfg.P2P && len(i.cfg.Prefixes) > 0 && !i.cfg.Prefixes[0].Contains(src):
		return
	case r.v == V3 && !src.IsLinkLocalUnicast():
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
	r.originateDeferred()
}

// settle finishes an event: own LSAs are re-originated when something
// changed, and SPF runs when it is due.
func (r *Router) settle() {
	for range 4 { // origination and SPF feed each other (summaries)
		if r.dirty {
			r.dirty = false
			r.originateAll()
		}
		if !r.spfPending || r.now.Before(r.spfAt) {
			return
		}
		r.spfPending = false
		r.runSPF()
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
	slices.SortFunc(out, func(a, b *iface) int { return cmp.Compare(a.cfg.Name, b.cfg.Name) })
	return out
}

func (r *Router) sortedAreas() []*area {
	out := make([]*area, 0, len(r.areas))
	for _, a := range r.areas {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b *area) int { return cmp.Compare(a.id, b.id) })
	return out
}

// isABR: interfaces in more than one area, one of them the backbone
// (RFC 3509 is not needed: an ABR must have the backbone, reference 5.13).
func (r *Router) isABR() bool {
	if len(r.areas) < 2 {
		return false
	}
	_, ok := r.areas[Backbone]
	return ok
}

func (r *Router) isASBR() bool { return len(r.cfg.Externals) > 0 }

// options are the options this router announces.
func (r *Router) options() uint32 {
	if r.v == V3 {
		return OptV6 | OptE | OptR
	}
	return OptE
}

// send encodes and sends a packet on an interface.
func (r *Router) send(i *iface, dst netip.Addr, p *Packet) {
	p.V, p.RouterID, p.AreaID, p.InstanceID = r.v, r.rid, i.area.id, i.cfg.InstanceID
	if r.v == V2 && i.cfg.Auth != nil && i.cfg.Auth.Type == AuthCrypto {
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

// scopeFor returns the scope an LSA received on interface i belongs to
// (nil: not accepted: an OSPFv2 LSA of an unknown type). An unknown
// OSPFv3 LSA without the U bit is treated as link-local (RFC 5340 §4.5.2).
func (r *Router) scopeFor(i *iface, t LSType) *scope {
	if !r.v.Known(t) {
		if r.v == V2 {
			return nil
		}
		if t&0x8000 == 0 {
			return i.sc
		}
	}
	switch r.v.Scope(t) {
	case ScopeLink:
		return i.sc
	case ScopeAS:
		return r.as
	}
	return i.area.sc
}

// scopes returns every scope (for aging and show).
func (r *Router) scopes() []*scope {
	out := []*scope{r.as}
	for _, a := range r.sortedAreas() {
		out = append(out, a.sc)
		for _, i := range a.ifaces {
			out = append(out, i.sc)
		}
	}
	return out
}

// floodIfaces returns the interfaces a scope floods over.
func (r *Router) floodIfaces(s *scope) []*iface {
	switch {
	case s.ifc != nil:
		return []*iface{s.ifc}
	case s.area != nil:
		out := slices.Clone(s.area.ifaces)
		slices.SortFunc(out, func(a, b *iface) int { return cmp.Compare(a.cfg.Name, b.cfg.Name) })
		return out
	}
	return r.sortedIfaces()
}

// age handles LSAs that reached MaxAge (RFC 2328 §14): they are flooded
// once and removed when no neighbour still needs them; own LSAs are
// refreshed before that.
func (r *Router) age() {
	for _, s := range r.scopes() {
		for _, ref := range s.db.MaxAged(r.now) {
			if !s.maxAgeFlooded[ref] {
				s.maxAgeFlooded[ref] = true
				r.flood(s, s.db.Get(ref, r.now), nil, nil)
				r.scheduleSPF()
			}
			if !r.maxAgeBusy(s, ref) {
				s.db.Delete(ref)
				delete(s.maxAgeFlooded, ref)
			}
		}
	}
	r.refreshOwn()
}

// maxAgeBusy: an LSA is still on a retransmission list, or a neighbour of
// the scope is exchanging databases (RFC 2328 §14).
func (r *Router) maxAgeBusy(s *scope, ref LSRef) bool {
	for _, i := range r.floodIfaces(s) {
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

// Overloaded reports whether the router announces overload.
func (r *Router) Overloaded() bool { return r.cfg.Overload }

// Exchanging reports whether a neighbour is still exchanging databases
// (ExStart, Exchange or Loading).
func (r *Router) Exchanging() bool {
	for _, i := range r.ifaces {
		for _, n := range i.nbrs {
			if n.state >= NbrExStart && n.state < NbrFull {
				return true
			}
		}
	}
	return false
}

// Shutdown sends hellos without neighbours on every interface, so the
// neighbours drop their adjacencies at once (1-Way), and stops the router
// sending.
func (r *Router) Shutdown(now time.Time) {
	r.now = now
	for _, i := range r.sortedIfaces() {
		if i.state == IfDown || i.state == IfPassive {
			continue
		}
		h := &Hello{MaskBits: i.prefixBits(), InterfaceID: i.cfg.ID, Interval: i.cfg.Hello, Options: r.options(),
			Priority: i.cfg.Priority, Dead: i.cfg.Dead}
		r.send(i, r.v.AllSPF(), &Packet{Type: TypeHello, Hello: h})
	}
	for _, i := range r.sortedIfaces() {
		i.down()
	}
}
