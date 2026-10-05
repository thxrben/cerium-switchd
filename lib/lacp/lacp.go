package lacp

import (
	"slices"
	"sort"
	"time"
)

// Timer constants (802.1AX 6.4.4).
const (
	FastPeriodic  = 1 * time.Second
	SlowPeriodic  = 30 * time.Second
	ShortTimeout  = 3 * time.Second
	LongTimeout   = 90 * time.Second
	AggregateWait = 2 * time.Second
	maxTxPerFast  = 3 // LACPDUs per FastPeriodic interval
	// deafAfter: our LACPDUs sent before a partner that does not name us
	// counts as not receiving them.
	deafAfter = 3
)

// Config is the actor configuration of one bundle.
type Config struct {
	System SystemID
	Key    uint16
	Active bool // send LACPDUs on its own (else only answer)
	Fast   bool // ask the partner for LACPDUs every second (short timeout)
}

// RxState is the receive machine state.
type RxState int

const (
	RxPortDisabled RxState = iota
	RxExpired
	RxDefaulted
	RxCurrent
)

func (s RxState) String() string {
	return [...]string{"Port disabled", "Expired", "Defaulted", "Current"}[s]
}

// MuxState is the mux machine state (coupled control).
type MuxState int

const (
	MuxDetached MuxState = iota
	MuxWaiting
	MuxAttached
	MuxCollectingDistributing
)

func (s MuxState) String() string {
	return [...]string{"Detached", "Waiting", "Attached", "Collecting distributing"}[s]
}

// Port is one member port of a bundle.
type Port struct {
	Name     string
	Number   uint16
	Priority uint16
	up       bool
	actor    State // Sync, Collecting, Distributing, Defaulted, Expired (the rest comes from Config)
	partner  Info
	rx       RxState
	mux      MuxState
	selected bool
	// held keeps the port unselected (not in sync): it cannot carry
	// traffic yet (a MACsec port before its link is secured).
	held         bool
	currentWhile time.Time // deadlines; zero: stopped
	waitWhile    time.Time
	drainUntil   time.Time // held while distributing: out once the partner stopped sending
	periodicAt   time.Time
	ntt          bool
	txTimes      []time.Time
	stats        Stats
	lastRx       time.Time
	// slowPartner: with `periodic fast`, the partner's LACPDUs arrived more
	// than ShortTimeout apart (it ignores the request to send every
	// second), so this side keeps timing out.
	slowPartner bool
	// deafPartner: the partner's LACPDUs do not name this port although
	// we sent it several: ours do not reach it (cable, transmit path or
	// the partner's port). txSinceUp counts our LACPDUs since link up.
	deafPartner bool
	txSinceUp   int
}

// Stats counts LACPDUs of a port.
type Stats struct {
	RxPDUs, TxPDUs, RxErrors uint64
}

// PortStatus is a port as shown by "show lacp interfaces".
type PortStatus struct {
	Name     string
	Actor    Info
	Partner  Info
	Rx       RxState
	Mux      MuxState
	Selected bool
	Stats    Stats
	// SlowPartner: the partner sends less often than `periodic fast`
	// needs (configure `periodic slow`).
	SlowPartner bool
	// PartnerDeaf: the partner does not receive this side's LACPDUs.
	PartnerDeaf bool
	// Held: the port cannot carry traffic yet (MACsec negotiating).
	Held bool `json:",omitempty"`
}

// BundleStatus is a bundle for "show lacp".
type BundleStatus struct {
	Name  string
	Ports []PortStatus
	// PortNames maps kernel names to configuration names.
	PortNames map[string]string
}

// Bundle runs LACP on the member ports of one aggregated interface of this
// switch.
type Bundle struct {
	cfg   Config
	ports map[string]*Port
	send  func(port string, p *PDU)
	// hold keeps every port out of the bundle (an MC-LAG leg that must not
	// attract traffic); LACP tells the partner "not in sync".
	hold bool
	// agg is the partner the aggregator is attached to (zero: none).
	agg struct {
		set    bool
		system SystemID
		key    uint16
	}
}

// NewBundle creates a bundle; send transmits an LACPDU on a port.
func NewBundle(cfg Config, send func(port string, p *PDU)) *Bundle {
	return &Bundle{cfg: cfg, ports: map[string]*Port{}, send: send}
}

// SetConfig changes the actor configuration; ports renegotiate as needed.
func (b *Bundle) SetConfig(cfg Config) {
	if cfg != b.cfg {
		b.cfg = cfg
		for _, p := range b.ports {
			p.ntt = true
		}
	}
}

// SetHold keeps all ports out of the bundle (true) or lets them in again.
func (b *Bundle) SetHold(h bool) { b.hold = h }

// Held reports whether the bundle is held.
func (b *Bundle) Held() bool { return b.hold }

// SetPortHold keeps one port out of the bundle (true: LACP tells the
// partner "not in sync" on it) or lets it in again.
func (b *Bundle) SetPortHold(name string, h bool) {
	if p := b.ports[name]; p != nil && p.held != h {
		p.held = h
		p.ntt = true
	}
}

// AddPort adds a member port (down until SetLink).
func (b *Bundle) AddPort(name string, number, priority uint16) {
	if _, ok := b.ports[name]; ok {
		return
	}
	b.ports[name] = &Port{Name: name, Number: number, Priority: priority}
}

// RemovePort removes a member port.
func (b *Bundle) RemovePort(name string) { delete(b.ports, name) }

// Ports lists the member ports.
func (b *Bundle) Ports() []string {
	out := make([]string, 0, len(b.ports))
	for n := range b.ports {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// actorInfo is what this switch announces for p.
func (b *Bundle) actorInfo(p *Port) Info {
	st := p.actor & (Sync | Collecting | Distributing | Defaulted | Expired)
	st |= Aggregation
	if b.cfg.Active {
		st |= Activity
	}
	if b.cfg.Fast {
		st |= Timeout
	}
	return Info{System: b.cfg.System, Key: b.cfg.Key, PortPriority: p.Priority, Port: p.Number, State: st}
}

// SlowPartners lists the ports whose partner sends less often than
// `periodic fast` needs.
func (b *Bundle) SlowPartners() []string {
	var out []string
	for _, n := range b.Ports() {
		// A deaf partner sends slowly because it never got our request.
		if p := b.ports[n]; p.slowPartner && b.cfg.Fast && !p.deafPartner {
			out = append(out, n)
		}
	}
	return out
}

// DeafPartners lists the ports whose partner does not receive our
// LACPDUs.
func (b *Bundle) DeafPartners() []string {
	var out []string
	for _, n := range b.Ports() {
		if b.ports[n].deafPartner {
			out = append(out, n)
		}
	}
	return out
}

// SetLink reports a port's link state (port_enabled).
func (b *Bundle) SetLink(name string, up bool, now time.Time) {
	p := b.ports[name]
	if p == nil || p.up == up {
		return
	}
	p.up = up
	p.deafPartner, p.txSinceUp = false, 0
	if !up {
		p.lastRx = time.Time{}
		p.rx = RxPortDisabled
		p.partner.State &^= Sync
		p.currentWhile, p.periodicAt = time.Time{}, time.Time{}
		p.actor &^= Expired
		return
	}
	b.enterExpired(p, now)
	p.ntt = true
}

func (b *Bundle) enterExpired(p *Port, now time.Time) {
	p.rx = RxExpired
	p.partner.State &^= Sync
	p.partner.State |= Timeout
	p.currentWhile = now.Add(ShortTimeout)
	p.actor |= Expired
}

func (b *Bundle) enterDefaulted(p *Port) {
	p.rx = RxDefaulted
	// Partner admin values: none (a defaulted port has no partner and is
	// not aggregated).
	p.partner = Info{}
	p.currentWhile = time.Time{}
	p.actor |= Defaulted
	p.actor &^= Expired
}

// Receive processes an LACPDU received on a port.
func (b *Bundle) Receive(name string, pdu *PDU, now time.Time) {
	p := b.ports[name]
	if p == nil || !p.up {
		return
	}
	p.stats.RxPDUs++
	if !p.lastRx.IsZero() {
		gap := now.Sub(p.lastRx)
		switch {
		case b.cfg.Fast && gap > ShortTimeout:
			p.slowPartner = true
		case gap <= FastPeriodic+FastPeriodic/2:
			p.slowPartner = false // it does send every second
		}
	}
	p.lastRx = now
	actor := b.actorInfo(p)
	switch pp := pdu.Partner; {
	case pp.System == actor.System && pp.Port == actor.Port && pp.Key == actor.Key:
		p.deafPartner = false
	case p.txSinceUp >= deafAfter:
		p.deafPartner = true
	}
	// updateNTT: the partner's view of us is outdated.
	pp := pdu.Partner
	const mask = Activity | Timeout | Sync | Aggregation
	if pp.Port != actor.Port || pp.PortPriority != actor.PortPriority || pp.System != actor.System ||
		pp.Key != actor.Key || pp.State&mask != actor.State&mask {
		p.ntt = true
	}
	// recordPDU
	matched := pp.Port == actor.Port && pp.PortPriority == actor.PortPriority && pp.System == actor.System &&
		pp.Key == actor.Key && pp.State.Has(Aggregation) == actor.State.Has(Aggregation)
	p.partner = pdu.Actor
	if !((matched && pdu.Actor.State.Has(Sync)) || (!pdu.Actor.State.Has(Aggregation) && pdu.Actor.State.Has(Sync))) {
		p.partner.State &^= Sync
	}
	p.actor &^= Defaulted | Expired
	p.rx = RxCurrent
	timeout := LongTimeout
	if b.cfg.Fast {
		timeout = ShortTimeout
	}
	p.currentWhile = now.Add(timeout)
}

// RxError counts a malformed LACPDU.
func (b *Bundle) RxError(name string) {
	if p := b.ports[name]; p != nil {
		p.stats.RxErrors++
	}
}

func expired(t, now time.Time) bool { return !t.IsZero() && !now.Before(t) }

// Tick runs the timers and machines; call it often (e.g. every 100 ms)
// and after every Receive/SetLink.
func (b *Bundle) Tick(now time.Time) {
	names := b.Ports()
	for _, n := range names {
		p := b.ports[n]
		// Receive machine timers.
		if expired(p.currentWhile, now) {
			switch p.rx {
			case RxCurrent:
				b.enterExpired(p, now)
			case RxExpired:
				b.enterDefaulted(p)
			}
		}
		// Periodic machine.
		if !p.up || (!b.cfg.Active && !p.partner.State.Has(Activity)) {
			p.periodicAt = time.Time{}
		} else {
			interval := SlowPeriodic
			if p.partner.State.Has(Timeout) || (p.rx == RxDefaulted && b.cfg.Fast) {
				// The partner asked for fast; without a partner, our own rate
				// (a partner that appears is found quickly).
				interval = FastPeriodic
			}
			switch {
			case p.periodicAt.IsZero():
				p.periodicAt = now.Add(interval)
			case !now.Before(p.periodicAt):
				p.ntt = true
				p.periodicAt = now.Add(interval)
			case p.periodicAt.Sub(now) > interval:
				p.periodicAt = now.Add(interval) // the partner asked for faster
			}
		}
	}
	b.selection(names)
	for _, n := range names {
		b.muxStep(b.ports[n], now)
	}
	for _, n := range names {
		b.transmit(b.ports[n], now)
	}
}

// selection attaches the bundle's aggregator to one partner: the one it
// has now if a port with it is attached (carries or is about to carry
// traffic), else the partner of the port with the lowest number. Until a
// port is attached (the wait-while time), a lower-numbered port's partner
// takes over, so the choice does not depend on which partner was heard
// first. Ports with another partner stay unselected.
func (b *Bundle) selection(names []string) {
	candidate := func(p *Port) bool {
		return p.up && (p.rx == RxCurrent || p.rx == RxExpired) && p.partner.State.Has(Aggregation) &&
			p.partner.System.MAC != [6]byte{}
	}
	keep := false
	for _, n := range names {
		p := b.ports[n]
		if b.agg.set && candidate(p) && p.partner.System == b.agg.system && p.partner.Key == b.agg.key && p.mux >= MuxAttached {
			keep = true
		}
	}
	if !keep {
		b.agg.set = false
		var first *Port
		for _, n := range names {
			p := b.ports[n]
			if candidate(p) && (first == nil || p.Number < first.Number) {
				first = p
			}
		}
		if first != nil {
			b.agg.set, b.agg.system, b.agg.key = true, first.partner.System, first.partner.Key
		}
	}
	for _, n := range names {
		p := b.ports[n]
		p.selected = !b.hold && !p.held && b.agg.set && candidate(p) && p.partner.System == b.agg.system && p.partner.Key == b.agg.key
	}
}

func (b *Bundle) muxStep(p *Port, now time.Time) {
	for range 4 { // follow immediate transitions
		prev := p.mux
		switch p.mux {
		case MuxDetached:
			if p.selected {
				p.mux, p.waitWhile = MuxWaiting, now.Add(AggregateWait)
			}
		case MuxWaiting:
			switch {
			case !p.selected:
				b.detach(p)
			case expired(p.waitWhile, now):
				p.mux = MuxAttached
				p.actor |= Sync
				p.ntt = true
			}
		case MuxAttached:
			switch {
			case !p.selected:
				b.detach(p)
			case p.partner.State.Has(Sync):
				p.mux = MuxCollectingDistributing
				p.actor |= Collecting | Distributing
				p.ntt = true
			}
		case MuxCollectingDistributing:
			if b.draining(p, now) {
				break
			}
			if !p.selected || !p.partner.State.Has(Sync) {
				p.mux = MuxAttached
				p.actor &^= Collecting | Distributing
				p.ntt = true
			}
		}
		if p.mux == prev {
			return
		}
	}
}

// DrainWait bounds how long a held port keeps collecting while its partner
// takes it out of its bundle.
const DrainWait = 2 * time.Second

// draining handles a port that is held while collecting and distributing:
// the partner is told "not in sync" first and the port stays in the kernel
// bundle until the partner reports it no longer distributes to it (frames
// on the way still arrive), at most DrainWait. True: stay.
func (b *Bundle) draining(p *Port, now time.Time) bool {
	if p.selected {
		if !p.drainUntil.IsZero() {
			p.drainUntil = time.Time{}
			p.actor |= Sync
			p.ntt = true
		}
		return false
	}
	if !b.hold || !p.up || p.rx != RxCurrent {
		return false
	}
	if p.drainUntil.IsZero() {
		p.drainUntil = now.Add(DrainWait)
		p.actor &^= Sync
		p.ntt = true
	}
	return p.partner.State.Has(Distributing) && !expired(p.drainUntil, now)
}

func (b *Bundle) detach(p *Port) {
	p.drainUntil = time.Time{}
	p.mux = MuxDetached
	if p.actor.Has(Sync) {
		p.ntt = true
	}
	p.actor &^= Sync | Collecting | Distributing
	p.waitWhile = time.Time{}
}

// transmit sends an LACPDU if one is due, at most maxTxPerFast per second.
func (b *Bundle) transmit(p *Port, now time.Time) {
	if !p.ntt || !p.up || (!b.cfg.Active && !p.partner.State.Has(Activity)) {
		return
	}
	p.txTimes = slices.DeleteFunc(p.txTimes, func(t time.Time) bool { return now.Sub(t) >= FastPeriodic })
	if len(p.txTimes) >= maxTxPerFast {
		return
	}
	p.txTimes = append(p.txTimes, now)
	p.ntt = false
	p.stats.TxPDUs++
	p.txSinceUp++
	b.send(p.Name, &PDU{Actor: b.actorInfo(p), Partner: p.partner})
}

// Distributing lists the ports that carry traffic (collecting and
// distributing).
func (b *Bundle) Distributing() []string {
	var out []string
	for _, n := range b.Ports() {
		if b.ports[n].mux == MuxCollectingDistributing {
			out = append(out, n)
		}
	}
	return out
}

// Status reports every port.
func (b *Bundle) Status() []PortStatus {
	var out []PortStatus
	for _, n := range b.Ports() {
		p := b.ports[n]
		out = append(out, PortStatus{Name: n, Actor: b.actorInfo(p), Partner: p.partner, Rx: p.rx, Mux: p.mux, Selected: p.selected, Stats: p.stats,
			SlowPartner: p.slowPartner && b.cfg.Fast && !p.deafPartner, PartnerDeaf: p.deafPartner, Held: p.held})
	}
	return out
}

// PortSnapshot is a port's negotiated state, kept across switchd restarts
// so that a restart does not disturb the partner.
type PortSnapshot struct {
	Partner   Info     `json:"partner"`
	Mux       MuxState `json:"mux"`
	AggSystem SystemID `json:"agg_system"`
	AggKey    uint16   `json:"agg_key"`
}

// Snapshot returns the negotiated state of the ports that carry traffic or
// are attached.
func (b *Bundle) Snapshot() map[string]PortSnapshot {
	out := map[string]PortSnapshot{}
	for n, p := range b.ports {
		if p.mux >= MuxAttached && b.agg.set {
			out[n] = PortSnapshot{Partner: p.partner, Mux: p.mux, AggSystem: b.agg.system, AggKey: b.agg.key}
		}
	}
	return out
}

// Restore continues a port where a previous run left it (the port is up;
// the caller checked that it still carries traffic). If the partner does
// not confirm within the timeout, the port expires as usual.
func (b *Bundle) Restore(name string, s PortSnapshot, now time.Time) {
	p := b.ports[name]
	if p == nil || s.Mux < MuxAttached {
		return
	}
	p.up = true
	p.partner = s.Partner
	p.rx = RxCurrent
	timeout := LongTimeout
	if b.cfg.Fast {
		timeout = ShortTimeout
	}
	p.currentWhile = now.Add(timeout)
	p.mux = s.Mux
	p.actor = Sync
	if s.Mux == MuxCollectingDistributing {
		p.actor |= Collecting | Distributing
	}
	p.selected = true
	b.agg.set, b.agg.system, b.agg.key = true, s.AggSystem, s.AggKey
}
