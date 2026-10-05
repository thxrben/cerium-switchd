package bgpd

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/api/ribapi"
	"github.com/thxrben/cerium-switchd/pkg/bgp"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

// The routes reach cer-ribd and the members as changes by prefix (PLAN
// 15b): the speaker reports which prefixes were decided, the sender reads
// their current paths when it sends, in pieces of deltaChunk prefixes. A
// receiver that may have missed something (a lost message, its restart, a
// member that became reachable) gets a full sync: the table's prefixes in
// pieces between "begin" and "end". No copy of the table is kept or sent
// whole.

const (
	deltaChunk = 4096
	// keepalive: a receiver that heard nothing for this long gets an empty
	// delta (a restarted cer-ribd notices the gap and asks for a sync).
	keepalive = 30 * time.Second
	// retryAfter is how long a failed receiver waits before the next sync.
	retryAfter = time.Second
)

// target is one receiver of the routes: cer-ribd here (member 0) or a
// member.
type target struct {
	member int
	seq    uint64
	synced bool
	full   bool // the last sync ended converged (it swept)
	legacy bool // a member of an older release: whole tables (SetRoutes)
	failed time.Time
	sent   time.Time
	// sources the legacy member has (to withdraw a neighbour that is gone).
	sources map[string]bool
}

type sender struct {
	in *instance

	mu        sync.Mutex
	dirty     map[netip.Prefix]bool
	converged bool
	wake      chan struct{}

	// owned by run
	rib      *target
	members  map[int]*target
	lastConv bool
	resync   map[int]bool // members to sync again (Resend); 0: cer-ribd
}

func newSender(in *instance) *sender {
	return &sender{in: in, dirty: map[netip.Prefix]bool{}, wake: make(chan struct{}, 1), rib: &target{},
		members: map[int]*target{}, resync: map[int]bool{}}
}

// changed is the speaker's OnChanged (on its loop: must not block).
func (s *sender) changed(ps []netip.Prefix, converged bool) {
	s.mu.Lock()
	for _, p := range ps {
		s.dirty[p] = true
	}
	s.converged = converged
	s.mu.Unlock()
	s.poke()
}

func (s *sender) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// syncAgain makes a receiver sync (0: cer-ribd, -1: every member).
func (s *sender) syncAgain(member int) {
	s.mu.Lock()
	s.resync[member] = true
	s.mu.Unlock()
	s.poke()
}

func (s *sender) run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-t.C:
		}
		s.round(ctx)
	}
}

// round sends what changed to every receiver, or syncs those that need it.
func (s *sender) round(ctx context.Context) {
	s.mu.Lock()
	ps := make([]netip.Prefix, 0, len(s.dirty))
	for p := range s.dirty {
		ps = append(ps, p)
	}
	clear(s.dirty)
	conv := s.converged
	again := s.resync
	s.resync = map[int]bool{}
	s.mu.Unlock()
	d := s.in.d
	if again[0] {
		s.rib.synced = false
	}
	// Converged now: a sync that sweeps what is gone (the earlier ones,
	// before convergence, kept the routes of before a restart).
	convNow := conv && !s.lastConv
	s.lastConv = conv
	targets := []*target{s.rib}
	if d.Members != nil {
		want := map[int]bool{}
		for _, m := range d.Members() {
			want[m] = true
			t := s.members[m]
			if t == nil || again[-1] || again[m] {
				t = &target{member: m}
				s.members[m] = t
			}
			targets = append(targets, t)
		}
		for m := range s.members {
			if !want[m] {
				delete(s.members, m) // unreachable: a full sync when it is back
			}
		}
	}
	now := time.Now()
	var chunks [][]ribapi.PrefixRoutes // the changes, read once for every receiver
	read := false
	for _, t := range targets {
		if convNow || (t.synced && !t.full && conv) {
			t.synced = false
		}
		switch {
		case t.legacy:
			if !t.synced || len(ps) > 0 {
				s.legacySync(ctx, t, conv)
			}
		case !t.synced:
			if now.Sub(t.failed) >= retryAfter {
				s.sync(ctx, t, conv)
			}
		case len(ps) > 0:
			if !read {
				chunks, read = s.read(ctx, ps), true
			}
			for _, c := range chunks {
				if !s.send(ctx, t, ribapi.RoutesDelta{Prefixes: c, Full: conv}) {
					break
				}
			}
		case now.Sub(t.sent) >= keepalive:
			s.send(ctx, t, ribapi.RoutesDelta{Full: conv})
		}
	}
}

// read returns the current paths of ps, in pieces.
func (s *sender) read(ctx context.Context, ps []netip.Prefix) [][]ribapi.PrefixRoutes {
	var out [][]ribapi.PrefixRoutes
	for i := 0; i < len(ps); i += deltaChunk {
		pp, err := s.in.sp.Paths(ctx, ps[i:min(i+deltaChunk, len(ps))])
		if err != nil {
			return out
		}
		out = append(out, prefixRoutes(pp))
	}
	return out
}

func prefixRoutes(pp []bgp.PrefixPaths) []ribapi.PrefixRoutes {
	out := make([]ribapi.PrefixRoutes, 0, len(pp))
	for _, p := range pp {
		pr := ribapi.PrefixRoutes{Prefix: p.Prefix}
		for _, r := range p.Routes {
			rr := ribRoute(r)
			rr.Source = r.Peer.String()
			pr.Routes = append(pr.Routes, rr)
		}
		out = append(out, pr)
	}
	return out
}

// sync gives a receiver the whole table in pieces; it sweeps only when
// converged (before that, routes of before a restart stay).
func (s *sender) sync(ctx context.Context, t *target, conv bool) {
	keys, err := s.in.sp.Prefixes(ctx)
	if err != nil {
		return
	}
	if !s.send(ctx, t, ribapi.RoutesDelta{Sync: "begin", Full: conv}) {
		return
	}
	for i := 0; i < len(keys); i += deltaChunk {
		pp, err := s.in.sp.Paths(ctx, keys[i:min(i+deltaChunk, len(keys))])
		if err != nil {
			t.synced = false
			return
		}
		if !s.send(ctx, t, ribapi.RoutesDelta{Prefixes: prefixRoutes(pp), Full: conv}) {
			return
		}
	}
	if s.send(ctx, t, ribapi.RoutesDelta{Sync: "end", Full: conv}) {
		t.synced, t.full = true, conv
	}
}

// send gives one message to a receiver; false: it failed or asked for a
// sync (the receiver syncs at the next round).
func (s *sender) send(ctx context.Context, t *target, m ribapi.RoutesDelta) bool {
	d := s.in.d
	t.seq++
	m.Instance, m.Protocol, m.Seq = s.in.name, rib.BGP, t.seq
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var reply ribapi.DeltaReply
	var err error
	if t.member == 0 {
		reply, err = d.RIB.Delta(cctx, m)
	} else {
		reply, err = d.MemberDelta(cctx, t.member, m)
		if err != nil && strings.Contains(err.Error(), "unknown operation") {
			d.Log.Warn("bgp: member runs an older release: it gets whole tables until it is updated", "member", t.member)
			t.legacy = true
			return false
		}
	}
	t.sent = time.Now()
	if err != nil || reply.Resync {
		if err != nil {
			d.Log.Debug("bgp: routes not delivered; syncing again", "member", t.member, "err", err)
		}
		t.synced, t.failed = false, time.Now()
		return false
	}
	return true
}

// legacySync gives a member of an older release the whole table, per
// neighbour (the protocol of before; only during a rolling update).
func (s *sender) legacySync(ctx context.Context, t *target, conv bool) {
	d := s.in.d
	if d.MemberSetRoutes == nil {
		return
	}
	keys, err := s.in.sp.Prefixes(ctx)
	if err != nil {
		return
	}
	by := map[string][]rib.Route{}
	for i := 0; i < len(keys); i += deltaChunk {
		pp, err := s.in.sp.Paths(ctx, keys[i:min(i+deltaChunk, len(keys))])
		if err != nil {
			return
		}
		for _, p := range pp {
			for _, r := range p.Routes {
				by[r.Peer.String()] = append(by[r.Peer.String()], ribRoute(r))
			}
		}
	}
	if t.sources == nil {
		t.sources = map[string]bool{}
	}
	ok := true
	for src, rs := range by {
		if err := d.MemberSetRoutes(ctx, t.member, ribapi.SetRoutes{Instance: s.in.name, Protocol: rib.BGP, Source: src, Routes: rs, Full: conv}); err != nil {
			ok = false
		}
	}
	for src := range t.sources {
		if _, still := by[src]; !still {
			d.MemberSetRoutes(ctx, t.member, ribapi.SetRoutes{Instance: s.in.name, Protocol: rib.BGP, Source: src, Full: conv})
		}
	}
	t.sources = map[string]bool{}
	for src := range by {
		t.sources[src] = true
	}
	t.synced = ok
}

// withdraw removes the instance's routes everywhere (the instance stops):
// an empty sync that sweeps.
func (s *sender) withdraw() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	targets := []*target{s.rib}
	for _, t := range s.members {
		targets = append(targets, t)
	}
	for _, t := range targets {
		if t.legacy {
			for src := range t.sources {
				s.in.d.MemberSetRoutes(ctx, t.member, ribapi.SetRoutes{Instance: s.in.name, Protocol: rib.BGP, Source: src})
			}
			continue
		}
		if s.send(ctx, t, ribapi.RoutesDelta{Sync: "begin", Full: true}) {
			s.send(ctx, t, ribapi.RoutesDelta{Sync: "end", Full: true})
		}
	}
}
