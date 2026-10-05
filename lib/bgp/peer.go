package bgp

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// PeerState is a neighbour's state as shown (RFC 4271 §8.2.2).
type PeerState uint8

const (
	Idle PeerState = iota
	Connect
	Active
	OpenSent
	OpenConfirm
	Established
)

func (s PeerState) String() string {
	return [...]string{"Idle", "Connect", "Active", "OpenSent", "OpenConfirm", "Established"}[s]
}

// inPath is a received path: as received, and after import policy (nil:
// rejected, "hidden").
type inPath struct {
	raw      *Path
	accepted *Path
}

// outPath is what was announced for a prefix.
type outPath struct {
	path *Path
	key  string
}

// Stats are a neighbour's counters.
type Stats struct {
	MsgsIn, MsgsOut       uint64
	UpdatesIn, UpdatesOut uint64
	Flaps                 uint64
}

type peer struct {
	sp *Speaker
	n  Neighbor

	sessions []*session // at most one per direction until one is established
	est      *session   // the established one
	open     peerOpen   // the neighbour's capabilities (est)
	families []Family   // negotiated
	hold     time.Duration
	keepAt   time.Time

	dialing bool
	retryAt time.Time
	backoff time.Duration

	in  map[netip.Prefix]*inPath
	out map[netip.Prefix]*outPath

	eorIn map[Family]bool
	// Graceful restart (helper): the neighbour's paths are stale until
	// it is back and sends End-of-RIB, or until staleUntil.
	stale      bool
	staleUntil time.Time
	replacing  bool // the stale paths are a replaced session's

	since     time.Time // state change
	lastError string
	stats     Stats

	// BFD (RFC 5882): bfdWasUp once the session was up; bfdHold after it
	// went down: no session until it is up again.
	bfdWasUp, bfdHold bool
}

func newPeer(sp *Speaker, n Neighbor) *peer {
	return &peer{sp: sp, n: n, in: map[netip.Prefix]*inPath{}, out: map[netip.Prefix]*outPath{}, eorIn: map[Family]bool{},
		since: time.Now()}
}

func (p *peer) established() bool { return p.est != nil && p.est.state == sEstablished }

// usable: the neighbour's paths count (established, or stale in a
// graceful restart).
func (p *peer) usable() bool { return p.established() || p.stale }

func (p *peer) allEOR() bool {
	for _, f := range p.families {
		if !p.eorIn[f] {
			return false
		}
	}
	return true
}

// state is the neighbour's state for show.
func (p *peer) state() PeerState {
	if p.est != nil {
		switch p.est.state {
		case sEstablished:
			return Established
		case sOpenConfirm:
			return OpenConfirm
		}
	}
	best := Idle
	for _, s := range p.sessions {
		st := OpenSent
		if s.state == sOpenConfirm {
			st = OpenConfirm
		}
		best = max(best, st)
	}
	switch {
	case best != Idle:
		return best
	case p.n.Disabled:
		return Idle
	case p.dialing:
		return Connect
	case p.n.Passive || !p.retryAt.IsZero():
		return Active
	}
	return Idle
}

func (p *peer) ebgp() bool { return !p.n.Internal }

func (p *peer) retryDelay() time.Duration {
	if p.sp.cfg.ConnectRetry > 0 {
		return p.sp.cfg.ConnectRetry
	}
	return 30 * time.Second
}

// tick runs the timers.
func (p *peer) tick(now time.Time) {
	if p.stale && now.After(p.staleUntil) {
		p.endStale()
	}
	for _, s := range slices.Clone(p.sessions) {
		if !s.holdAt.IsZero() && now.After(s.holdAt) {
			p.fail(s, bgp.NewMessageError(bgp.BGP_ERROR_HOLD_TIMER_EXPIRED, 0, nil, "hold timer expired").(*bgp.MessageError))
		}
	}
	if p.established() && p.hold > 0 && now.After(p.keepAt) {
		p.est.send(bgp.NewBGPKeepAliveMessage())
		p.keepAt = now.Add(p.hold / 3)
	}
	if p.n.Disabled || p.n.Passive || p.bfdHold || p.dialing || len(p.sessions) > 0 || now.Before(p.retryAt) {
		return
	}
	p.dial()
}

func (p *peer) dial() {
	p.dialing = true
	n, sp := p.n, p.sp
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := sp.Transport.Dial(ctx, n)
		cancel()
		sp.do(func() {
			p.dialing = false
			if sp.peers[n.Addr] != p || p.n.Disabled {
				if c != nil {
					c.Close()
				}
				return
			}
			if err != nil {
				p.lastError = "connect: " + err.Error()
				p.retryAt = time.Now().Add(p.retryDelay())
				return
			}
			p.attach(c, false)
		})
	}()
}

// attach starts a session on a new connection.
func (p *peer) attach(c net.Conn, incoming bool) {
	if len(p.sessions) >= 2 {
		s := newSession(p, c, incoming)
		s.start(p.sp)
		s.notify(bgp.BGP_ERROR_CEASE, bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil)
		return
	}
	s := newSession(p, c, incoming)
	p.sessions = append(p.sessions, s)
	s.start(p.sp)
	hold := p.n.HoldTime
	s.send(openMsg(openParams{AS: p.n.LocalAS, HoldTime: uint16(hold), RouterID: p.sp.cfg.RouterID, Families: p.n.Families,
		GR: p.n.GracefulRestart, GRTime: uint16(p.n.RestartTime),
		Restarting: p.sp.cfg.Restarting && time.Since(p.sp.started) < time.Duration(max(p.n.RestartTime, 1))*time.Second}))
	s.state = sOpenSent
	s.holdAt = time.Now().Add(4 * time.Minute) // RFC 4271: a large hold time until the OPEN
	p.since = time.Now()
}

// message handles a received message.
func (p *peer) message(s *session, m *bgp.BGPMessage, err error) {
	if !slices.Contains(p.sessions, s) {
		return
	}
	p.stats.MsgsIn++
	if m == nil {
		p.fail(s, asMessageError(err))
		return
	}
	switch b := m.Body.(type) {
	case *bgp.BGPOpen:
		if err != nil || s.state != sOpenSent {
			p.fail(s, fsmOrErr(err))
			return
		}
		p.onOpen(s, b)
	case *bgp.BGPKeepAlive:
		switch s.state {
		case sOpenConfirm:
			p.up(s)
		case sEstablished:
		default:
			p.fail(s, fsmOrErr(nil))
			return
		}
		p.refreshHold(s)
	case *bgp.BGPUpdate:
		if s.state != sEstablished {
			p.fail(s, fsmOrErr(nil))
			return
		}
		p.stats.UpdatesIn++
		p.refreshHold(s)
		withdraw := false
		if err != nil {
			w, discard, notify := errorHandling(err)
			if notify != nil {
				p.fail(s, notify)
				return
			}
			withdraw = w
			_ = discard
		}
		p.update(b, withdraw)
	case *bgp.BGPNotification:
		if b.ErrorCode == bgp.BGP_ERROR_CEASE && b.ErrorSubcode == bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION &&
			s == p.est && s.state == sEstablished {
			// The neighbour keeps our other connection (it may not have
			// reached us yet): a replacement, not a failure.
			p.drop(s)
			s.close()
			p.replaced()
			if len(p.sessions) == 0 {
				p.retryAt = time.Time{}
			}
			return
		}
		p.lastError = fmt.Sprintf("received notification %d/%d", b.ErrorCode, b.ErrorSubcode)
		p.closed(s, nil)
	case *bgp.BGPRouteRefresh:
		if s.state == sEstablished {
			if f, ok := familyOfRF(bgp.AfiSafiToRouteFamily(b.AFI, b.SAFI)); ok {
				p.resend(f)
			}
		}
	}
}

func asMessageError(err error) *bgp.MessageError {
	if me, ok := err.(*bgp.MessageError); ok {
		return me
	}
	return bgp.NewMessageError(bgp.BGP_ERROR_MESSAGE_HEADER_ERROR, 0, nil, fmt.Sprint(err)).(*bgp.MessageError)
}

func fsmOrErr(err error) *bgp.MessageError {
	if err != nil {
		return asMessageError(err)
	}
	return bgp.NewMessageError(bgp.BGP_ERROR_FSM_ERROR, 0, nil, "unexpected message").(*bgp.MessageError)
}

func (p *peer) refreshHold(s *session) {
	if p.hold > 0 {
		s.holdAt = time.Now().Add(p.hold)
	} else {
		s.holdAt = time.Time{}
	}
}

func (p *peer) onOpen(s *session, o *bgp.BGPOpen) {
	po := parseOpen(o)
	switch {
	case o.Version != 4:
		p.fail(s, bgp.NewMessageError(bgp.BGP_ERROR_OPEN_MESSAGE_ERROR, bgp.BGP_ERROR_SUB_UNSUPPORTED_VERSION_NUMBER, []byte{0, 4}, "version").(*bgp.MessageError))
		return
	case po.AS != p.n.PeerAS:
		p.lastError = fmt.Sprintf("the neighbour says AS %d, configured is %d", po.AS, p.n.PeerAS)
		p.fail(s, bgp.NewMessageError(bgp.BGP_ERROR_OPEN_MESSAGE_ERROR, bgp.BGP_ERROR_SUB_BAD_PEER_AS, nil, "bad peer AS").(*bgp.MessageError))
		return
	case !po.RouterID.Is4() || po.RouterID.IsUnspecified() || (p.n.Internal && po.RouterID == p.sp.cfg.RouterID):
		p.fail(s, bgp.NewMessageError(bgp.BGP_ERROR_OPEN_MESSAGE_ERROR, bgp.BGP_ERROR_SUB_BAD_BGP_IDENTIFIER, nil, "bad identifier").(*bgp.MessageError))
		return
	case po.HoldTime == 1 || po.HoldTime == 2:
		p.fail(s, bgp.NewMessageError(bgp.BGP_ERROR_OPEN_MESSAGE_ERROR, bgp.BGP_ERROR_SUB_UNACCEPTABLE_HOLD_TIME, nil, "hold time").(*bgp.MessageError))
		return
	}
	s.open = &po
	// Collision (RFC 4271 §6.8): of two connections that both got an
	// OPEN, the one started by the higher router id stays. This holds for
	// an established one too: the neighbour may have decided against it
	// before it saw our KEEPALIVE, and both sides must keep the same one.
	for _, o := range slices.Clone(p.sessions) {
		if o == s || o.open == nil {
			continue
		}
		keepIncoming := po.RouterID.Compare(p.sp.cfg.RouterID) > 0
		loser := o
		if s.incoming != keepIncoming {
			loser = s
		}
		wasEst := loser == p.est && loser.state == sEstablished
		p.drop(loser)
		loser.notify(bgp.BGP_ERROR_CEASE, bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil)
		if loser == s {
			return
		}
		if wasEst {
			p.replaced()
		}
	}
	hold := min(int(po.HoldTime), p.n.HoldTime)
	p.hold = time.Duration(hold) * time.Second
	s.state = sOpenConfirm
	s.send(bgp.NewBGPKeepAliveMessage())
	p.keepAt = time.Now().Add(p.hold / 3)
	p.refreshHold(s)
	if hold == 0 {
		s.holdAt = time.Time{}
	}
	p.est = s
}

// up: the session is established.
func (p *peer) up(s *session) {
	s.state = sEstablished
	p.open = *s.open
	p.families = nil
	for _, f := range p.n.Families {
		if slices.Contains(p.open.Families, f) {
			p.families = append(p.families, f)
		}
	}
	for _, o := range slices.Clone(p.sessions) {
		if o != s {
			p.drop(o)
			o.notify(bgp.BGP_ERROR_CEASE, bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil)
		}
	}
	p.since, p.lastError, p.backoff = time.Now(), "", 0
	p.eorIn = map[Family]bool{}
	p.out = map[netip.Prefix]*outPath{}
	if p.stale && !p.open.GR && !p.replacing {
		p.endStale() // it restarted without graceful restart: its old paths go
	}
	p.replacing = false
	p.sp.Log.Info("bgp neighbor up", "neighbor", p.n.Addr, "as", p.n.PeerAS)
	// The whole table, then End-of-RIB per family.
	var all []netip.Prefix
	for pf := range p.sp.best {
		all = append(all, pf)
	}
	for pf := range p.sp.local {
		all = append(all, pf)
	}
	p.advertise(all)
	for _, f := range p.families {
		s.send(eorMsg(f))
	}
}

// drop forgets a session (without notification).
func (p *peer) drop(s *session) {
	p.sessions = slices.DeleteFunc(p.sessions, func(o *session) bool { return o == s })
	if p.est == s {
		p.est = nil
	}
}

// fail sends a NOTIFICATION for an error and ends the session.
func (p *peer) fail(s *session, me *bgp.MessageError) {
	if !slices.Contains(p.sessions, s) {
		return
	}
	if p.lastError == "" || s == p.est {
		p.lastError = fmt.Sprintf("sent notification %d/%d: %s", me.TypeCode, me.SubTypeCode, me.Message)
	}
	s.notify(me.TypeCode, me.SubTypeCode, me.Data)
	p.closed(s, nil)
}

// closed: a connection ended.
func (p *peer) closed(s *session, err error) {
	if !slices.Contains(p.sessions, s) {
		return
	}
	wasUp := p.established() && p.est == s
	p.drop(s)
	s.close()
	if err != nil && p.lastError == "" {
		p.lastError = err.Error()
	}
	if wasUp {
		p.down(true)
	}
	if len(p.sessions) == 0 {
		// Retry soon after a session was up, then every retry interval.
		p.backoff = min(max(p.backoff*2, time.Second), p.retryDelay())
		p.retryAt = time.Now().Add(p.backoff)
		p.since = time.Now()
	}
}

// down: the established session ended. graceful: the neighbour may
// restart gracefully (its paths are kept stale).
func (p *peer) down(graceful bool) {
	p.stats.Flaps++
	p.sp.Log.Info("bgp neighbor down", "neighbor", p.n.Addr, "reason", p.lastError)
	p.out = map[netip.Prefix]*outPath{}
	if graceful && p.n.GracefulRestart && p.open.GR && len(p.in) > 0 {
		p.stale = true
		restart := time.Duration(max(int(p.open.GRTime), 1)) * time.Second
		p.staleUntil = time.Now().Add(restart)
		for pf, ip := range p.in {
			ip.raw.Stale = true
			if ip.accepted != nil {
				ip.accepted.Stale = true
			}
			p.sp.dirty[pf] = true
		}
		return
	}
	p.dropPaths()
}

// replaced: the established session lost a collision to a new one. Its
// paths stay (stale) until the new session's End-of-RIB: no flap.
func (p *peer) replaced() {
	p.out = map[netip.Prefix]*outPath{}
	p.replacing = true
	if len(p.in) == 0 {
		return
	}
	p.stale = true
	p.staleUntil = time.Now().Add(time.Duration(max(p.n.StaleTime, 30)) * time.Second)
	for pf, ip := range p.in {
		ip.raw.Stale = true
		if ip.accepted != nil {
			ip.accepted.Stale = true
		}
		p.sp.dirty[pf] = true
	}
}

// stop ends every session with a Cease (subcode sub).
func (p *peer) stop(code, sub uint8) {
	wasUp := p.established()
	for _, s := range slices.Clone(p.sessions) {
		s.notify(code, sub, nil)
		p.drop(s)
	}
	if wasUp {
		p.lastError = fmt.Sprintf("sent notification %d/%d", code, sub)
		p.down(false)
	}
	p.retryAt = time.Now().Add(time.Second)
}

func (p *peer) dropPaths() {
	for pf := range p.in {
		p.delIn(pf)
	}
	p.stale = false
}

// delIn removes a received path.
func (p *peer) delIn(pf netip.Prefix) {
	if _, ok := p.in[pf]; !ok {
		return
	}
	delete(p.in, pf)
	p.sp.dirty[pf] = true
	p.sp.received(pf, -1)
}

// addIn stores a received path; a new prefix or path beyond the memory
// slots' capacity is not stored (as if withdrawn; reference 5.1).
func (p *peer) addIn(pf netip.Prefix, ip *inPath) {
	if _, ok := p.in[pf]; !ok {
		if !p.sp.room(pf) {
			return
		}
		p.sp.received(pf, 1)
	}
	p.in[pf] = ip
	p.sp.dirty[pf] = true
}

// endStale removes the paths still stale after a graceful restart.
func (p *peer) endStale() {
	p.stale = false
	for pf, ip := range p.in {
		if ip.raw.Stale {
			p.delIn(pf)
		}
	}
}

// update processes a received UPDATE.
func (p *peer) update(u *bgp.BGPUpdate, withdrawAll bool) {
	d := decodeUpdate(u, withdrawAll)
	for _, f := range d.EOR {
		p.eorIn[f] = true
		if p.stale {
			// The restarted neighbour sent everything: what is still
			// stale of this family is gone.
			for pf, ip := range p.in {
				if ip.raw.Stale && FamilyOf(pf) == f {
					p.delIn(pf)
				}
			}
			if p.allEOR() {
				p.endStale()
			}
		}
	}
	for _, pf := range d.Withdrawn {
		p.delIn(pf)
	}
	if len(d.Reach) == 0 {
		return
	}
	if !p.open.AS4 && len(d.as4Path) > 0 {
		d.Attrs.ASPath = mergeAS4(d.Attrs.ASPath, d.as4Path)
	}
	now := time.Now()
	for _, r := range d.Reach {
		if !slices.Contains(p.families, FamilyOf(r.Prefix)) {
			continue
		}
		// The prefixes of one UPDATE share its attributes (they are never
		// changed in place: importPath copies before a policy changes them).
		a := d.Attrs
		a.NextHop, a.LinkLocal = r.NextHop, r.LinkLocal
		raw := &Path{Prefix: r.Prefix, Attrs: a, Peer: p.n.Addr, PeerAS: p.n.PeerAS, PeerID: p.open.RouterID.As4(), EBGP: p.ebgp(), Since: now}
		if old := p.in[r.Prefix]; old != nil && attrKey(&old.raw.Attrs) == attrKey(&raw.Attrs) {
			old.raw.Stale = false
			if old.accepted != nil {
				old.accepted.Stale = false
			}
			continue
		}
		p.addIn(r.Prefix, &inPath{raw: raw, accepted: p.importPath(raw)})
	}
}

// importPath applies the loop checks and the import policy (nil:
// rejected).
func (p *peer) importPath(raw *Path) *Path {
	sp := p.sp
	if p.ebgp() {
		if slices.Contains(raw.ASNs(), p.n.LocalAS) || slices.Contains(raw.ASNs(), sp.cfg.AS) {
			return nil // our own AS: a loop
		}
	} else {
		if raw.OriginatorID == sp.cfg.RouterID {
			return nil
		}
		for _, c := range raw.ClusterList {
			if sp.clusterIDs[c] {
				return nil
			}
		}
	}
	a := *raw
	a.Attrs = raw.Attrs.Clone()
	if p.ebgp() {
		a.LocalPref = nil // not accepted from another AS
		if p.n.LocalAS != sp.cfg.AS && p.n.LocalAS != 0 {
			a.Prepend(p.n.LocalAS) // local-as: other neighbours see it in the path
		}
	}
	if sp.pol.Import != nil && !sp.pol.Import(&p.n, &a) {
		return nil
	}
	// Unchanged: the accepted path is the received one (one copy per path
	// instead of two; the memory slots' cost, reference 5.1).
	if a.Preference == raw.Preference && a.NextHopSelf == raw.NextHopSelf && a.PolicyNextHop == raw.PolicyNextHop &&
		a.PolicyMED == raw.PolicyMED && attrKey(&a.Attrs) == attrKey(&raw.Attrs) {
		return raw
	}
	a.shareUnchanged(&raw.Attrs)
	return &a
}

// shareUnchanged points the attributes a policy left as they were back to
// the received ones (no second copy; they are never changed in place).
func (a *Attrs) shareUnchanged(raw *Attrs) {
	if slices.EqualFunc(a.ASPath, raw.ASPath, func(x, y Segment) bool { return x.Set == y.Set && slices.Equal(x.ASNs, y.ASNs) }) {
		a.ASPath = raw.ASPath
	}
	if slices.Equal(a.Communities, raw.Communities) {
		a.Communities = raw.Communities
	}
	if slices.Equal(a.Large, raw.Large) {
		a.Large = raw.Large
	}
	if slices.Equal(a.ClusterList, raw.ClusterList) {
		a.ClusterList = raw.ClusterList
	}
	if slices.EqualFunc(a.Unknown, raw.Unknown, func(x, y RawAttr) bool {
		return x.Flags == y.Flags && x.Type == y.Type && bytes.Equal(x.Value, y.Value)
	}) {
		a.Unknown = raw.Unknown
	}
	if a.MED != nil && raw.MED != nil && *a.MED == *raw.MED {
		a.MED = raw.MED
	}
	if a.LocalPref != nil && raw.LocalPref != nil && *a.LocalPref == *raw.LocalPref {
		a.LocalPref = raw.LocalPref
	}
}

// reimport evaluates the import policy again for every received path.
func (p *peer) reimport() {
	for pf, ip := range p.in {
		acc := p.importPath(ip.raw)
		if (acc == nil) != (ip.accepted == nil) || (acc != nil && attrKey(&acc.Attrs) != attrKey(&ip.accepted.Attrs)) {
			p.sp.dirty[pf] = true
		}
		if acc != nil {
			acc.Stale = ip.raw.Stale
		}
		ip.accepted = acc
	}
}

// readvertise sends what changed after a policy or setting change.
func (p *peer) readvertise() {
	var all []netip.Prefix
	for pf := range p.sp.best {
		all = append(all, pf)
	}
	for pf := range p.sp.local {
		all = append(all, pf)
	}
	for pf := range p.out {
		all = append(all, pf)
	}
	p.advertise(all)
}

// resend sends the family again (ROUTE-REFRESH, clear soft).
func (p *peer) resend(f Family) {
	for pf := range p.out {
		if FamilyOf(pf) == f {
			delete(p.out, pf)
		}
	}
	p.readvertise()
	if p.established() {
		p.est.send(eorMsg(f))
	}
}

// outgoing is the path announced to this neighbour for a prefix (nil:
// none).
func (p *peer) outgoing(pf netip.Prefix) *Path {
	sp := p.sp
	if !slices.Contains(p.families, FamilyOf(pf)) {
		return nil
	}
	x := sp.advertised(pf)
	if x == nil {
		return nil
	}
	reflect := false
	var cluster netip.Addr
	if !x.Local() {
		if x.Peer == p.n.Addr {
			return nil // back to where it came from
		}
		src := sp.peers[x.Peer]
		if !x.EBGP && !p.ebgp() {
			// iBGP to iBGP only by route reflection.
			srcClient := src != nil && src.n.Cluster.IsValid()
			switch {
			case srcClient:
				cluster = src.n.Cluster
			case p.n.Cluster.IsValid():
				cluster = p.n.Cluster
			default:
				return nil
			}
			reflect = true
		}
	}
	// Well-known communities.
	for _, c := range x.Communities {
		if c == 0xFFFFFF02 || (c == 0xFFFFFF01 && p.ebgp()) {
			return nil // no-advertise, no-export
		}
	}
	o := *x
	o.Attrs = x.Attrs.Clone()
	o.Stale = false
	if sp.pol.Export != nil {
		if !sp.pol.Export(&p.n, &o) {
			return nil
		}
	} else if x.Local() {
		return nil // originated paths need an export policy
	}
	self := p.selfAddr(FamilyOf(pf))
	if p.ebgp() {
		path := o.ASPath
		if p.n.RemovePrivate {
			path = removePrivate(path)
		}
		o.ASPath = path
		if p.n.LocalAS != p.sp.cfg.AS && p.n.LocalAS != 0 {
			o.Prepend(p.n.LocalAS, p.sp.cfg.AS) // local-as, then the global AS
		} else {
			o.Prepend(p.sp.cfg.AS)
		}
		o.LocalPref = nil
		if !x.Local() && !o.PolicyMED {
			o.MED = nil // a MED is not passed on to another AS
		}
		o.OriginatorID, o.ClusterList = netip.Addr{}, nil
		if !o.NextHop.IsValid() || o.NextHopSelf || !o.PolicyNextHop {
			o.NextHop, o.LinkLocal = self, netip.Addr{}
		}
	} else {
		if o.LocalPref == nil {
			lp := uint32(100)
			o.LocalPref = &lp
		}
		if x.Local() || o.NextHopSelf || !o.NextHop.IsValid() {
			o.NextHop, o.LinkLocal = self, netip.Addr{}
		}
		if reflect {
			if !o.OriginatorID.IsValid() {
				o.OriginatorID = x.RouterID()
			}
			o.ClusterList = append([]netip.Addr{cluster}, o.ClusterList...)
		}
	}
	if !o.NextHop.IsValid() || FamilyOf(netip.PrefixFrom(o.NextHop, 0)) != FamilyOf(pf) {
		return nil // no next hop of the family (configure the address)
	}
	return &o
}

// selfAddr is this switch's address towards the neighbour in a family.
func (p *peer) selfAddr(f Family) netip.Addr {
	if p.est != nil {
		if l := p.est.local(); l.IsValid() && FamilyOf(netip.PrefixFrom(l, 0)) == f {
			return l
		}
	}
	if f == IPv4Unicast {
		return p.n.NextHop4
	}
	return p.n.NextHop6
}

func removePrivate(segs []Segment) []Segment {
	var out []Segment
	for _, s := range segs {
		ns := Segment{Set: s.Set}
		for _, a := range s.ASNs {
			if !isPrivateAS(a) {
				ns.ASNs = append(ns.ASNs, a)
			}
		}
		if len(ns.ASNs) > 0 {
			out = append(out, ns)
		}
	}
	return out
}

// advertise sends the changes of the given prefixes.
func (p *peer) advertise(prefixes []netip.Prefix) {
	if !p.established() {
		return
	}
	type gkey struct {
		f Family
		k string
	}
	type group struct {
		attrs    *Attrs
		prefixes []netip.Prefix
	}
	adds := map[gkey]*group{}
	var keys []gkey
	withdraws := map[Family][]netip.Prefix{}
	seen := map[netip.Prefix]bool{}
	for _, pf := range prefixes {
		if seen[pf] {
			continue
		}
		seen[pf] = true
		o := p.outgoing(pf)
		cur := p.out[pf]
		if o == nil {
			if cur != nil {
				delete(p.out, pf)
				withdraws[FamilyOf(pf)] = append(withdraws[FamilyOf(pf)], pf)
			}
			continue
		}
		k := attrKey(&o.Attrs)
		if cur != nil && cur.key == k {
			cur.path = o
			continue
		}
		p.out[pf] = &outPath{path: o, key: k}
		gk := gkey{FamilyOf(pf), k}
		g := adds[gk]
		if g == nil {
			g = &group{attrs: &o.Attrs}
			adds[gk] = g
			keys = append(keys, gk)
		}
		g.prefixes = append(g.prefixes, pf)
	}
	for _, f := range []Family{IPv4Unicast, IPv6Unicast} {
		if w := withdraws[f]; len(w) > 0 {
			for _, m := range withdrawMsgs(f, w) {
				p.est.send(m)
			}
		}
	}
	for _, gk := range keys {
		g := adds[gk]
		for _, m := range updateMsgs(gk.f, g.attrs, p.open.AS4, g.prefixes) {
			p.est.send(m)
		}
	}
}

// attrKey identifies attributes (equal keys: equal announcements).
func attrKey(a *Attrs) string {
	med, lp := "-", "-"
	if a.MED != nil {
		med = fmt.Sprint(*a.MED)
	}
	if a.LocalPref != nil {
		lp = fmt.Sprint(*a.LocalPref)
	}
	return fmt.Sprintf("%d|%v|%v|%v|%s|%s|%v|%v|%v|%v|%v|%v", a.Origin, a.ASPath, a.NextHop, a.LinkLocal, med, lp,
		a.Communities, a.Large, a.OriginatorID, a.ClusterList, a.AtomicAggregate, a.Unknown)
}
