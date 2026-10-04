package ospfd

import (
	"context"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/bfdd"
	"github.com/thxrben/cerium-switchd/pkg/bfd"
)

// BFD for OSPF neighbours (reference 5.12, 5.8): the master asks cer-bfdd
// for a session to every neighbour in state 2-Way or higher on an interface
// with bfd-liveness-detection, and takes the neighbour down when its
// session goes from up to down (a session that never came up changes
// nothing: the neighbour may not run BFD, RFC 5882 §4.1).
//
// A session runs where the interface's OSPF packets are sent from: on the
// master when it has the device (irb, its own routed port, an MC-LAG with
// a leg on the master), otherwise on the member that relays the interface
// (relay.go). The master gives that member its sessions as a whole list
// (StackBFDSet, again every 10 s and when it changes); the member runs
// them in its cer-bfdd and reports their states (StackBFDState).

// Stacking-protocol methods of the BFD relay.
const (
	StackBFDSet   = "ospf-bfd-set"
	StackBFDState = "ospf-bfd-state"
)

// cer-bfdd clients: the master's own sessions, and those a member runs for
// the master.
const (
	bfdClient      = "ospf"
	bfdRelayClient = "ospf-relay"
)

// BFD is cer-bfdd on this member.
type BFD interface {
	Set(ctx context.Context, s bfdd.Set) error
}

// BFDSpec is an interface's bfd-liveness-detection.
type BFDSpec struct {
	IntervalMs int    `json:"interval_ms"`
	Multiplier int    `json:"multiplier"`
	AuthType   string `json:"auth_type,omitempty"` // keyed-md5, keyed-sha-1 ("": none)
	AuthKeyID  int    `json:"auth_key_id,omitempty"`
	AuthKey    string `json:"auth_key,omitempty"`
}

// RelayBFDSession is a session a member runs for the master.
type RelayBFDSession struct {
	Key  string     `json:"key"` // instance key (name/vN)
	Unit string     `json:"unit"`
	Peer netip.Addr `json:"peer"` // the neighbour's source address (OSPFv3: link-local, no zone)
	Spec BFDSpec    `json:"spec"`
}

// RelayBFD is the master's complete list of sessions for one member.
type RelayBFD struct {
	Sessions []RelayBFDSession `json:"sessions,omitempty"`
}

// RelayBFDState is a relayed session's state, reported to the master in
// order. Downs counts the session's up -> down changes, so a master that
// missed a report (or the "up" before a "down") still notices a failure.
type RelayBFDState struct {
	Key   string     `json:"key"`
	Unit  string     `json:"unit"`
	Peer  netip.Addr `json:"peer"`
	Up    bool       `json:"up"`
	Downs uint64     `json:"downs"`
}

// bfdRef identifies a session of an OSPF neighbour.
type bfdRef struct {
	key, unit string
	peer      netip.Addr
}

// bfdState is the event loop's BFD state.
type bfdState struct {
	// The master's sessions: those of cer-bfdd here by its key, and every
	// session's last state (up).
	local      map[string]bfdRef
	localSpecs []bfdd.SessionSpec // last given to cer-bfdd (nil: never)
	up         map[bfdRef]bool
	downs      map[bfdRef]uint64 // relayed sessions: the owner's count
	remote     map[int]RelayBFD  // per member, last sent
	sentAt     time.Time

	// A member's sessions for the master (relayMaster): the master's list,
	// what was given to cer-bfdd, the sessions by cer-bfdd key and their
	// states.
	relayMaster  int
	lastRelay    RelayBFD
	relayedSpecs []bfdd.SessionSpec
	relayed      map[string]bfdRef
	relayedUp    map[string]bool
	relayedDowns map[string]uint64
	reports      ordered // to the master

	// Calls to cer-bfdd and to other members, latest first.
	set  map[string]*latest
	mset map[int]*latest
}

// spec converts an interface's settings for cer-bfdd.
func (s BFDSpec) session(k bfd.Key, unit string) bfdd.SessionSpec {
	return bfdd.SessionSpec{Key: k, Interface: unit, IntervalMs: s.IntervalMs, Multiplier: s.Multiplier,
		AuthType: s.AuthType, AuthKeyID: s.AuthKeyID, AuthKey: s.AuthKey}
}

// bfdKey is cer-bfdd's key of a single-hop session: link-local peers carry
// the device as zone.
func bfdKey(vrf string, peer netip.Addr, dev string) bfd.Key {
	if peer.Is6() && peer.IsLinkLocalUnicast() && dev != "" {
		peer = peer.WithZone(dev)
	}
	return bfd.Key{Instance: vrf, Peer: peer}
}

// syncBFD converges the sessions with the neighbours (the event loop, on
// the master; a non-master drops its own sessions).
func (d *Daemon) syncBFD(now time.Time) {
	b := &d.bfd
	if b.up == nil {
		b.up, b.local, b.remote = map[bfdRef]bool{}, map[string]bfdRef{}, map[int]RelayBFD{}
	}
	var local []bfdd.SessionSpec
	localRefs := map[string]bfdRef{}
	remote := map[int]RelayBFD{}
	live := map[bfdRef]bool{}
	for _, k := range d.sortedKeys() {
		in := d.insts[k]
		ifs := map[string]Iface{}
		for _, ic := range in.cfg.Interfaces {
			ifs[ic.Unit] = ic
		}
		for _, n := range in.r.TwoWayNeighbors() {
			ic, ok := ifs[n.Iface]
			if !ok || ic.BFD == nil {
				continue
			}
			ref := bfdRef{key: k, unit: n.Iface, peer: n.Addr}
			switch owner := in.owner[n.Iface]; {
			case owner == 0 && ic.Device != "":
				bk := bfdKey(in.cfg.VRF, n.Addr, ic.Device)
				local = append(local, ic.BFD.session(bk, ic.Unit))
				localRefs[bk.String()] = ref
			case owner != 0:
				r := remote[owner]
				r.Sessions = append(r.Sessions, RelayBFDSession{Key: k, Unit: n.Iface, Peer: n.Addr, Spec: *ic.BFD})
				remote[owner] = r
			default:
				continue
			}
			live[ref] = true
		}
	}
	for ref := range b.up {
		if !live[ref] {
			delete(b.up, ref)
		}
	}
	for ref := range b.downs {
		if !live[ref] {
			delete(b.downs, ref)
		}
	}
	b.local = localRefs
	if b.localSpecs == nil || !slices.Equal(local, b.localSpecs) {
		b.localSpecs = append([]bfdd.SessionSpec{}, local...)
		d.bfdSet(bfdClient, local)
	}
	resend := now.Sub(b.sentAt) >= 10*time.Second
	if resend {
		b.sentAt = now
	}
	for m := range b.remote {
		if _, ok := remote[m]; !ok {
			remote[m] = RelayBFD{} // the member ends its sessions for us
		}
	}
	for m, r := range remote {
		if resend || !slices.Equal(r.Sessions, b.remote[m].Sessions) || len(r.Sessions) == 0 {
			d.bfdToMember(m, r)
		}
		if len(r.Sessions) == 0 {
			delete(remote, m)
		}
	}
	b.remote = remote
}

// bfdSet gives cer-bfdd a client's sessions (without waiting; the latest
// list wins).
func (d *Daemon) bfdSet(client string, ss []bfdd.SessionSpec) {
	if d.BFD == nil {
		return
	}
	b := &d.bfd
	if b.set == nil {
		b.set = map[string]*latest{}
	}
	l := b.set[client]
	if l == nil {
		l = &latest{}
		b.set[client] = l
	}
	set := bfdd.Set{Client: client, Sessions: ss}
	l.put(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.BFD.Set(ctx, set); err != nil {
			d.Log.Warn("ospf: BFD sessions not set", "client", client, "err", err)
		}
	})
}

// bfdToMember gives a member the sessions it runs for the master.
func (d *Daemon) bfdToMember(m int, r RelayBFD) {
	if d.StackCall == nil {
		return
	}
	b := &d.bfd
	if b.mset == nil {
		b.mset = map[int]*latest{}
	}
	l := b.mset[m]
	if l == nil {
		l = &latest{}
		b.mset[m] = l
	}
	l.put(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// The answer: the states the member knows (repairs lost reports).
		var states []RelayBFDState
		if err := d.StackCall(ctx, m, StackBFDSet, r, &states); err != nil {
			d.Log.Debug("ospf: BFD sessions to a member", "member", m, "err", err)
			return
		}
		for _, st := range states {
			d.RelayedBFDState(st)
		}
	})
}

// ResendBFD gives cer-bfdd the sessions again (it restarted).
func (d *Daemon) ResendBFD() {
	d.do(func() {
		d.bfd.localSpecs = nil // syncBFD sends the list again
		d.syncBFD(time.Now())
		d.setRelayedBFD(nil, true)
	})
}

// BFDChanged is a state of cer-bfdd's topic on this member (key: the
// session key's string; deleted: the session ended).
func (d *Daemon) BFDChanged(key string, up, deleted bool) {
	d.do(func() {
		b := &d.bfd
		if ref, ok := b.local[key]; ok && !deleted {
			d.bfdState(ref, up)
		}
		if ref, ok := b.relayed[key]; ok {
			if deleted {
				delete(b.relayedUp, key)
				return
			}
			if b.relayedUp[key] && !up {
				b.relayedDowns[key]++
			}
			b.relayedUp[key] = up
			st := RelayBFDState{Key: ref.key, Unit: ref.unit, Peer: ref.peer, Up: up, Downs: b.relayedDowns[key]}
			master := b.relayMaster
			if master == 0 || d.StackCall == nil {
				return
			}
			b.reports.put(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := d.StackCall(ctx, master, StackBFDState, st, nil); err != nil {
					d.Log.Debug("ospf: BFD state to the master", "err", err)
				}
			})
		}
	})
}

// RelayedBFDState is StackBFDState on the master (and the states in the
// answer to StackBFDSet).
func (d *Daemon) RelayedBFDState(s RelayBFDState) {
	d.do(func() {
		b := &d.bfd
		ref := bfdRef{key: s.Key, unit: s.Unit, peer: s.Peer}
		in := d.insts[ref.key]
		if in == nil || b.up == nil {
			return
		}
		known, seen := b.downs[ref]
		if b.downs == nil {
			b.downs = map[bfdRef]uint64{}
		}
		b.downs[ref] = s.Downs
		if seen && s.Downs > known && d.watched(in, ref) {
			// It failed since the last report, whatever was missed.
			in.r.NeighborFailed(ref.unit, ref.peer, time.Now())
			delete(b.up, ref)
			return
		}
		d.bfdState(ref, s.Up)
	})
}

// bfdState follows a session; up -> down takes the neighbour down.
func (d *Daemon) bfdState(ref bfdRef, up bool) {
	b := &d.bfd
	was, known := b.up[ref]
	in := d.insts[ref.key]
	if in == nil || (!known && !d.watched(in, ref)) {
		return
	}
	b.up[ref] = up
	if was && !up {
		in.r.NeighborFailed(ref.unit, ref.peer, time.Now())
		delete(b.up, ref)
	}
}

// watched: the neighbour is one a session is wanted for.
func (d *Daemon) watched(in *instance, ref bfdRef) bool {
	for _, n := range in.r.TwoWayNeighbors() {
		if n.Iface == ref.unit && n.Addr == ref.peer {
			return true
		}
	}
	return false
}

// SetRelayedBFD is StackBFDSet on a member: the sessions it runs for the
// master (from), answered with the states known of them.
func (d *Daemon) SetRelayedBFD(from int, r RelayBFD) []RelayBFDState {
	done := make(chan []RelayBFDState, 1)
	d.do(func() {
		d.bfd.relayMaster = from
		d.setRelayedBFD(&r, false)
		var out []RelayBFDState
		for _, k := range slices.Sorted(maps.Keys(d.bfd.relayed)) {
			if up, ok := d.bfd.relayedUp[k]; ok {
				ref := d.bfd.relayed[k]
				out = append(out, RelayBFDState{Key: ref.key, Unit: ref.unit, Peer: ref.peer, Up: up, Downs: d.bfd.relayedDowns[k]})
			}
		}
		done <- out
	})
	return <-done
}

// relayRole drops the sessions run for a master that no longer is one
// (the event loop; the new master sends its own list).
func (d *Daemon) relayRole(master bool, masterID int) {
	b := &d.bfd
	if b.relayMaster != 0 && (master || masterID != b.relayMaster) {
		b.relayMaster = 0
		d.setRelayedBFD(&RelayBFD{}, false)
		return
	}
	d.setRelayedBFD(nil, false) // the configuration may have moved a device
}

// setRelayedBFD converges the relayed sessions (r nil: the last list again,
// resolved against the current configuration; force: send it even if
// unchanged).
func (d *Daemon) setRelayedBFD(r *RelayBFD, force bool) {
	b := &d.bfd
	if r != nil {
		b.lastRelay = *r
	}
	d.mu.Lock()
	cfg := d.cfg
	d.mu.Unlock()
	ifs := map[string]Iface{}
	vrf := map[string]string{}
	for _, in := range cfg.Instances {
		vrf[in.key()] = in.VRF
		for _, ic := range in.Interfaces {
			ifs[in.key()+"|"+ic.Unit] = ic
		}
	}
	var ss []bfdd.SessionSpec
	refs := map[string]bfdRef{}
	for _, s := range b.lastRelay.Sessions {
		ic, ok := ifs[s.Key+"|"+s.Unit]
		if !ok || ic.Device == "" {
			continue // not this member's (any more)
		}
		bk := bfdKey(vrf[s.Key], s.Peer, ic.Device)
		ss = append(ss, s.Spec.session(bk, s.Unit))
		refs[bk.String()] = bfdRef{key: s.Key, unit: s.Unit, peer: s.Peer}
	}
	b.relayed = refs
	if b.relayedUp == nil {
		b.relayedUp, b.relayedDowns = map[string]bool{}, map[string]uint64{}
	}
	for k := range b.relayedUp {
		if _, ok := refs[k]; !ok {
			delete(b.relayedUp, k)
			delete(b.relayedDowns, k)
		}
	}
	if !force && b.relayedSpecs != nil && slices.Equal(ss, b.relayedSpecs) {
		return
	}
	b.relayedSpecs = append([]bfdd.SessionSpec{}, ss...)
	d.bfdSet(bfdRelayClient, ss)
}

// latest runs the newest of its jobs, one at a time: older ones that have
// not started yet are dropped.
type latest struct {
	mu      sync.Mutex
	next    func()
	running bool
}

func (l *latest) put(f func()) {
	l.mu.Lock()
	l.next = f
	if l.running {
		l.mu.Unlock()
		return
	}
	l.running = true
	l.mu.Unlock()
	go func() {
		for {
			l.mu.Lock()
			f := l.next
			l.next = nil
			if f == nil {
				l.running = false
				l.mu.Unlock()
				return
			}
			l.mu.Unlock()
			f()
		}
	}()
}

// ordered runs its jobs one at a time, in order.
type ordered struct {
	mu      sync.Mutex
	q       []func()
	running bool
}

func (o *ordered) put(f func()) {
	o.mu.Lock()
	o.q = append(o.q, f)
	if o.running {
		o.mu.Unlock()
		return
	}
	o.running = true
	o.mu.Unlock()
	go func() {
		for {
			o.mu.Lock()
			if len(o.q) == 0 {
				o.running = false
				o.mu.Unlock()
				return
			}
			f := o.q[0]
			o.q = o.q[1:]
			o.mu.Unlock()
			f()
		}
	}()
}
