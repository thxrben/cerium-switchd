package ospf

import (
	"net/netip"
	"slices"
	"time"
)

// NbrState is a neighbour state (RFC 2328 §10.1).
type NbrState uint8

const (
	NbrDown NbrState = iota
	NbrAttempt
	NbrInit
	NbrTwoWay
	NbrExStart
	NbrExchange
	NbrLoading
	NbrFull
)

func (s NbrState) String() string {
	return [...]string{"Down", "Attempt", "Init", "2Way", "ExStart", "Exchange", "Loading", "Full"}[s]
}

type neighbor struct {
	ifc     *iface
	id      ID         // router id
	addr    netip.Addr // source address (OSPFv3: link-local)
	ifID    uint32     // OSPFv3: its interface id
	prio    uint8
	options uint32
	dr, bdr ID
	state   NbrState
	since   time.Time
	inactAt time.Time

	// Database exchange.
	master  bool
	ddSeq   uint32
	lastRx  *DD     // the last DD received (duplicate detection)
	lastTx  *Packet // the last DD sent (retransmission)
	ddAt    time.Time
	summary []LSAHeader // still to describe

	requests map[LSRef]LSAHeader
	reqAt    time.Time
	retrans  map[LSRef]*LSA
	rxmtAt   time.Time
	// lastSentBack: when a newer database copy was last sent back for an
	// LSA (RFC 2328 §13 step 8, at most once per MinLSArrival).
	sentBack map[LSRef]time.Time

	cryptoSeq     uint32
	cryptoSeqSeen bool
	Events        uint64 // state changes (show ospf neighbor detail)
}

// self is the neighbour's identity in DR/BDR fields.
func (n *neighbor) self() ID {
	if n.ifc.r.v == V2 {
		return IDFrom(n.addr)
	}
	return n.id
}

func (n *neighbor) setState(s NbrState) {
	if s == n.state {
		return
	}
	r := n.ifc.r
	r.Log.Debug("ospf neighbor state", "version", r.v, "interface", n.ifc.cfg.Name, "neighbor", n.id, "from", n.state, "to", s)
	if s == NbrFull || n.state == NbrFull {
		// The router (and network) LSAs list full neighbours only.
		r.dirty = true
		r.scheduleSPF()
		what := "down"
		if s == NbrFull {
			what = "up"
		}
		r.Log.Info("ospf adjacency "+what, "version", r.v, "interface", n.ifc.cfg.Name, "neighbor", n.id, "state", s)
	}
	twoWayChange := (s >= NbrTwoWay) != (n.state >= NbrTwoWay)
	n.state = s
	n.since = r.now
	n.Events++
	if twoWayChange {
		n.ifc.neighborChange()
	}
}

func (n *neighbor) clearLists() {
	n.summary = nil
	n.requests = map[LSRef]LSAHeader{}
	n.retrans = map[LSRef]*LSA{}
	n.lastRx, n.lastTx = nil, nil
}

// ---- events (RFC 2328 §10.3) ----

func (n *neighbor) helloReceived() {
	n.inactAt = n.ifc.r.now.Add(n.ifc.dead())
	if n.state == NbrDown {
		n.setState(NbrInit)
	}
}

func (n *neighbor) twoWay() {
	if n.state != NbrInit {
		return
	}
	if n.ifc.adjacent(n) {
		n.exStart()
	} else {
		n.setState(NbrTwoWay)
	}
}

func (n *neighbor) oneWay() {
	if n.state >= NbrTwoWay {
		n.clearLists()
		n.setState(NbrInit)
	}
}

// adjOK is the AdjOK? event.
func (n *neighbor) adjOK() {
	switch {
	case n.state == NbrTwoWay && n.ifc.adjacent(n):
		n.exStart()
	case n.state >= NbrExStart && !n.ifc.adjacent(n):
		n.clearLists()
		n.setState(NbrTwoWay)
	}
}

func (n *neighbor) kill() {
	n.clearLists()
	n.setState(NbrDown)
}

// seqMismatch is the SeqNumberMismatch and BadLSReq event.
func (n *neighbor) seqMismatch() {
	if n.state >= NbrExchange {
		n.ifc.r.Log.Debug("ospf exchange restarted", "interface", n.ifc.cfg.Name, "neighbor", n.id)
		n.clearLists()
		n.exStart()
	}
}

func (n *neighbor) exStart() {
	n.clearLists()
	n.setState(NbrExStart)
	if n.ddSeq == 0 {
		n.ddSeq = uint32(n.ifc.r.now.UnixNano()>>20) | 1
	} else {
		n.ddSeq++
	}
	n.master = true
	n.sendDD(DDInit | DDMore | DDMaster)
}

// ---- database exchange (RFC 2328 §10.6, §10.8) ----

func (n *neighbor) sendDD(flags uint8) {
	i := n.ifc
	d := &DD{MTU: uint16(i.mtu()), Options: i.r.options(), Flags: flags, Seq: n.ddSeq}
	if i.cfg.MTU == 0 {
		d.MTU = 0
	}
	if flags&DDInit == 0 {
		k := min(len(n.summary), i.maxHeaders())
		d.Headers = slices.Clone(n.summary[:k])
		n.summary = n.summary[k:]
		if len(n.summary) > 0 {
			d.Flags |= DDMore
		} else {
			d.Flags &^= DDMore
		}
	}
	if n.master {
		d.Flags |= DDMaster
	}
	p := &Packet{Type: TypeDD, DD: d}
	n.lastTx = p
	n.ddAt = i.r.now.Add(i.rxmt())
	i.r.send(i, i.unicast(n), p)
}

func (n *neighbor) resendDD() {
	if n.lastTx != nil {
		n.ddAt = n.ifc.r.now.Add(n.ifc.rxmt())
		n.ifc.r.send(n.ifc, n.ifc.unicast(n), n.lastTx)
	}
}

func sameDD(a, b *DD) bool {
	return a != nil && b != nil && a.Flags == b.Flags && a.Options == b.Options && a.Seq == b.Seq
}

func (n *neighbor) receiveDD(d *DD) {
	r := n.ifc.r
	if n.ifc.cfg.MTU != 0 && d.MTU > uint16(n.ifc.mtu()) {
		r.Stats.RxErrors++ // the neighbour's MTU is larger: no adjacency (RFC 2328 §10.6)
		return
	}
	switch n.state {
	case NbrDown, NbrAttempt, NbrTwoWay:
		return
	case NbrInit:
		n.twoWay()
		if n.state != NbrExStart {
			return
		}
		fallthrough
	case NbrExStart:
		switch {
		case d.Flags&(DDInit|DDMore|DDMaster) == DDInit|DDMore|DDMaster && len(d.Headers) == 0 && n.id > r.rid:
			n.master, n.ddSeq = false, d.Seq
		case d.Flags&(DDInit|DDMaster) == 0 && d.Seq == n.ddSeq && n.id < r.rid:
			n.master = true
		default:
			return
		}
		n.options = d.Options
		n.negotiationDone()
		n.acceptDD(d)
	case NbrExchange:
		if sameDD(d, n.lastRx) {
			if !n.master {
				n.resendDD()
			}
			return
		}
		if (d.Flags&DDMaster != 0) == n.master || d.Flags&DDInit != 0 || d.Options != n.options {
			n.seqMismatch()
			return
		}
		if (n.master && d.Seq != n.ddSeq) || (!n.master && d.Seq != n.ddSeq+1) {
			n.seqMismatch()
			return
		}
		n.acceptDD(d)
	case NbrLoading, NbrFull:
		if sameDD(d, n.lastRx) {
			if !n.master {
				n.resendDD()
			}
			return
		}
		n.seqMismatch()
	}
}

// negotiationDone: Exchange, with the summary of the databases this
// neighbour shares (area, AS and link scope); MaxAge LSAs go to the
// retransmission list instead (RFC 2328 §10.3).
func (n *neighbor) negotiationDone() {
	r := n.ifc.r
	n.setState(NbrExchange)
	n.summary = nil
	for _, s := range []*scope{n.ifc.sc, n.ifc.area.sc, r.as} {
		for _, l := range s.db.All(r.now) {
			if l.Age >= MaxAge {
				n.retrans[l.Ref()] = l
				continue
			}
			n.summary = append(n.summary, l.LSAHeader)
		}
	}
}

// acceptDD processes the headers of an accepted DD and continues the
// exchange (RFC 2328 §10.6, last part).
func (n *neighbor) acceptDD(d *DD) {
	r := n.ifc.r
	n.lastRx = d
	for _, h := range d.Headers {
		if r.v == V2 && !r.v.Known(h.Type) {
			n.seqMismatch()
			return
		}
		s := r.scopeFor(n.ifc, h.Type)
		if s == nil {
			continue
		}
		if have := s.db.Get(h.Ref(), r.now); have == nil || Compare(h, have.LSAHeader) > 0 {
			n.requests[h.Ref()] = h
		}
	}
	if n.master {
		n.ddSeq++
		if len(n.summary) == 0 && d.Flags&DDMore == 0 && (n.lastTx == nil || n.lastTx.DD.Flags&DDMore == 0) {
			n.exchangeDone()
			return
		}
		n.sendDD(0)
		return
	}
	n.ddSeq = d.Seq
	n.sendDD(0)
	if d.Flags&DDMore == 0 && n.lastTx.DD.Flags&DDMore == 0 {
		n.exchangeDone()
	}
}

func (n *neighbor) exchangeDone() {
	if len(n.requests) == 0 {
		n.setState(NbrFull)
		return
	}
	n.setState(NbrLoading)
	n.sendLSR()
}

// ---- requests (RFC 2328 §10.7, §10.9) ----

func (n *neighbor) sendLSR() {
	if len(n.requests) == 0 {
		return
	}
	refs := make([]LSRef, 0, len(n.requests))
	for ref := range n.requests {
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, compareRef)
	k := min(len(refs), max(1, (n.ifc.payload()-n.ifc.r.v.headerLen())/12))
	n.reqAt = n.ifc.r.now.Add(n.ifc.rxmt())
	n.ifc.r.send(n.ifc, n.ifc.unicast(n), &Packet{Type: TypeLSR, LSR: refs[:k]})
}

func (n *neighbor) receiveLSR(refs []LSRef) {
	if n.state < NbrExchange {
		return
	}
	r := n.ifc.r
	var out []*LSA
	for _, ref := range refs {
		s := r.scopeFor(n.ifc, ref.Type)
		var l *LSA
		if s != nil {
			l = s.db.Get(ref, r.now)
		}
		if l == nil {
			n.seqMismatch() // BadLSReq
			return
		}
		out = append(out, l)
	}
	n.sendLSU(out, n.ifc.unicast(n))
}

// sendLSU sends LSAs in as many packets as needed, their ages increased
// by the interface's transit delay.
func (n *neighbor) sendLSU(lsas []*LSA, dst netip.Addr) { n.ifc.sendLSU(lsas, dst) }

func (i *iface) sendLSU(lsas []*LSA, dst netip.Addr) {
	room := i.payload() - i.r.v.headerLen() - 4
	var batch []*LSA
	size := 0
	flush := func() {
		if len(batch) > 0 {
			i.r.send(i, dst, &Packet{Type: TypeLSU, LSU: batch})
			batch, size = nil, 0
		}
	}
	for _, l := range lsas {
		age := min(int(l.Age)+int(max(i.cfg.TransitDelay, 1)), MaxAge)
		c := l.WithAge(uint16(age))
		if size+len(c.Raw) > room {
			flush()
		}
		batch = append(batch, c)
		size += len(c.Raw)
	}
	flush()
}

// receiveAck removes acknowledged LSAs from the retransmission list.
func (n *neighbor) receiveAck(hs []LSAHeader) {
	if n.state < NbrExchange {
		return
	}
	for _, h := range hs {
		if l, ok := n.retrans[h.Ref()]; ok && Compare(h, l.LSAHeader) == 0 {
			delete(n.retrans, h.Ref())
		}
	}
}

// ---- timers ----

func (n *neighbor) tick() {
	r := n.ifc.r
	now := r.now
	if n.state > NbrDown && !now.Before(n.inactAt) {
		r.Log.Info("ospf neighbor dead", "version", r.v, "interface", n.ifc.cfg.Name, "neighbor", n.id)
		n.kill()
		delete(n.ifc.nbrs, n.ifc.nbrKey(n.addr, n.id))
		return
	}
	if (n.state == NbrExStart || (n.state == NbrExchange && n.master)) && !now.Before(n.ddAt) {
		n.resendDD()
	}
	if n.state >= NbrExchange && len(n.requests) > 0 && !now.Before(n.reqAt) {
		n.sendLSR()
	}
	if n.state >= NbrExchange && len(n.retrans) > 0 && !now.Before(n.rxmtAt) {
		var out []*LSA
		for ref, l := range n.retrans {
			// The current database copy, aged.
			if s := r.scopeFor(n.ifc, ref.Type); s != nil {
				if cur := s.db.Get(ref, now); cur != nil && Compare(cur.LSAHeader, l.LSAHeader) == 0 {
					l = cur
				}
			}
			out = append(out, l)
		}
		slices.SortFunc(out, func(a, b *LSA) int { return compareRef(a.Ref(), b.Ref()) })
		n.rxmtAt = now.Add(n.ifc.rxmt())
		n.sendLSU(out, n.ifc.unicast(n))
	}
}

// addRetrans puts an LSA on the retransmission list (sent at the next
// retransmission interval unless acknowledged).
func (n *neighbor) addRetrans(l *LSA) {
	if len(n.retrans) == 0 {
		n.rxmtAt = n.ifc.r.now.Add(n.ifc.rxmt())
	}
	n.retrans[l.Ref()] = l
}
