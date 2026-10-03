//go:build ospfwip

// Work in progress: excluded from the build until the neighbour state
// machine, flooding, origination and SPF exist.

package ospf

import (
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
	cfg   IfaceConfig
	state IfState
	// dr and bdr are interface addresses (OSPFv2 identifies them so).
	dr, bdr netip.Addr
	nbrs    map[netip.Addr]*neighbor // broadcast: by address; p2p: by router id
	helloAt time.Time
	waitAt  time.Time
	acks    []LSAHeader // delayed acknowledgments
	ackAt   time.Time
	since   time.Time
}

func (i *iface) addr() netip.Addr { return i.cfg.Prefix.Addr() }

func (i *iface) hello() time.Duration { return time.Duration(i.cfg.Hello) * time.Second }

func (i *iface) dead() time.Duration { return time.Duration(i.cfg.Dead) * time.Second }

func (i *iface) rxmt() time.Duration { return time.Duration(i.cfg.Retransmit) * time.Second }

// up is the InterfaceUp event.
func (i *iface) up() {
	i.since = i.r.now
	i.dr, i.bdr = netip.Addr{}, netip.Addr{}
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

// down is the InterfaceDown event: every neighbour is killed.
func (i *iface) down() {
	for _, n := range i.sortedNbrs() {
		n.kill()
	}
	i.acks = nil
	i.dr, i.bdr = netip.Addr{}, netip.Addr{}
	i.setState(IfDown)
}

func (i *iface) setState(s IfState) {
	if s == i.state {
		return
	}
	i.r.Log.Debug("ospf interface state", "interface", i.cfg.Name, "from", i.state, "to", s)
	i.state = s
	i.since = i.r.now
	i.r.dirty = true
}

func (i *iface) sortedNbrs() []*neighbor {
	out := make([]*neighbor, 0, len(i.nbrs))
	for _, n := range i.nbrs {
		out = append(out, n)
	}
	slices.SortFunc(out, func(a, b *neighbor) int { return a.id.Compare(b.id) })
	return out
}

// neighborFor finds the neighbour a non-hello packet came from.
func (i *iface) neighborFor(src, rid netip.Addr) *neighbor {
	if i.cfg.P2P {
		return i.nbrs[rid]
	}
	return i.nbrs[src]
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
	h := &Hello{Mask: Mask(i.cfg.Prefix.Bits()), Interval: i.cfg.Hello, Options: OptE, Priority: i.cfg.Priority,
		Dead: i.cfg.Dead, DR: zeroIfInvalid(i.dr), BDR: zeroIfInvalid(i.bdr)}
	if i.cfg.P2P {
		h.Priority = 0
	}
	for _, n := range i.sortedNbrs() {
		if n.state >= NbrInit {
			h.Neighbors = append(h.Neighbors, n.id)
		}
	}
	i.r.send(i, AllSPFRouters, &Packet{Type: TypeHello, Hello: h})
}

func zeroIfInvalid(a netip.Addr) netip.Addr {
	if !a.IsValid() {
		return netip.IPv4Unspecified()
	}
	return a
}

func validOrZero(a netip.Addr) netip.Addr {
	if a == netip.IPv4Unspecified() {
		return netip.Addr{}
	}
	return a
}

// receiveHello processes a hello (RFC 2328 §10.5).
func (i *iface) receiveHello(src netip.Addr, p *Packet) {
	h := p.Hello
	if h.Interval != i.cfg.Hello || h.Dead != i.cfg.Dead || h.Options&OptE == 0 {
		i.r.Stats.RxErrors++
		return
	}
	if !i.cfg.P2P && MaskBits(h.Mask) != i.cfg.Prefix.Bits() {
		i.r.Stats.RxErrors++
		return
	}
	key := src
	if i.cfg.P2P {
		key = p.RouterID
	}
	n := i.nbrs[key]
	if n == nil {
		n = &neighbor{ifc: i, id: p.RouterID, addr: src, state: NbrDown, retrans: map[LSRef]*LSA{}, requests: map[LSRef]LSAHeader{}}
		i.nbrs[key] = n
	}
	n.id, n.addr = p.RouterID, src
	oldPrio, oldDR, oldBDR := n.prio, n.dr, n.bdr
	n.prio, n.options = h.Priority, h.Options
	n.dr, n.bdr = validOrZero(h.DR), validOrZero(h.BDR)
	n.helloReceived()
	if !slices.Contains(h.Neighbors, i.r.rid) {
		n.oneWay()
		return
	}
	n.twoWay()
	if i.cfg.P2P {
		return
	}
	change := false
	if n.prio != oldPrio {
		change = true
	}
	declDR, wasDR := n.dr == src, oldDR == src
	declBDR, wasBDR := n.bdr == src, oldBDR == src
	backupSeen := false
	if declDR && !n.bdr.IsValid() && i.state == IfWaiting {
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
	id, addr netip.Addr
	prio     uint8
	dr, bdr  netip.Addr
}

// better: higher priority, then higher router id.
func (c candidate) better(o candidate) bool {
	if c.prio != o.prio {
		return c.prio > o.prio
	}
	return o.id.Compare(c.id) < 0
}

// electDR runs the DR election (RFC 2328 §9.4) and the AdjOK? event on
// the neighbours when the DR or BDR changed.
func (i *iface) electDR() {
	if i.cfg.P2P || i.state == IfDown || i.state == IfPassive {
		return
	}
	self := candidate{id: i.r.rid, addr: i.addr(), prio: i.cfg.Priority, dr: i.dr, bdr: i.bdr}
	var others []candidate
	for _, n := range i.sortedNbrs() {
		if n.state >= NbrTwoWay && n.prio > 0 {
			others = append(others, candidate{id: n.id, addr: n.addr, prio: n.prio, dr: n.dr, bdr: n.bdr})
		}
	}
	oldDR, oldBDR := i.dr, i.bdr
	elect := func(self candidate) (dr, bdr netip.Addr) {
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
			if c.dr == c.addr {
				continue
			}
			decl := c.bdr == c.addr
			if best == nil || (decl && !bestDecl) || (decl == bestDecl && c.better(*best)) {
				best, bestDecl = c, decl
			}
		}
		if best != nil {
			bdr = best.addr
		}
		// DR: among those declaring themselves DR; none: the BDR.
		best = nil
		for k := range cs {
			c := &cs[k]
			if c.dr != c.addr {
				continue
			}
			if best == nil || c.better(*best) {
				best = c
			}
		}
		if best != nil {
			dr = best.addr
		} else {
			dr = bdr
		}
		return dr, bdr
	}
	dr, bdr := elect(self)
	me := i.addr()
	// Step 4: when this router's role changed, run steps 2 and 3 again.
	if (dr == me) != (oldDR == me) || (bdr == me) != (oldBDR == me) {
		self.dr, self.bdr = dr, bdr
		dr, bdr = elect(self)
	}
	// A DR that is also BDR: the BDR is empty (two-router network).
	if bdr == dr {
		bdr = netip.Addr{}
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
		i.r.Log.Debug("ospf DR election", "interface", i.cfg.Name, "dr", dr, "bdr", bdr)
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
	me := i.addr()
	return i.dr == me || i.bdr == me || n.addr == i.dr || n.addr == i.bdr
}

// floodDst is the destination of flooded updates and delayed acks.
func (i *iface) floodDst() netip.Addr {
	if i.cfg.P2P || i.state == IfDR || i.state == IfBackup {
		return AllSPFRouters
	}
	return AllDRouters
}

// unicast is the destination of packets for one neighbour (p2p: the
// multicast group, which also works on unnumbered links).
func (i *iface) unicast(n *neighbor) netip.Addr {
	if i.cfg.P2P {
		return AllSPFRouters
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

// maxHeaders is how many LSA headers fit into a packet on the interface.
func (i *iface) maxHeaders() int {
	mtu := int(i.cfg.MTU)
	if mtu == 0 {
		mtu = 1500
	}
	return max(1, (mtu-20-headerLen-ddLen-md5Len)/lsaHeaderLen)
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
