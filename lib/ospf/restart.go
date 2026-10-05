package ospf

import (
	"time"
)

// The restarting role of graceful restart (RFC 3623 §2, RFC 5187): after
// a restart of the program this router announces a grace period, keeps the
// routes it had (it reports none meanwhile: the routing table keeps the old
// ones), originates none of its own topology LSAs and keeps the ones it
// receives from before, until its neighbours are fully adjacent again.

type restartState struct {
	until  time.Time
	reason uint8
	// nbrs are the neighbours before the restart, per interface.
	nbrs map[string][]ID
	// sent: grace LSAs sent on an interface (before its first hello).
	sent map[string]int
}

// Restarting reports whether the router is in a graceful restart.
func (r *Router) Restarting() bool { return r.restart != nil }

// StartRestart enters the restarting role (right after Configure): nbrs
// are the fully adjacent neighbours before the restart, by interface name;
// since is when the restart began (the grace period counts from then).
func (r *Router) StartRestart(nbrs map[string][]ID, since time.Time, reason uint8) {
	if !r.cfg.GracefulRestart || r.cfg.RestartDuration <= 0 {
		return
	}
	until := since.Add(r.cfg.RestartDuration)
	if !r.now.Before(until) || len(nbrs) == 0 {
		return
	}
	r.restart = &restartState{until: until, reason: reason, nbrs: nbrs, sent: map[string]int{}}
	r.Log.Info("ospf: graceful restart", "version", r.v, "until", until, "neighbors", len(nbrs))
}

// AdjacentNeighbors lists the fully adjacent neighbours per interface (what
// a restart needs to know, kept by the program across it).
func (r *Router) AdjacentNeighbors() map[string][]ID {
	out := map[string][]ID{}
	for _, i := range r.sortedIfaces() {
		for _, n := range i.sortedNbrs() {
			if n.state == NbrFull {
				out[i.cfg.Name] = append(out[i.cfg.Name], n.id)
			}
		}
	}
	return out
}

// PrepareRestart announces a planned restart (the program is about to
// stop): grace LSAs flooded on every interface with full neighbours. The
// program then stops without flushing anything; it should give the
// acknowledgements a moment (about a second).
func (r *Router) PrepareRestart(reason uint8) {
	if !r.cfg.GracefulRestart || r.cfg.RestartDuration <= 0 {
		return
	}
	for _, i := range r.sortedIfaces() {
		if len(i.fullNbrs()) == 0 {
			continue
		}
		l := r.graceLSA(i, GraceInfo{Period: r.cfg.RestartDuration, Reason: reason})
		r.installGrace(i, l)
		r.flood(i.sc, l, nil, nil)
	}
	r.planned = true
}

// installGrace stores an own grace LSA with a sequence number above an
// earlier one.
func (r *Router) installGrace(i *iface, l *LSA) {
	if have := i.sc.db.Get(l.Ref(), r.now); have != nil && have.Seq >= l.Seq {
		h := l.LSAHeader
		h.Seq = have.Seq + 1
		*l = *rawLSA(r.v, h, l.Raw[lsaHeaderLen:])
	}
	i.sc.db.Install(l, r.now)
}

// restartHello sends the grace LSA on an interface before its hellos
// (RFC 3623 §2.3: the neighbours must know before they see a hello that
// does not list them). It reports whether hellos may follow.
func (r *Router) restartHello(i *iface) {
	rs := r.restart
	if rs == nil || rs.sent[i.cfg.Name] >= 3 {
		return
	}
	rs.sent[i.cfg.Name]++
	left := rs.until.Sub(r.now).Round(time.Second)
	if left <= 0 {
		return
	}
	l := r.graceLSA(i, GraceInfo{Period: left, Reason: rs.reason})
	r.installGrace(i, l)
	i.sendLSU([]*LSA{l}, i.floodDst())
}

// checkRestart ends the restart when it is done, over, or inconsistent.
func (r *Router) checkRestart() {
	rs := r.restart
	if rs == nil {
		return
	}
	switch {
	case !r.now.Before(rs.until):
		r.endRestart("grace period over")
		return
	case r.inconsistent():
		r.endRestart("topology changed")
		return
	}
	for name, ids := range rs.nbrs {
		i := r.ifaces[name]
		if i == nil {
			continue // the interface is gone: nothing to wait for
		}
		for _, id := range ids {
			full := false
			for _, n := range i.nbrs {
				if n.id == id && n.state == NbrFull {
					full = true
				}
			}
			if !full {
				return
			}
		}
	}
	r.endRestart("completed")
}

// inconsistent: a fully adjacent neighbour of before announces a router LSA
// without a link to this router (the network moved on: RFC 3623 §2.2).
func (r *Router) inconsistent() bool {
	for name, ids := range r.restart.nbrs {
		i := r.ifaces[name]
		if i == nil || i.area == nil {
			continue
		}
		for _, n := range i.nbrs {
			if n.state != NbrFull || !containsID(ids, n.id) {
				continue
			}
			ref := LSRef{Type: V2Router, ID: n.id, AdvRtr: n.id}
			if r.v == V3 {
				ref = LSRef{Type: V3Router, ID: 0, AdvRtr: n.id}
			}
			l := i.area.sc.db.Get(ref, r.now)
			if l == nil {
				continue
			}
			lists := false
			for _, k := range l.Links {
				if k.ID == r.rid || (r.v == V3 && k.NbrRouter == r.rid) || (r.v == V2 && k.Type == LinkTransit) {
					lists = true
				}
			}
			if !lists {
				return true
			}
		}
	}
	return false
}

func containsID(ids []ID, id ID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// endRestart returns to normal operation: own LSAs originated, SPF, the
// routes reported, the grace LSAs flushed.
func (r *Router) endRestart(why string) {
	r.Log.Info("ospf: graceful restart ended", "version", r.v, "reason", why)
	r.restart = nil
	for _, i := range r.sortedIfaces() {
		for _, l := range i.sc.db.All(r.now) {
			if l.AdvRtr == r.rid && r.v.isGrace(l) && l.Age < MaxAge {
				r.flush(i.sc, l)
			}
		}
	}
	r.routes = nil // the next SPF reports its table (none was reported meanwhile)
	r.dirty = true
	r.scheduleSPF()
}
