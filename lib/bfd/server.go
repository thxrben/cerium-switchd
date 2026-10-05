package bfd

import (
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"
)

// Key identifies a session: routing instance, neighbour address, and
// whether it is multihop (RFC 5883, port 4784).
type Key struct {
	Instance string
	Peer     netip.Addr
	Local    netip.Addr // source address (multihop; single-hop: optional)
	Multihop bool
}

func (k Key) String() string {
	s := k.Peer.String()
	if k.Instance != "" {
		s += " (" + k.Instance + ")"
	}
	if k.Multihop {
		s += " multihop"
	}
	return s
}

// Transport sends and receives BFD packets.
type Transport interface {
	// Send sends a packet for the session (UDP to 3784 or 4784, TTL 255).
	Send(k Key, b []byte) error
	// Open prepares receiving for the instance (sockets); Close ends it.
	Open(instance string, multihop bool) error
	Close(instance string, multihop bool)
}

// Packet from the network, handed to Server.Input by the transport.
type Input struct {
	Instance string
	From     netip.Addr
	To       netip.Addr
	Multihop bool
	TTL      int
	Raw      []byte
}

// Client is a routing protocol using a session; Up/down is reported to
// every client of a session.
type Client struct {
	Name     string // "ospf", "bgp", shown in show bfd session
	OnChange func(up bool)
}

type session struct {
	key       Key
	s         *Session
	ifc       string // interface (show)
	clients   map[string]Client
	cfgByName map[string]Config
}

// Server runs the BFD sessions of a member.
type Server struct {
	T   Transport
	Log *slog.Logger
	Now func() time.Time

	mu       sync.Mutex
	sessions map[Key]*session
	byDisc   map[uint32]*session
	open     map[[2]any]int // (instance, multihop) -> sessions
	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
}

// NewServer returns a server; Run drives its timers.
func NewServer(t Transport, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{T: t, Log: log, Now: time.Now, sessions: map[Key]*session{}, byDisc: map[uint32]*session{},
		open: map[[2]any]int{}, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
}

// Add starts (or joins) the session for k for a client with its
// configuration. Several clients share a session; the fastest
// configuration of them is used.
func (sv *Server) Add(k Key, ifc string, c Client, cfg Config) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	ss := sv.sessions[k]
	if ss == nil {
		ok := [2]any{k.Instance, k.Multihop}
		if sv.open[ok] == 0 {
			if err := sv.T.Open(k.Instance, k.Multihop); err != nil {
				return fmt.Errorf("bfd: %w", err)
			}
		}
		sv.open[ok]++
		ss = &session{key: k, ifc: ifc, clients: map[string]Client{}, cfgByName: map[string]Config{}}
		ss.s = NewSession(cfg, sv.Now())
		ss.s.Send = func(b []byte) {
			if err := sv.T.Send(k, b); err != nil {
				sv.Log.Debug("bfd: send", "peer", k, "err", err)
			}
		}
		ss.s.OnChange = func(old, new State, diag Diag) {
			sv.Log.Info("bfd: session "+new.String(), "peer", k.String(), "interface", ss.ifc, "diag", diag.String())
			up := new == Up
			if old == Up || up {
				for _, cl := range ss.clients {
					if cl.OnChange != nil {
						go cl.OnChange(up)
					}
				}
			}
		}
		sv.sessions[k] = ss
		sv.byDisc[ss.s.LocalDisc()] = ss
	}
	ss.clients[c.Name] = c
	ss.cfgByName[c.Name] = cfg
	ss.s.Configure(merged(ss.cfgByName), sv.Now())
	sv.kick()
	return nil
}

// merged: the fastest intervals and smallest multiplier of the clients;
// the authentication of the first client that has one.
func merged(cs map[string]Config) Config {
	var out Config
	first := true
	for _, c := range cs {
		if first {
			out = c
			first = false
			continue
		}
		out.MinTx, out.MinRx = min(out.MinTx, c.MinTx), min(out.MinRx, c.MinRx)
		out.Multiplier = min(out.Multiplier, c.Multiplier)
		if out.Auth == nil {
			out.Auth = c.Auth
		}
	}
	return out
}

// Remove drops a client from a session; the last client ends it (the
// neighbour sees AdminDown first, so it does not count it as a failure).
func (sv *Server) Remove(k Key, client string) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	ss := sv.sessions[k]
	if ss == nil {
		return
	}
	delete(ss.clients, client)
	delete(ss.cfgByName, client)
	if len(ss.clients) > 0 {
		ss.s.Configure(merged(ss.cfgByName), sv.Now())
		return
	}
	c := ss.s.cfg
	c.AdminDown = true
	ss.s.OnChange = nil
	ss.s.Configure(c, sv.Now())
	ss.s.Tick(sv.Now()) // tell the neighbour
	delete(sv.sessions, k)
	delete(sv.byDisc, ss.s.LocalDisc())
	ok := [2]any{k.Instance, k.Multihop}
	if sv.open[ok]--; sv.open[ok] <= 0 {
		delete(sv.open, ok)
		sv.T.Close(k.Instance, k.Multihop)
	}
}

// Up reports whether the session for k is up.
func (sv *Server) Up(k Key) bool {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	ss := sv.sessions[k]
	return ss != nil && ss.s.State() == Up
}

// Input processes a received packet (RFC 5881: single-hop packets must
// have TTL 255; RFC 5880 §6.8.6 demultiplexing by Your Discriminator, or
// by addresses while it is 0).
func (sv *Server) Input(in Input) {
	p, err := Decode(in.Raw)
	if err != nil {
		return
	}
	if !in.Multihop && in.TTL != 255 && in.TTL != 0 {
		return
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	var ss *session
	if p.YourDisc != 0 {
		ss = sv.byDisc[p.YourDisc]
	} else {
		ss = sv.sessions[Key{Instance: in.Instance, Peer: in.From, Multihop: in.Multihop}]
		if ss == nil {
			for k, s := range sv.sessions {
				if k.Instance == in.Instance && k.Peer == in.From && k.Multihop == in.Multihop {
					ss = s
				}
			}
		}
	}
	if ss == nil || ss.key.Peer != in.From || ss.key.Instance != in.Instance {
		return
	}
	ss.s.Receive(in.Raw, p, sv.Now())
	sv.kick()
}

func (sv *Server) kick() {
	select {
	case sv.wake <- struct{}{}:
	default:
	}
}

// Run drives the timers until Stop.
func (sv *Server) Run() {
	defer close(sv.done)
	t := time.NewTimer(time.Second)
	defer t.Stop()
	for {
		sv.mu.Lock()
		now := sv.Now()
		next := now.Add(time.Second)
		for _, ss := range sv.sessions {
			if n := ss.s.Tick(now); n.Before(next) {
				next = n
			}
		}
		sv.mu.Unlock()
		t.Reset(max(time.Until(next), time.Millisecond))
		select {
		case <-sv.stop:
			return
		case <-sv.wake:
		case <-t.C:
		}
	}
}

// Stop ends Run and every session.
func (sv *Server) Stop() {
	sv.mu.Lock()
	keys := make([]Key, 0, len(sv.sessions))
	for k := range sv.sessions {
		keys = append(keys, k)
	}
	sv.mu.Unlock()
	for _, k := range keys {
		sv.mu.Lock()
		ss := sv.sessions[k]
		var names []string
		if ss != nil {
			for n := range ss.clients {
				names = append(names, n)
			}
		}
		sv.mu.Unlock()
		for _, n := range names {
			sv.Remove(k, n)
		}
	}
	close(sv.stop)
	<-sv.done
}

// Status is one session for "show bfd session".
type Status struct {
	Key         Key
	Interface   string
	State       State
	RemoteState State
	Diag        Diag
	Interval    time.Duration
	Multiplier  uint8
	Detection   time.Duration
	Clients     []string
	UpSince     time.Time
	Transitions int
	LocalDisc   uint32
	RemoteDisc  uint32
	Rx, Tx      uint64
	RxDropped   uint64
}

// Sessions returns the status of every session.
func (sv *Server) Sessions() []Status {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	var out []Status
	for k, ss := range sv.sessions {
		st := Status{Key: k, Interface: ss.ifc, State: ss.s.State(), RemoteState: ss.s.RemoteState(), Diag: ss.s.Diag(),
			Interval: ss.s.TxInterval(), Multiplier: ss.s.cfg.Multiplier, Detection: ss.s.DetectionTime(),
			UpSince: ss.s.UpSince, Transitions: ss.s.Transitions, LocalDisc: ss.s.LocalDisc(), RemoteDisc: ss.s.RemoteDisc(),
			Rx: ss.s.RxPackets, Tx: ss.s.TxPackets, RxDropped: ss.s.RxDropped}
		for n := range ss.clients {
			st.Clients = append(st.Clients, n)
		}
		out = append(out, st)
	}
	return out
}
