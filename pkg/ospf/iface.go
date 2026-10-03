package ospf

import (
	"cmp"
	"net/netip"
	"slices"
	"time"
)

// IfState is an interface state (RFC 2328 §9.1); Passive is switchd's
// state of a passive interface (announced, no neighbours).
type IfState uint8

const (
	IfDown IfState = iota
	IfWaiting
	IfP2P
	IfDROther
	IfBackup
	IfDR
	IfPassive
)

func (s IfState) String() string {
	return [...]string{"Down", "Waiting", "PtToPt", "DROther", "BDR", "DR", "Passive"}[s]
}

type iface struct {
	r     *Router
	area  *area
	sc    *scope // link-scope LSAs (OSPFv3 link LSAs)
	cfg   IfaceConfig
	state IfState
	// dr and bdr: OSPFv2 the interface addresses, OSPFv3 the router ids.
	dr, bdr ID
	// nbrs: OSPFv2 broadcast by address, otherwise by router id.
	nbrs    map[ID]*neighbor
	helloAt time.Time
	waitAt  time.Time
	acks    []LSAHeader // delayed acknowledgments
	ackAt   time.Time
	since   time.Time
}

func (i *iface) hello() time.Duration { return time.Duration(i.cfg.Hello) * time.Second }

func (i *iface) dead() time.Duration { return time.Duration(i.cfg.Dead) * time.Second }

func (i *iface) rxmt() time.Duration {
	if i.cfg.Retransmit == 0 {
		return 5 * time.Second
	}
	return time.Duration(i.cfg.Retransmit) * time.Second
}

// self is this router's identity in DR/BDR fields on the interface.
func (i *iface) self() ID {
	if i.r.v == V2 {
		return IDFrom(i.cfg.Addr)
	}
	return i.r.rid
}

// prefixBits is the OSPFv2 interface's prefix length (hello mask).
func (i *iface) prefixBits() int {
	if len(i.cfg.Prefixes) == 0 {
		return 32
	}
	return i.cfg.Prefixes[0].Bits()
}

// up is the InterfaceUp event.
func (i *iface) up() {
	i.since = i.r.now
	i.dr, i.bdr = 0, 0
	switch {
	case i.cfg.Passive:
		i.setState(IfPassive)
		return
	case i.cfg.P2P:
		i.setState(IfP2P)
	case i.cfg.Priority == 0:
		i.setState(IfDROther)
	default:
		i.setState(IfWaiting)
		i.waitAt = i.r.now.Add(i.dead())
	}
	i.helloAt = i.r.now // first hello at once
}

// down is the InterfaceDown event: every neighbour is killed and the
// link-scope LSAs are gone.
func (i *iface) down() {
	for _, n := range i.sortedNbrs() {
		n.kill()
	}
	i.nbrs = map[ID]*neighbor{}
	i.acks = nil
	i.dr, i.bdr = 0, 0
	i.sc = newScope(i.area, i)
	i.setState(IfDown)
}

func (i *iface) setState(s IfState) {
	if s == i.state {
		return
	}
	i.r.Log.Debug("ospf interface state", "version", i.r.v, "interface", i.cfg.Name, "from", i.state, "to", s)
	i.state = s
	i.since = i.r.now
	i.r.dirty = true
	i.r.scheduleSPF()
}

func (i *iface) sortedNbrs() []*neighbor {
	out := make([]*neighbor, 0, len(i.nbrs))
	for _, n := range i.nbrs {
		out = append(out, n)
	}
	slices.SortFunc(out, func(a, b *neighbor) int { return cmp.Compare(a.id, b.id) })
	return out
}

// nbrKey is the key of a neighbour: OSPFv2 on broadcast networks its
// address, otherwise its router id (RFC 2328 §10.5, RFC 5340 §4.2.2.1).
func (i *iface) nbrKey(src netip.Addr, rid ID) ID {
	if i.r.v == V2 && !i.cfg.P2P {
		return IDFrom(src)
	}
	return rid
}

// neighborFor finds the neighbour a non-hello packet came from.
func (i *iface) neighborFor(src netip.Addr, rid ID) *neighbor {
	return i.nbrs[i.nbrKey(src, rid)]
}

func (i *iface) tick() {
	now := i.r.now
	if i.state == IfDown || i.state == IfPassive {
		return
	}
	if !now.Before(i.helloAt) {
		i.sendHello()
		i.helloAt = now.Add(i.hello())
	}
	if i.state == IfWaiting && !now.Before(i.waitAt) {
		i.electDR()
	}
	for _, n := range i.sortedNbrs() {
		n.tick()
	}
	if len(i.acks) > 0 && !now.Before(i.ackAt) {
		i.flushAcks()
	}
}

func (i *iface) sendHello() {
	h := &Hello{MaskBits: i.prefixBits(), InterfaceID: i.cfg.ID, Interval: i.cfg.Hello, Options: i.r.options(),
		Priority: i.cfg.Priority, Dead: i.cfg.Dead, DR: i.dr, BDR: i.bdr}
	if i.cfg.P2P {
		h.Priority = 0
		if i.r.v == V2 {
			h.MaskBits = 0 // RFC 2328 §9.5: unnumbered/p2p mask is 0
		}
	}
	for _, n := range i.sortedNbrs() {
		if n.state >= NbrInit {
			h.Neighbors = append(h.Neighbors, n.id)
		}
	}
	i.r.send(i, i.r.v.AllSPF(), &Packet{Type: TypeHello, Hello: h})
}

// receiveHello processes a hello (RFC 2328 §10.5, RFC 5340 §4.2.2.1).
func (i *iface) receiveHello(src netip.Addr, p *Packet) {
	h := p.Hello
	if h.Interval != i.cfg.Hello || h.Dead != i.cfg.Dead || h.Options&OptE == 0 {
		i.r.Stats.RxErrors++
		return
	}
	if i.r.v == V2 && !i.cfg.P2P && h.MaskBits != i.prefixBits() {
		i.r.Stats.RxErrors++
		return
	}
	key := i.nbrKey(src, p.RouterID)
	n := i.nbrs[key]
	if n == nil {
		n = &neighbor{ifc: i, id: p.RouterID, addr: src, state: NbrDown, retrans: map[LSRef]*LSA{}, requests: map[LSRef]LSAHeader{}}
		i.nbrs[key] = n
	}
	if n.state == NbrDown {
		// A new neighbour hears from us at once instead of after up to a
		// hello interval (2-Way in one round trip).
		i.helloAt = i.r.now
	}
	n.id, n.addr, n.ifID = p.RouterID, src, h.InterfaceID
	oldPrio, oldDR, oldBDR := n.prio, n.dr, n.bdr
	n.prio, n.options = h.Priority, h.Options
	n.dr, n.bdr = h.DR, h.BDR
	n.helloReceived()
	if !slices.Contains(h.Neighbors, i.r.rid) {
		n.oneWay()
		return
	}
	n.twoWay()
	if i.cfg.P2P {
		return
	}
	me := n.self()
	change := n.prio != oldPrio
	declDR, wasDR := n.dr == me, oldDR == me
	declBDR, wasBDR := n.bdr == me, oldBDR == me
	backupSeen := false
	if declDR && n.bdr == 0 && i.state == IfWaiting {
		backupSeen = true
	} else if declDR != wasDR {
		change = true
	}
	if declBDR && i.state == IfWaiting {
		backupSeen = true
	} else if declBDR != wasBDR {
		change = true
	}
	if backupSeen {
		i.electDR()
	} else if change {
		i.neighborChange()
	}
}

// neighborChange is the NeighborChange event.
func (i *iface) neighborChange() {
	if i.state == IfDR || i.state == IfBackup || i.state == IfDROther {
		i.electDR()
	}
}

type candidate struct {
	rid, self ID // router id; identity in DR fields
	prio      uint8
	dr, bdr   ID
}

// better: higher priority, then higher router id.
func (c candidate) better(o candidate) bool {
	if c.prio != o.prio {
		return c.prio > o.prio
	}
	return c.rid > o.rid
}

// electDR runs the DR election (RFC 2328 §9.4) and the AdjOK? event on
// the neighbours when the DR or BDR changed.
func (i *iface) electDR() {
	if i.cfg.P2P || i.state == IfDown || i.state == IfPassive {
		return
	}
	me := i.self()
	self := candidate{rid: i.r.rid, self: me, prio: i.cfg.Priority, dr: i.dr, bdr: i.bdr}
	var others []candidate
	for _, n := range i.sortedNbrs() {
		if n.state >= NbrTwoWay && n.prio > 0 {
			others = append(others, candidate{rid: n.id, self: n.self(), prio: n.prio, dr: n.dr, bdr: n.bdr})
		}
	}
	oldDR, oldBDR := i.dr, i.bdr
	elect := func(self candidate) (dr, bdr ID) {
		cs := slices.Clone(others)
		if self.prio > 0 {
			cs = append(cs, self)
		}
		// BDR: among those not declaring themselves DR, prefer those
		// declaring themselves BDR.
		var best *candidate
		bestDecl := false
		for k := range cs {
			c := &cs[k]
			if c.dr == c.self {
				continue
			}
			decl := c.bdr == c.self
			if best == nil || (decl && !bestDecl) || (decl == bestDecl && c.better(*best)) {
				best, bestDecl = c, decl
			}
		}
		if best != nil {
			bdr = best.self
		}
		// DR: among those declaring themselves DR; none: the BDR.
		best = nil
		for k := range cs {
			c := &cs[k]
			if c.dr != c.self {
				continue
			}
			if best == nil || c.better(*best) {
				best = c
			}
		}
		if best != nil {
			dr = best.self
		} else {
			dr = bdr
		}
		return dr, bdr
	}
	dr, bdr := elect(self)
	// Step 4: when this router's role changed, run steps 2 and 3 again.
	if (dr == me) != (oldDR == me) || (bdr == me) != (oldBDR == me) {
		self.dr, self.bdr = dr, bdr
		dr, bdr = elect(self)
	}
	if bdr == dr {
		bdr = 0
	}
	i.dr, i.bdr = dr, bdr
	switch {
	case dr == me:
		i.setState(IfDR)
	case bdr == me:
		i.setState(IfBackup)
	default:
		i.setState(IfDROther)
	}
	if dr != oldDR || bdr != oldBDR {
		i.r.Log.Debug("ospf DR election", "version", i.r.v, "interface", i.cfg.Name, "dr", dr, "bdr", bdr)
		for _, n := range i.sortedNbrs() {
			if n.state >= NbrTwoWay {
				n.adjOK()
			}
		}
		i.r.dirty = true
	}
}

// adjacent reports whether an adjacency should be formed with a neighbour
// (RFC 2328 §10.4).
func (i *iface) adjacent(n *neighbor) bool {
	if i.cfg.P2P {
		return true
	}
	me, other := i.self(), n.self()
	return i.dr == me || i.bdr == me || other == i.dr || other == i.bdr
}

// drNeighbor returns the DR's neighbour entry (nil: this router or none).
func (i *iface) drNeighbor() *neighbor {
	for _, n := range i.nbrs {
		if n.self() == i.dr {
			return n
		}
	}
	return nil
}

// floodDst is the destination of flooded updates and delayed acks.
func (i *iface) floodDst() netip.Addr {
	if i.cfg.P2P || i.state == IfDR || i.state == IfBackup {
		return i.r.v.AllSPF()
	}
	return i.r.v.AllDR()
}

// unicast is the destination of packets for one neighbour (p2p: the
// multicast group, which also works on unnumbered links).
func (i *iface) unicast(n *neighbor) netip.Addr {
	if i.cfg.P2P {
		return i.r.v.AllSPF()
	}
	return n.addr
}

// delayedAck queues an acknowledgment (sent within a second).
func (i *iface) delayedAck(h LSAHeader) {
	if len(i.acks) == 0 {
		i.ackAt = i.r.now.Add(time.Second)
	}
	i.acks = append(i.acks, h)
}

func (i *iface) flushAcks() {
	for len(i.acks) > 0 {
		n := min(len(i.acks), i.maxHeaders())
		i.r.send(i, i.floodDst(), &Packet{Type: TypeLSAck, Ack: i.acks[:n]})
		i.acks = i.acks[n:]
	}
	i.acks = nil
}

// mtu is the interface's IP MTU (1500 when unknown).
func (i *iface) mtu() int {
	if i.cfg.MTU == 0 {
		return 1500
	}
	return int(i.cfg.MTU)
}

// payload is the room for an OSPF packet (IP header and, for OSPFv2 MD5,
// the digest left out).
func (i *iface) payload() int {
	if i.r.v == V3 {
		return i.mtu() - 40
	}
	return i.mtu() - 20 - md5Len
}

// maxHeaders is how many LSA headers fit into a DD or ack packet.
func (i *iface) maxHeaders() int {
	return max(1, (i.payload()-i.r.v.headerLen()-ddLen(i.r.v))/lsaHeaderLen)
}

// fullNbrs returns the neighbours in state Full.
func (i *iface) fullNbrs() []*neighbor {
	var out []*neighbor
	for _, n := range i.sortedNbrs() {
		if n.state == NbrFull {
			out = append(out, n)
		}
	}
	return out
}
