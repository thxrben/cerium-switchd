package bfd

import (
	"bytes"
	"net/netip"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPacketRoundTrip(t *testing.T) {
	p := &Packet{Diag: DiagTimeExpired, State: Up, Poll: true, DetectMult: 3, MyDisc: 0x11223344, YourDisc: 0x55667788,
		DesiredMinTx: 300000, RequiredMinRx: 250000}
	b := p.Encode(nil)
	if len(b) != 24 || b[0] != 0x21 || b[1] != 0xe0 || b[3] != 24 {
		t.Fatalf("encoding % x", b)
	}
	q, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(q, p) {
		t.Fatalf("decoded %+v", q)
	}
	for _, auth := range []*Auth{{Type: AuthKeyedMD5, KeyID: 2, Key: []byte("secret")}, {Type: AuthKeyedSHA1, KeyID: 9, Key: []byte("other secret")}} {
		p.AuthSeq = 77
		b := p.Encode(auth)
		q, err := Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		if !Verify(b, q, auth) || q.AuthSeq != 77 || q.AuthKeyID != auth.KeyID {
			t.Fatalf("auth %d: %+v", auth.Type, q)
		}
		wrong := *auth
		wrong.Key = []byte("guess")
		if Verify(b, q, &wrong) {
			t.Fatal("wrong key verified")
		}
		b[10] ^= 1 // tamper with YourDisc
		q, _ = Decode(b)
		if q != nil && Verify(b, q, auth) {
			t.Fatal("tampered packet verified")
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	good := (&Packet{State: Down, DetectMult: 3, MyDisc: 1}).Encode(nil)
	cases := map[string]func([]byte) []byte{
		"short":      func(b []byte) []byte { return b[:20] },
		"version":    func(b []byte) []byte { b[0] = 2 << 5; return b },
		"mult 0":     func(b []byte) []byte { b[2] = 0; return b },
		"multipoint": func(b []byte) []byte { b[1] |= flagMultipoint; return b },
		"my disc 0":  func(b []byte) []byte { b[4], b[5], b[6], b[7] = 0, 0, 0, 0; return b },
		"poll+final": func(b []byte) []byte { b[1] |= flagPoll | flagFinal; return b },
		"up, your 0": func(b []byte) []byte { b[1] = byte(Up) << 6; return b },
		"length":     func(b []byte) []byte { b[3] = 30; return b },
	}
	for name, mut := range cases {
		if _, err := Decode(mut(bytes.Clone(good))); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func FuzzDecode(f *testing.F) {
	f.Add((&Packet{State: Up, DetectMult: 3, MyDisc: 1, YourDisc: 2}).Encode(nil))
	f.Add((&Packet{State: Init, DetectMult: 3, MyDisc: 1, YourDisc: 2}).Encode(&Auth{Type: AuthKeyedSHA1, Key: []byte("k")}))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := Decode(b)
		if err != nil {
			return
		}
		Verify(b, p, &Auth{Type: AuthKeyedMD5, Key: []byte("k")})
		if p.AuthType == AuthNone {
			if q, err := Decode(p.Encode(nil)); err != nil || !reflect.DeepEqual(q, p) {
				t.Fatalf("re-encode %+v -> %+v %v", p, q, err)
			}
		}
	})
}

// pair is two sessions connected by a lossless (or broken) link, driven by
// a fake clock.
type pair struct {
	a, b    *Session
	now     time.Time
	cut     bool
	changes []string
}

func newPair(t *testing.T, ca, cb Config) *pair {
	pr := &pair{now: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	pr.a, pr.b = NewSession(ca, pr.now), NewSession(cb, pr.now)
	link := func(to **Session) func([]byte) {
		return func(raw []byte) {
			if pr.cut {
				return
			}
			p, err := Decode(raw)
			if err != nil {
				t.Fatalf("sent an invalid packet: %v", err)
			}
			(*to).Receive(raw, p, pr.now)
		}
	}
	pr.a.Send, pr.b.Send = link(&pr.b), link(&pr.a)
	pr.a.OnChange = func(o, n State, d Diag) { pr.changes = append(pr.changes, "a:"+n.String()) }
	pr.b.OnChange = func(o, n State, d Diag) { pr.changes = append(pr.changes, "b:"+n.String()) }
	return pr
}

// run advances the clock by d in 1 ms steps.
func (pr *pair) run(d time.Duration) {
	for end := pr.now.Add(d); pr.now.Before(end); pr.now = pr.now.Add(time.Millisecond) {
		pr.a.Tick(pr.now)
		pr.b.Tick(pr.now)
	}
}

var fast = Config{MinTx: 100 * time.Millisecond, MinRx: 100 * time.Millisecond, Multiplier: 3}

func TestSessionComesUp(t *testing.T) {
	pr := newPair(t, fast, fast)
	pr.run(3 * time.Second)
	if pr.a.State() != Up || pr.b.State() != Up {
		t.Fatalf("states %v %v (%v)", pr.a.State(), pr.b.State(), pr.changes)
	}
	if d := pr.a.DetectionTime(); d != 300*time.Millisecond {
		t.Fatalf("detection time %v", d)
	}
	// Up: packets at about 100 ms (75-100 ms with jitter).
	before := pr.a.TxPackets
	pr.run(time.Second)
	if n := pr.a.TxPackets - before; n < 10 || n > 14 {
		t.Fatalf("%d packets in 1 s", n)
	}
}

func TestFailureDetectionTime(t *testing.T) {
	pr := newPair(t, fast, fast)
	pr.run(3 * time.Second)
	pr.cut = true
	cutAt := pr.now
	var downAt time.Time
	pr.a.OnChange = func(o, n State, d Diag) {
		if n == Down && downAt.IsZero() {
			downAt = pr.now
			if d != DiagTimeExpired {
				t.Errorf("diag %v", d)
			}
		}
	}
	pr.run(time.Second)
	if downAt.IsZero() {
		t.Fatal("not detected")
	}
	if dt := downAt.Sub(cutAt); dt > 300*time.Millisecond || dt < 200*time.Millisecond {
		t.Fatalf("detected after %v (detection time 300 ms)", dt)
	}
	// The link returns: up again.
	pr.cut = false
	pr.run(4 * time.Second)
	if pr.a.State() != Up || pr.b.State() != Up {
		t.Fatalf("not up again: %v %v", pr.a.State(), pr.b.State())
	}
}

func TestSlowerSideWins(t *testing.T) {
	slow := Config{MinTx: 500 * time.Millisecond, MinRx: 500 * time.Millisecond, Multiplier: 3}
	pr := newPair(t, fast, slow)
	pr.run(4 * time.Second)
	if pr.a.State() != Up {
		t.Fatal("not up")
	}
	if pr.a.TxInterval() != 500*time.Millisecond || pr.a.DetectionTime() != 1500*time.Millisecond {
		t.Fatalf("a tx %v detect %v", pr.a.TxInterval(), pr.a.DetectionTime())
	}
}

func TestAdminDownIsNotAFailure(t *testing.T) {
	pr := newPair(t, fast, fast)
	pr.run(3 * time.Second)
	var diag Diag
	pr.a.OnChange = func(o, n State, d Diag) { diag = d }
	ad := fast
	ad.AdminDown = true
	pr.b.Configure(ad, pr.now)
	pr.run(time.Second)
	if pr.b.State() != AdminDown || pr.a.State() != Down || diag != DiagNeighborDown {
		t.Fatalf("%v %v %v", pr.a.State(), pr.b.State(), diag)
	}
	pr.b.Configure(fast, pr.now)
	pr.run(4 * time.Second)
	if pr.a.State() != Up || pr.b.State() != Up {
		t.Fatal("not up after admin up")
	}
}

func TestPollSequenceChangesTimers(t *testing.T) {
	pr := newPair(t, fast, fast)
	pr.run(3 * time.Second)
	trans := pr.a.Transitions
	faster := Config{MinTx: 50 * time.Millisecond, MinRx: 50 * time.Millisecond, Multiplier: 3}
	pr.a.Configure(faster, pr.now)
	pr.b.Configure(faster, pr.now)
	pr.run(time.Second)
	if pr.a.TxInterval() != 50*time.Millisecond || pr.a.DetectionTime() != 150*time.Millisecond || pr.a.State() != Up {
		t.Fatalf("tx %v detect %v %v", pr.a.TxInterval(), pr.a.DetectionTime(), pr.a.State())
	}
	if pr.a.Transitions != trans {
		t.Fatalf("timer change flapped the session (%d transitions, %d before)", pr.a.Transitions, trans)
	}
}

func TestAuthenticatedSession(t *testing.T) {
	key := &Auth{Type: AuthKeyedSHA1, KeyID: 1, Key: []byte("bfd-key")}
	ca, cb := fast, fast
	ca.Auth, cb.Auth = key, key
	pr := newPair(t, ca, cb)
	pr.run(3 * time.Second)
	if pr.a.State() != Up {
		t.Fatal("authenticated session not up")
	}
	// Mismatched key: never up.
	cb.Auth = &Auth{Type: AuthKeyedSHA1, KeyID: 1, Key: []byte("other")}
	pr2 := newPair(t, ca, cb)
	pr2.run(3 * time.Second)
	if pr2.a.State() == Up || pr2.a.RxDropped == 0 {
		t.Fatalf("up with a wrong key: %v, dropped %d", pr2.a.State(), pr2.a.RxDropped)
	}
	// One side without authentication: never up.
	pr3 := newPair(t, ca, fast)
	pr3.run(3 * time.Second)
	if pr3.a.State() == Up || pr3.b.State() == Up {
		t.Fatal("up with authentication on one side only")
	}
}

// TestStateMachine checks the transitions of RFC 5880 §6.8.6 directly.
func TestStateMachine(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		from, remote State
		want         State
	}{
		{Down, Down, Init}, {Down, Init, Up}, {Down, Up, Down}, {Down, AdminDown, Down},
		{Init, Down, Init}, {Init, Init, Up}, {Init, Up, Up}, {Init, AdminDown, Down},
		{Up, Down, Down}, {Up, Init, Up}, {Up, Up, Up}, {Up, AdminDown, Down},
	} {
		s := NewSession(fast, now)
		s.state = tc.from
		p := &Packet{State: tc.remote, DetectMult: 3, MyDisc: 42, DesiredMinTx: 100000, RequiredMinRx: 100000}
		if tc.remote != Down && tc.remote != AdminDown {
			p.YourDisc = s.LocalDisc()
		}
		s.Receive(p.Encode(nil), p, now)
		if s.State() != tc.want {
			t.Errorf("%v + remote %v: %v, want %v", tc.from, tc.remote, s.State(), tc.want)
		}
	}
	// A packet for another discriminator is dropped.
	s := NewSession(fast, now)
	p := &Packet{State: Init, DetectMult: 3, MyDisc: 42, YourDisc: s.LocalDisc() + 1}
	s.Receive(p.Encode(nil), p, now)
	if s.State() != Down || s.RxDropped != 1 {
		t.Fatal("foreign discriminator accepted")
	}
}

// memNet connects servers in memory: a packet from one server's session to
// a peer address is delivered to the server owning that address.
type memNet struct {
	mu      sync.Mutex
	servers map[netip.Addr]*Server
	cut     bool
}

type memT struct {
	n    *memNet
	self netip.Addr
}

func (t memT) Send(k Key, b []byte) error {
	t.n.mu.Lock()
	dst, cut := t.n.servers[k.Peer], t.n.cut
	t.n.mu.Unlock()
	if dst != nil && !cut {
		go dst.Input(Input{Instance: k.Instance, From: t.self, To: k.Peer, Multihop: k.Multihop, TTL: 255, Raw: bytes.Clone(b)})
	}
	return nil
}
func (memT) Open(string, bool) error { return nil }
func (memT) Close(string, bool)      {}

func TestServers(t *testing.T) {
	n := &memNet{servers: map[netip.Addr]*Server{}}
	a1, a2 := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2")
	s1, s2 := NewServer(memT{n, a1}, nil), NewServer(memT{n, a2}, nil)
	n.servers[a1], n.servers[a2] = s1, s2
	go s1.Run()
	go s2.Run()
	defer s1.Stop()
	defer s2.Stop()
	var ups, downs atomic.Int32
	c := Client{Name: "ospf", OnChange: func(up bool) {
		if up {
			ups.Add(1)
		} else {
			downs.Add(1)
		}
	}}
	cfg := Config{MinTx: 50 * time.Millisecond, MinRx: 50 * time.Millisecond, Multiplier: 3}
	k1, k2 := Key{Peer: a2}, Key{Peer: a1}
	if err := s1.Add(k1, "1/0/5.0", c, cfg); err != nil {
		t.Fatal(err)
	}
	s2.Add(k2, "1/0/1.0", Client{Name: "ospf"}, cfg)
	waitFor(t, func() bool { return s1.Up(k1) && s2.Up(k2) }, 5*time.Second)
	if ups.Load() != 1 {
		t.Fatalf("up notifications %d", ups.Load())
	}
	// A second client shares the session.
	s1.Add(k1, "1/0/5.0", Client{Name: "bgp"}, Config{MinTx: time.Second, MinRx: time.Second, Multiplier: 5})
	if st := s1.Sessions(); len(st) != 1 || len(st[0].Clients) != 2 || st[0].State != Up {
		t.Fatalf("sessions %+v", st)
	}
	// Cut: detected quickly, clients told.
	n.mu.Lock()
	n.cut = true
	n.mu.Unlock()
	start := time.Now()
	waitFor(t, func() bool { return downs.Load() == 1 }, 3*time.Second)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("detected after %v", d)
	}
	n.mu.Lock()
	n.cut = false
	n.mu.Unlock()
	waitFor(t, func() bool { return s1.Up(k1) }, 5*time.Second)
	// Removing the last client ends the session; the peer sees AdminDown
	// (no failure, Down with diag neighbor signalled).
	s1.Remove(k1, "ospf")
	s1.Remove(k1, "bgp")
	waitFor(t, func() bool { return !s2.Up(k2) }, 3*time.Second)
	if st := s2.Sessions(); st[0].Diag != DiagNeighborDown {
		t.Fatalf("peer diag %v", st[0].Diag)
	}
	if len(s1.Sessions()) != 0 {
		t.Fatal("session kept")
	}
}

func waitFor(t *testing.T, f func() bool, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatal("timeout")
}
