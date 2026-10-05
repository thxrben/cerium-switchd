// Package ribd is the routing table of a member and the only installer of
// routes (reference 5.8, 1.9): switchd gives it the connected, static and
// DHCP routes of every routing instance, the routing protocols give it
// theirs; it selects the active routes by preference (with ECMP) and
// installs them, changing only what differs. cer-ribd runs it.
package ribd

import (
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/netdev"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

// Grace is how long routes of a routing protocol that has not reported
// since cer-ribd started are left alone in the kernel (its graceful
// restart time).
const Grace = 180 * time.Second

// configProtos are the sources switchd's configuration provides.
var configProtos = []rib.Protocol{rib.Direct, rib.Local, rib.Static, rib.DHCP}

// Installer installs routes (netdev.SyncRoutes; a fake in tests).
type Installer func(want []netdev.Route, protos []int) (changed bool, warnings []string, err error)

// Server is the routing table and its installation.
type Server struct {
	RIB     *rib.RIB
	Log     *slog.Logger
	Install Installer
	// Full is told when a memory slot purpose becomes full or has room
	// again (nil: nobody).
	Full func(purpose string, full bool)

	mu      sync.Mutex
	cfg     *Config
	ready   map[rib.Protocol]bool
	started time.Time
	kick    chan struct{}
	warned  string
	full    bool // OSPF routes were refused
	// seqs is the last delta per (instance, protocol); syncing those in a
	// sync between "begin" and "end".
	seqs    map[deltaKey]uint64
	syncing map[deltaKey]bool
}

type deltaKey struct {
	instance string
	proto    rib.Protocol
}

// New returns a server.
func New(install Installer, log *slog.Logger, now time.Time) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{RIB: rib.New(nil), Log: log, Install: install, ready: map[rib.Protocol]bool{}, started: now,
		kick: make(chan struct{}, 1)}
}

func (s *Server) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// SetConfig takes switchd's configuration.
func (s *Server) SetConfig(c *Config) {
	s.mu.Lock()
	old := s.cfg
	s.cfg = c
	s.mu.Unlock()
	s.RIB.SetLimit(rib.OSPF, c.OSPFLimit)
	if old != nil {
		for inst := range old.Instances {
			if _, ok := c.Instances[inst]; !ok {
				s.RIB.DropInstance(inst)
			}
		}
	}
	for inst, in := range c.Instances {
		by := map[rib.Protocol][]rib.Route{}
		for _, r := range in.Routes {
			by[r.Protocol] = append(by[r.Protocol], r)
		}
		for _, p := range configProtos {
			s.RIB.Set(inst, p, "", by[p])
		}
	}
	s.RIB.Changes() // settle the active routes the resolution reads
	s.revalidate()
	s.RIB.Changes() // (the installation reads the whole table)
	s.poke()
}

// SetRoutes takes a routing protocol's routes.
func (s *Server) SetRoutes(sr SetRoutes) {
	s.RIB.Set(sr.Instance, sr.Protocol, sr.Source, sr.Routes)
	if sr.Protocol == rib.OSPF {
		s.checkFull()
	}
	s.RIB.Changes() // settle the active routes the resolution reads
	s.revalidate()
	s.RIB.Changes()
	if sr.Full {
		s.mu.Lock()
		s.ready[sr.Protocol] = true
		s.mu.Unlock()
	}
	s.poke()
}

// Delta takes a protocol's changes by prefix (RoutesDelta).
func (s *Server) Delta(d RoutesDelta) DeltaReply {
	k := deltaKey{d.Instance, d.Protocol}
	s.mu.Lock()
	if s.seqs == nil {
		s.seqs, s.syncing = map[deltaKey]uint64{}, map[deltaKey]bool{}
	}
	switch {
	case d.Sync == "begin":
		s.syncing[k] = true
	case d.Seq != s.seqs[k]+1:
		s.mu.Unlock()
		s.Log.Warn("routes: a delta was lost; asking for a sync", "instance", d.Instance, "protocol", d.Protocol, "seq", d.Seq, "expected", s.seqs[k]+1)
		return DeltaReply{Resync: true}
	}
	s.seqs[k] = d.Seq
	s.mu.Unlock()
	if d.Sync == "begin" {
		s.RIB.BeginGen(d.Instance, d.Protocol)
	}
	for _, p := range d.Prefixes {
		s.RIB.Replace(d.Instance, d.Protocol, p.Prefix, p.Routes)
	}
	if d.Sync == "end" {
		// Only a converged protocol's sync removes what it did not set:
		// before, the routes of before its restart stay (no drop while it
		// learns the table again).
		if d.Full {
			s.RIB.SweepGen(d.Instance, d.Protocol)
		}
		s.mu.Lock()
		delete(s.syncing, k)
		s.mu.Unlock()
	}
	s.RIB.Changes() // settle the active routes the resolution reads
	if d.Protocol == rib.BGP && d.Sync != "end" {
		// Only BGP routes changed: only theirs need resolving again.
		s.revalidateAt(d.Instance, d.Prefixes)
	} else {
		s.revalidate()
	}
	s.RIB.Changes()
	if d.Full {
		s.mu.Lock()
		s.ready[d.Protocol] = true
		s.mu.Unlock()
	}
	s.poke()
	return DeltaReply{}
}

// kernelProto is the kernel protocol id of a source's routes (0: not
// installed: the kernel has connected and local routes itself).
func kernelProto(p rib.Protocol) int {
	switch p {
	case rib.Static, rib.DHCP:
		return netdev.ProtoStatic
	case rib.OSPF, rib.OSPF3:
		return netdev.ProtoOSPF
	case rib.BGP:
		return netdev.ProtoBGP
	}
	return 0
}

// managed returns the kernel ids whose routes are complete now: those of
// the configuration once it arrived, a routing protocol's once it
// reported everything or its grace time passed.
func (s *Server) managed(now time.Time) map[int]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int]bool{}
	if s.cfg == nil {
		return out
	}
	out[netdev.ProtoStatic] = true
	grace := now.Sub(s.started) >= Grace
	ospfGrace := now.Sub(s.started) >= max(Grace, time.Duration(s.cfg.OSPFGrace)*time.Second)
	if ospfGrace || (s.ready[rib.OSPF] && s.ready[rib.OSPF3]) {
		out[netdev.ProtoOSPF] = true
	}
	if grace || s.ready[rib.BGP] {
		out[netdev.ProtoBGP] = true
	}
	return out
}

// FIB returns the active routes to install, for the kernel ids in protos.
func (s *Server) FIB(protos map[int]bool) []netdev.Route {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if cfg == nil {
		return nil
	}
	var out []netdev.Route
	resolved := map[rib.Table]map[netip.Addr][]rib.NextHop{}
	for _, e := range s.RIB.Lookup(rib.Query{Active: true}) {
		if e.Active < 0 {
			continue
		}
		r := e.Routes[e.Active]
		kp := kernelProto(r.Protocol)
		if kp == 0 || !protos[kp] {
			continue
		}
		in, ok := cfg.Instances[e.Table.Instance]
		if !ok {
			continue
		}
		nr := netdev.Route{VRF: in.VRF, Prefix: e.Prefix, Discard: r.Discard, Proto: kp}
		if !r.Discard {
			hops := r.NextHops
			if r.Protocol == rib.BGP {
				hops = s.resolveAll(e.Table, hops, resolved)
			}
			for _, h := range hops {
				dev := ""
				if h.Interface != "" {
					d, ok := in.Devices[h.Interface]
					if !ok {
						continue // a unit of another member
					}
					dev = d
				}
				nr.NextHops = append(nr.NextHops, h.Gateway)
				nr.Devs = append(nr.Devs, dev)
			}
			if len(nr.NextHops) == 0 {
				continue
			}
		}
		out = append(out, nr)
	}
	return out
}

// revalidate hides the BGP routes whose next hops cannot be resolved (and
// shows them again once they can): a hidden route is never active, so a
// route of another protocol (or another BGP path) is used instead, as in
// Junos. Resolution never goes through BGP, so one pass suffices.
func (s *Server) revalidate() {
	type ref struct {
		t      rib.Table
		prefix netip.Prefix
		source string
		hops   []rib.NextHop
		hidden bool
	}
	var rs []ref
	s.RIB.Each(rib.BGP, func(t rib.Table, rt rib.Route) {
		rs = append(rs, ref{t, rt.Prefix, rt.Source, rt.NextHops, rt.Hidden})
	})
	cache := map[rib.Table]map[netip.Addr][]rib.NextHop{}
	for _, r := range rs {
		hidden := len(r.hops) > 0 && len(s.resolveAll(r.t, r.hops, cache)) == 0
		if hidden != r.hidden {
			s.RIB.SetHidden(r.t, r.prefix, rib.BGP, r.source, hidden)
		}
	}
}

// revalidateAt is revalidate for the BGP routes at some prefixes.
func (s *Server) revalidateAt(instance string, ps []PrefixRoutes) {
	cache := map[rib.Table]map[netip.Addr][]rib.NextHop{}
	for _, p := range ps {
		type ref struct {
			t      rib.Table
			source string
			hops   []rib.NextHop
			hidden bool
		}
		var rs []ref
		s.RIB.EachAt(instance, rib.BGP, p.Prefix, func(t rib.Table, rt rib.Route) {
			rs = append(rs, ref{t, rt.Source, rt.NextHops, rt.Hidden})
		})
		for _, r := range rs {
			hidden := len(r.hops) > 0 && len(s.resolveAll(r.t, r.hops, cache)) == 0
			if hidden != r.hidden {
				s.RIB.SetHidden(r.t, p.Prefix.Masked(), rib.BGP, r.source, hidden)
			}
		}
	}
}

// resolveAll resolves BGP next hops (cache: per table and next hop).
func (s *Server) resolveAll(t rib.Table, hops []rib.NextHop, cache map[rib.Table]map[netip.Addr][]rib.NextHop) []rib.NextHop {
	if cache[t] == nil {
		cache[t] = map[netip.Addr][]rib.NextHop{}
	}
	var out []rib.NextHop
	for _, h := range hops {
		if h.Interface != "" || !h.Gateway.IsValid() {
			out = append(out, h)
			continue
		}
		r, ok := cache[t][h.Gateway]
		if !ok {
			r = s.resolve(t, h.Gateway)
			cache[t][h.Gateway] = r
		}
		out = append(out, r...)
	}
	return out
}

// resolve finds how a BGP next hop is reached: through the longest
// matching active route of another protocol (directly connected: the
// next hop itself on that interface; else that route's next hops). A
// next hop reached only through BGP, or the switch's own address, is
// unresolvable (the route is not installed).
func (s *Server) resolve(t rib.Table, gw netip.Addr) []rib.NextHop {
	es := s.RIB.Lookup(rib.Query{Tables: []rib.Table{t}, Prefix: netip.PrefixFrom(gw, gw.BitLen()), Active: true})
	if len(es) == 0 || es[0].Active < 0 {
		return nil
	}
	r := es[0].Routes[es[0].Active]
	switch {
	case r.Protocol == rib.BGP || r.Protocol == rib.Local || r.Discard:
		return nil
	case r.Protocol == rib.Direct:
		var out []rib.NextHop
		for _, h := range r.NextHops {
			out = append(out, rib.NextHop{Gateway: gw, Interface: h.Interface})
		}
		return out
	}
	return r.NextHops
}

// Sync installs the active routes once.
func (s *Server) Sync(now time.Time) (warnings []string) {
	protos := s.managed(now)
	if len(protos) == 0 {
		return nil // no configuration yet: the kernel keeps what it has
	}
	ids := make([]int, 0, len(protos))
	for p := range protos {
		ids = append(ids, p)
	}
	slices.Sort(ids)
	changed, warnings, err := s.Install(s.FIB(protos), ids)
	if err != nil {
		s.Log.Error("routes", "err", err)
	}
	if changed {
		s.Log.Debug("routes installed")
	}
	if w := strings.Join(warnings, "; "); w != s.warned {
		s.warned = w
		if w != "" {
			s.Log.Warn("routes not installed yet", "reasons", w)
		}
	}
	return warnings
}

// Run installs after every change, every 30 s (which also undoes foreign
// changes) and every 2 s while routes wait for their interfaces.
func (s *Server) Run(ctx context.Context) {
	retry := 30 * time.Second
	t := time.NewTimer(retry)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
		case <-t.C:
		}
		retry = 30 * time.Second
		if len(s.Sync(time.Now())) > 0 {
			retry = 2 * time.Second
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(retry)
	}
}

// Lookup is show route.
func (s *Server) Lookup(q rib.Query) []rib.Entry { return s.RIB.Lookup(q) }

// checkFull reports OSPF routes refused by the memory slots (full), and
// when every route fits again.
func (s *Server) checkFull() {
	_, refused := s.RIB.Count(rib.OSPF)
	s.mu.Lock()
	changed := (refused > 0) != s.full
	s.full = refused > 0
	s.mu.Unlock()
	if !changed {
		return
	}
	if refused > 0 {
		s.Log.Error("memory slots of ospf are full: routes are not installed", "refused", refused)
	}
	if s.Full != nil {
		s.Full("ospf", refused > 0)
	}
}
