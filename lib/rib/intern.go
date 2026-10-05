package rib

import (
	"hash/maphash"
	"strconv"
	"strings"
	"unique"
	"weak"
)

// Routes share what they have in common: next hop sets and attributes are
// stored once (a full BGP table has far fewer of them than routes; every
// OSPF route of an area has the same attributes). This keeps the bytes
// per route, the memory slots' cost (reference 5.1), low.

// maxHopSets bounds the next hop sets kept for sharing; beyond it the
// table starts afresh (sets in use stay shared by their routes).
const maxHopSets = 4096

func (r *RIB) internHops(hops []NextHop) []NextHop {
	if len(hops) == 0 {
		return nil
	}
	var b strings.Builder
	for _, h := range hops {
		b.WriteString(h.Gateway.String())
		b.WriteByte('|')
		b.WriteString(h.Interface)
		b.WriteByte(';')
	}
	k := b.String()
	if s, ok := r.hopSets[k]; ok {
		return s
	}
	if r.hopSets == nil || len(r.hopSets) >= maxHopSets {
		r.hopSets = map[string][]NextHop{}
	}
	for i := range hops {
		hops[i].Interface = unique.Make(hops[i].Interface).Value()
	}
	r.hopSets[k] = hops
	return hops
}

var attrSeed = maphash.MakeSeed()

// internAttrs returns the shared copy of a; the table holds them weakly
// (an attribute set no route uses any more is freed).
func (r *RIB) internAttrs(a *Attrs) *Attrs {
	if a == nil {
		return nil
	}
	// By a hash of the contents (a key string per set would cost as much
	// as sharing saves); a collision only means no sharing.
	k := a.key()
	h := maphash.String(attrSeed, k)
	if w, ok := r.attrSets[h]; ok {
		if p := w.Value(); p != nil && p.key() == k {
			return p
		}
	}
	if r.attrSets == nil {
		r.attrSets = map[uint64]weak.Pointer[Attrs]{}
	}
	// Drop the freed entries now and then (the map would grow with every
	// attribute set ever seen).
	if len(r.attrSets) >= 2*r.attrLive+1024 {
		for k, w := range r.attrSets {
			if w.Value() == nil {
				delete(r.attrSets, k)
			}
		}
		r.attrLive = len(r.attrSets)
	}
	r.attrSets[h] = weak.Make(a)
	return a
}

// key identifies an attribute set.
func (a *Attrs) key() string {
	var b strings.Builder
	w := func(s string) {
		b.WriteString(s)
		b.WriteByte(0)
	}
	w(a.Area)
	w(a.PathType)
	w(strconv.FormatUint(uint64(a.Tag), 10))
	w(a.Peer)
	w(strconv.FormatUint(uint64(a.PeerAS), 10))
	w(a.ASPath)
	if a.LocalPref != nil {
		w(strconv.FormatUint(uint64(*a.LocalPref), 10))
	} else {
		w("-")
	}
	w(strings.Join(a.Communities, " "))
	w(a.Originator)
	w(strings.Join(a.ClusterList, " "))
	w(a.InactiveReason)
	return b.String()
}
