package bfd

import (
	"math/rand/v2"
	"time"
)

// Config is a session's configuration (reference 5.12).
type Config struct {
	// MinTx and MinRx are the desired transmit and required receive
	// intervals; Multiplier the detection multiplier.
	MinTx, MinRx time.Duration
	Multiplier   uint8
	Auth         *Auth
	// AdminDown keeps the session in AdminDown (the neighbour sees it as
	// administratively down, not as a failure).
	AdminDown bool
}

// slowTx is the transmit interval while the session is not up (RFC 5880
// §6.8.3: at least one second).
const slowTx = time.Second

// Session is one BFD session. It is not safe for concurrent use: the
// owner serialises Receive, Tick and Configure.
type Session struct {
	cfg Config
	// Send transmits a packet (encoded).
	Send func([]byte)
	// OnChange is called on every state change.
	OnChange func(old, new State, diag Diag)

	state        State
	remoteState  State
	localDisc    uint32
	remoteDisc   uint32
	localDiag    Diag
	remoteMinRx  time.Duration
	remoteMinTx  time.Duration
	remoteMult   uint8
	remoteDemand bool
	// The intervals in use (§6.8.3: changes take effect after a poll).
	txInterval  time.Duration // our transmit interval as negotiated
	pollPending bool
	pollTx      time.Duration // the new tx while polling
	pollRx      time.Duration
	// sendFinal: answer a received poll with the next packet.
	sendFinal bool

	lastRx     time.Time
	nextTx     time.Time
	detectAt   time.Time // zero: no detection timer
	authSeq    uint32
	rxSeqKnown bool
	rxSeq      uint32

	// Statistics.
	Transitions int
	UpSince     time.Time
	RxPackets   uint64
	TxPackets   uint64
	RxDropped   uint64
}

// NewSession returns a session in Down (AdminDown when configured so)
// with a random local discriminator.
func NewSession(cfg Config, now time.Time) *Session {
	s := &Session{cfg: cfg, state: Down, remoteMinRx: time.Microsecond, localDisc: rand.Uint32() | 1, authSeq: rand.Uint32()}
	if cfg.AdminDown {
		s.state, s.localDiag = AdminDown, DiagAdminDown
	}
	s.nextTx = now
	return s
}

// State returns the session state.
func (s *Session) State() State { return s.state }

// Diag returns the local diagnostic.
func (s *Session) Diag() Diag { return s.localDiag }

// RemoteState is the neighbour's last reported state.
func (s *Session) RemoteState() State { return s.remoteState }

// LocalDisc and RemoteDisc are the discriminators.
func (s *Session) LocalDisc() uint32  { return s.localDisc }
func (s *Session) RemoteDisc() uint32 { return s.remoteDisc }

// DetectionTime is the negotiated detection time (§6.8.4): the remote
// multiplier times the agreed interval of the remote's transmissions.
func (s *Session) DetectionTime() time.Duration {
	if s.remoteMult == 0 {
		return 0
	}
	return time.Duration(s.remoteMult) * s.rxInterval()
}

// rxInterval: the neighbour sends at max(its desired tx, our required rx).
func (s *Session) rxInterval() time.Duration {
	return max(s.remoteMinTx, s.cfg.MinRx)
}

// TxInterval is our negotiated transmit interval.
func (s *Session) TxInterval() time.Duration { return s.txInterval }

func (s *Session) setState(st State, diag Diag, now time.Time) {
	old := s.state
	if old == st {
		return
	}
	s.state, s.localDiag = st, diag
	s.Transitions++
	if st == Up {
		s.UpSince = now
	}
	if st != Up {
		// Not up: send at the slow rate (RFC 5880 §6.8.3).
		s.txInterval = slowTx
	} else {
		s.txInterval = max(s.cfg.MinTx, s.remoteMinRx)
	}
	s.nextTx = now // tell the neighbour at once
	if s.OnChange != nil {
		s.OnChange(old, st, diag)
	}
}

// desiredMinTx: while not up, at least one second (§6.8.3).
func (s *Session) desiredMinTx() time.Duration {
	if s.state != Up {
		return max(s.cfg.MinTx, slowTx)
	}
	return s.cfg.MinTx
}

// Configure changes the configuration; interval changes of an Up session
// are announced with a poll sequence (§6.8.3).
func (s *Session) Configure(cfg Config, now time.Time) {
	old := s.cfg
	s.cfg = cfg
	switch {
	case cfg.AdminDown && s.state != AdminDown:
		s.setState(AdminDown, DiagAdminDown, now)
		s.detectAt = time.Time{}
		return
	case !cfg.AdminDown && s.state == AdminDown:
		s.setState(Down, DiagNone, now)
		return
	}
	if s.state == Up && (old.MinTx != cfg.MinTx || old.MinRx != cfg.MinRx) {
		s.pollPending, s.pollTx, s.pollRx = true, cfg.MinTx, cfg.MinRx
		// Keep using the old intervals until the poll is answered, except
		// a faster tx may only take effect then and a slower tx now
		// (§6.8.3).
		s.cfg.MinTx, s.cfg.MinRx = old.MinTx, old.MinRx
		if cfg.MinTx > old.MinTx {
			s.cfg.MinTx = cfg.MinTx
			s.txInterval = max(cfg.MinTx, s.remoteMinRx)
		}
		s.nextTx = now
	}
}

// Receive processes a received control packet (raw bytes, already
// decoded into p) at time now, per RFC 5880 §6.8.6.
func (s *Session) Receive(raw []byte, p *Packet, now time.Time) {
	if p.YourDisc != 0 && p.YourDisc != s.localDisc {
		s.RxDropped++
		return
	}
	if (p.AuthType != AuthNone) != (s.cfg.Auth != nil) || !Verify(raw, p, s.cfg.Auth) {
		s.RxDropped++
		return
	}
	if s.cfg.Auth != nil {
		// Keyed (not meticulous): the sequence number must not go back by
		// more than 3 * multiplier (§6.7.3).
		if s.rxSeqKnown && seqBefore(p.AuthSeq, s.rxSeq-3*uint32(max(s.remoteMult, s.cfg.Multiplier))) {
			s.RxDropped++
			return
		}
		s.rxSeq, s.rxSeqKnown = p.AuthSeq, true
	}
	s.RxPackets++
	s.lastRx = now
	s.remoteDisc = p.MyDisc
	s.remoteState = p.State
	s.remoteDemand = p.Demand
	s.remoteMinTx = time.Duration(p.DesiredMinTx) * time.Microsecond
	newRemoteMinRx := time.Duration(p.RequiredMinRx) * time.Microsecond
	s.remoteMult = p.DetectMult
	if p.Final && s.pollPending {
		s.pollPending = false
		s.cfg.MinTx, s.cfg.MinRx = s.pollTx, s.pollRx
	}
	if newRemoteMinRx != s.remoteMinRx {
		s.remoteMinRx = newRemoteMinRx
	}
	if s.state == Up {
		s.txInterval = max(s.cfg.MinTx, s.remoteMinRx)
		if s.txInterval == 0 {
			s.txInterval = slowTx
		}
	}
	if p.Poll {
		s.sendFinal = true
		s.nextTx = now
	}
	if s.state == AdminDown {
		return
	}
	if p.State == AdminDown {
		if s.state != Down {
			s.setState(Down, DiagNeighborDown, now)
		}
	} else {
		switch s.state {
		case Down:
			switch p.State {
			case Down:
				s.setState(Init, DiagNone, now)
			case Init:
				s.setState(Up, DiagNone, now)
			}
		case Init:
			if p.State == Init || p.State == Up {
				s.setState(Up, DiagNone, now)
			}
		case Up:
			if p.State == Down {
				s.setState(Down, DiagNeighborDown, now)
			}
		}
	}
	// The detection timer runs while the session is Init or Up (and, for
	// stability, in Down once the neighbour talks).
	if s.state != AdminDown {
		if d := s.DetectionTime(); d > 0 {
			s.detectAt = now.Add(d)
		}
	}
}

func seqBefore(a, b uint32) bool { return int32(a-b) < 0 }

// Tick runs the timers at time now: the detection timer and periodic
// transmission. It returns when Tick must run next.
func (s *Session) Tick(now time.Time) time.Time {
	if !s.detectAt.IsZero() && !now.Before(s.detectAt) {
		s.detectAt = time.Time{}
		if s.state == Init || s.state == Up {
			s.setState(Down, DiagTimeExpired, now)
		}
		s.remoteDisc = 0
		s.remoteState = Down
		s.rxSeqKnown = false
	}
	if !now.Before(s.nextTx) {
		s.transmit(now)
	}
	next := s.nextTx
	if !s.detectAt.IsZero() && s.detectAt.Before(next) {
		next = s.detectAt
	}
	return next
}

// transmit sends a control packet and schedules the next one with the
// jitter of §6.8.7 (75-100% of the interval; 75-90% with multiplier 1).
func (s *Session) transmit(now time.Time) {
	interval := s.txInterval
	if s.state != Up || interval == 0 {
		interval = max(slowTx, s.remoteMinRx)
	}
	if s.remoteMinRx == 0 && s.state == Up {
		// The neighbour asked for no packets (RequiredMinRx 0).
		s.nextTx = now.Add(slowTx)
		return
	}
	p := &Packet{Diag: s.localDiag, State: s.state, DetectMult: s.cfg.Multiplier, MyDisc: s.localDisc, YourDisc: s.remoteDisc,
		DesiredMinTx: uint32(s.desiredMinTx() / time.Microsecond), RequiredMinRx: uint32(s.cfg.MinRx / time.Microsecond)}
	if s.pollPending {
		p.Poll = true
		p.DesiredMinTx, p.RequiredMinRx = uint32(max(s.pollTx, time.Microsecond)/time.Microsecond), uint32(s.pollRx/time.Microsecond)
	}
	if s.sendFinal {
		p.Final, p.Poll = true, false
		s.sendFinal = false
	}
	if s.cfg.Auth != nil {
		s.authSeq++
		p.AuthSeq = s.authSeq
	}
	if s.Send != nil {
		s.Send(p.Encode(s.cfg.Auth))
	}
	s.TxPackets++
	jitter := 75 + rand.IntN(26)
	if s.cfg.Multiplier == 1 {
		jitter = 75 + rand.IntN(16)
	}
	s.nextTx = now.Add(interval * time.Duration(jitter) / 100)
}
