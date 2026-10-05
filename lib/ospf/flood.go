package ospf

import "time"

// receiveLSU processes a link state update (RFC 2328 §13).
func (n *neighbor) receiveLSU(lsas []*LSA) {
	if n.state < NbrExchange {
		return
	}
	i, r := n.ifc, n.ifc.r
	var directAcks []LSAHeader
	pending := len(n.requests)
	for _, l := range lsas {
		s := r.scopeFor(i, l.Type) // step 2: unknown v2 types are discarded
		if s == nil {
			continue
		}
		ref := l.Ref()
		have := s.db.Get(ref, r.now)
		// Step 4: a MaxAge LSA nobody has: acknowledge and drop.
		if l.Age >= MaxAge && have == nil && !r.exchanging(s) {
			directAcks = append(directAcks, l.LSAHeader)
			continue
		}
		cmp := 1
		if have != nil {
			cmp = Compare(l.LSAHeader, have.LSAHeader)
		}
		switch {
		case cmp > 0: // step 5: newer
			if have != nil && r.now.Sub(s.db.arrived(ref)) < MinLSArrival*time.Second {
				continue // arrived too soon after the last one: no ack
			}
			r.strictCheck(s, l, have)
			back := r.flood(s, l, n, i)
			r.dropFromRetrans(s, ref)
			if r.v.isGrace(l) && l.AdvRtr != r.rid {
				defer r.graceReceived(i, l) // after it is installed
			}
			if s.db.Install(l, r.now) {
				r.scheduleSPF()
				if r.v == V3 && l.Type == V3Link {
					r.dirty = true // the DR's network prefix LSA lists the link's prefixes
				}
			}
			delete(s.maxAgeFlooded, ref)
			if !back {
				if i.state != IfBackup || n.self() == i.dr {
					i.delayedAck(l.LSAHeader)
				}
			}
			if l.AdvRtr == r.rid || r.ownNetwork(l) {
				r.receivedOwn(s, l)
			}
		case n.onRequests(ref):
			n.seqMismatch() // step 6: BadLSReq
			return
		case cmp == 0: // step 7: the same instance
			if _, ok := n.retrans[ref]; ok {
				delete(n.retrans, ref) // an implied acknowledgment
				if i.state == IfBackup && n.self() == i.dr {
					i.delayedAck(l.LSAHeader)
				}
			} else {
				directAcks = append(directAcks, l.LSAHeader)
			}
		default: // step 8: the database copy is newer
			if have.Age >= MaxAge && have.Seq == MaxSeq {
				continue
			}
			if t, ok := n.sentBack[ref]; !ok || r.now.Sub(t) >= MinLSArrival*time.Second {
				if n.sentBack == nil {
					n.sentBack = map[LSRef]time.Time{}
				}
				n.sentBack[ref] = r.now
				n.sendLSU([]*LSA{have}, i.unicast(n))
			}
		}
		if _, ok := n.requests[ref]; ok && cmp >= 0 {
			delete(n.requests, ref)
		}
	}
	if len(directAcks) > 0 {
		r.send(i, i.unicast(n), &Packet{Type: TypeLSAck, Ack: directAcks})
	}
	switch {
	case n.state == NbrLoading && len(n.requests) == 0:
		n.setState(NbrFull) // LoadingDone
	case n.state >= NbrExchange && len(n.requests) > 0 && len(n.requests) < pending:
		// Progress: ask for the next ones at once (otherwise the request
		// timer repeats them).
		n.sendLSR()
	}
}

func (n *neighbor) onRequests(ref LSRef) bool {
	_, ok := n.requests[ref]
	return ok
}

// exchanging: a neighbour of the scope is in Exchange or Loading.
func (r *Router) exchanging(s *scope) bool {
	for _, i := range r.floodIfaces(s) {
		for _, n := range i.nbrs {
			if n.state == NbrExchange || n.state == NbrLoading {
				return true
			}
		}
	}
	return false
}

// dropFromRetrans removes an older instance from every retransmission
// list (RFC 2328 §13 step 5c).
func (r *Router) dropFromRetrans(s *scope, ref LSRef) {
	for _, i := range r.floodIfaces(s) {
		for _, n := range i.nbrs {
			delete(n.retrans, ref)
		}
	}
}

// flood floods an LSA over its scope (RFC 2328 §13.3). from is the
// neighbour it came from (nil: originated here) and fromIf its interface.
// It reports whether the LSA was flooded back out the receiving interface
// (then no acknowledgment is needed).
func (r *Router) flood(s *scope, l *LSA, from *neighbor, fromIf *iface) (back bool) {
	for _, i := range r.floodIfaces(s) {
		if i.state == IfDown || i.state == IfPassive {
			continue
		}
		added := false
		for _, n := range i.sortedNbrs() {
			if n.state < NbrExchange {
				continue
			}
			if n.state < NbrFull {
				if req, ok := n.requests[l.Ref()]; ok {
					c := Compare(l.LSAHeader, req)
					if c < 0 {
						continue
					}
					delete(n.requests, l.Ref())
					if c == 0 {
						continue
					}
				}
			}
			if n == from {
				continue
			}
			if r.v == V2 && l.Type == V2OpaqueLink && n.options&OptO == 0 {
				continue // opaque LSAs only to neighbours that know them (RFC 5250)
			}
			n.addRetrans(l)
			added = true
		}
		if !added {
			continue
		}
		if i == fromIf && from != nil && (from.self() == i.dr || from.self() == i.bdr) {
			continue
		}
		if i == fromIf && i.state == IfBackup {
			continue
		}
		if i == fromIf {
			back = true
		}
		i.sendLSU([]*LSA{l}, i.floodDst())
	}
	return back
}

// ownNetwork: an OSPFv2 network LSA for one of this router's interface
// addresses (originated before a router id change, RFC 2328 §13.4).
func (r *Router) ownNetwork(l *LSA) bool {
	if r.v != V2 || l.Type != V2Network {
		return false
	}
	for _, i := range r.ifaces {
		if IDFrom(i.cfg.Addr) == l.ID {
			return true
		}
	}
	return false
}

// receivedOwn handles a newer copy of a self-originated LSA (RFC 2328
// §13.4): one this router still wants is originated again with a higher
// sequence number; one it does not want is flushed.
func (r *Router) receivedOwn(s *scope, l *LSA) {
	if l.Age >= MaxAge {
		return // our flush coming back
	}
	if r.restart != nil {
		return // restarting: our LSAs of before are kept as they are
	}
	if want := r.wanted(s, l.Ref()); want != nil && l.AdvRtr == r.rid {
		r.install(s, want, l.Seq)
		return
	}
	r.flush(s, l)
}

// flush removes an LSA from the routing domain: premature aging
// (RFC 2328 §14.1).
func (r *Router) flush(s *scope, l *LSA) {
	m := l.WithAge(MaxAge)
	s.db.Install(m, r.now)
	s.maxAgeFlooded[m.Ref()] = true
	r.flood(s, m, nil, nil)
	r.scheduleSPF()
}
