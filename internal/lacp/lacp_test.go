package lacp

import (
	"bytes"
	"slices"
	"testing"
	"time"
)

// sim wires bundles together: frames sent on a port arrive on the port it
// is cabled to (unless that direction is cut).
type sim struct {
	now     time.Time
	bundles map[string]*Bundle // name -> bundle
	cable   map[string]string  // "A/p1" -> "B/p1"
	cut     map[string]bool    // sending side "A/p1" cut
	sent    map[string]int
	queue   []delivery
}

type delivery struct {
	to  string
	pdu PDU
}

func newSim() *sim {
	return &sim{now: time.Unix(1000, 0), bundles: map[string]*Bundle{}, cable: map[string]string{}, cut: map[string]bool{}, sent: map[string]int{}}
}

func (s *sim) add(name string, cfg Config, ports ...string) *Bundle {
	b := NewBundle(cfg, nil)
	b.send = func(port string, p *PDU) {
		from := name + "/" + port
		s.sent[from]++
		if to, ok := s.cable[from]; ok && !s.cut[from] {
			// Through the wire format.
			q, err := ParseFrame(p.Frame([]byte{2, 0, 0, 0, 0, 1}))
			if err != nil {
				panic(err)
			}
			s.queue = append(s.queue, delivery{to, *q})
		}
	}
	for i, p := range ports {
		b.AddPort(p, uint16(i+1), 32768)
	}
	s.bundles[name] = b
	return b
}

func (s *sim) connect(a, b string) {
	s.cable[a], s.cable[b] = b, a
	for _, e := range []string{a, b} {
		bn, port := split(e)
		s.bundles[bn].SetLink(port, true, s.now)
	}
}

func split(e string) (string, string) {
	i := bytes.IndexByte([]byte(e), '/')
	return e[:i], e[i+1:]
}

// run advances time in 100 ms steps.
func (s *sim) run(d time.Duration) {
	for end := s.now.Add(d); s.now.Before(end); s.now = s.now.Add(100 * time.Millisecond) {
		for _, b := range s.bundles {
			b.Tick(s.now)
		}
		for len(s.queue) > 0 {
			q := s.queue[0]
			s.queue = s.queue[1:]
			bn, port := split(q.to)
			if s.bundles[bn] == nil {
				continue
			}
			s.bundles[bn].Receive(port, &q.pdu, s.now)
			s.bundles[bn].Tick(s.now)
		}
	}
}

func sys(last byte) SystemID { return SystemID{Priority: 32768, MAC: [6]byte{2, 0, 0, 0, 0, last}} }

func dist(b *Bundle) []string { return b.Distributing() }

func TestBundleForms(t *testing.T) {
	s := newSim()
	a := s.add("A", Config{System: sys(1), Key: 1, Active: true, Fast: true}, "p1", "p2")
	b := s.add("B", Config{System: sys(2), Key: 9, Active: true, Fast: true}, "p1", "p2")
	s.connect("A/p1", "B/p1")
	s.connect("A/p2", "B/p2")
	s.run(4 * time.Second)
	if !slices.Equal(dist(a), []string{"p1", "p2"}) || !slices.Equal(dist(b), []string{"p1", "p2"}) {
		t.Fatalf("after 4 s: A %v B %v\n%+v", dist(a), dist(b), a.Status())
	}
	st := a.Status()[0]
	if st.Partner.System != sys(2) || st.Partner.Key != 9 || st.Rx != RxCurrent || !st.Actor.State.Has(Sync|Collecting|Distributing) {
		t.Errorf("status: %+v", st)
	}
	// Steady state: one LACPDU per second per port (fast), not more.
	before := s.sent["A/p1"]
	s.run(10 * time.Second)
	if n := s.sent["A/p1"] - before; n < 9 || n > 11 {
		t.Errorf("%d LACPDUs in 10 s on a fast port", n)
	}
}

func TestActivePassive(t *testing.T) {
	s := newSim()
	a := s.add("A", Config{System: sys(1), Key: 1, Active: true, Fast: true}, "p1")
	b := s.add("B", Config{System: sys(2), Key: 1, Active: false, Fast: true}, "p1")
	s.connect("A/p1", "B/p1")
	s.run(4 * time.Second)
	if len(dist(a)) != 1 || len(dist(b)) != 1 {
		t.Fatalf("active-passive did not form: %+v %+v", a.Status(), b.Status())
	}

	s2 := newSim()
	c := s2.add("C", Config{System: sys(3), Key: 1, Fast: true}, "p1")
	s2.add("D", Config{System: sys(4), Key: 1, Fast: true}, "p1")
	s2.connect("C/p1", "D/p1")
	s2.run(10 * time.Second)
	if len(dist(c)) != 0 || s2.sent["C/p1"] != 0 {
		t.Errorf("passive-passive: distributing %v, sent %d", dist(c), s2.sent["C/p1"])
	}
}

func TestPartnerLoss(t *testing.T) {
	s := newSim()
	a := s.add("A", Config{System: sys(1), Key: 1, Active: true, Fast: true}, "p1", "p2")
	s.add("B", Config{System: sys(2), Key: 1, Active: true, Fast: true}, "p1", "p2")
	s.connect("A/p1", "B/p1")
	s.connect("A/p2", "B/p2")
	s.run(4 * time.Second)
	// B stops sending on p2 (a unidirectional failure): A leaves p2 out
	// within the short timeout, p1 keeps working.
	s.cut["B/p2"] = true
	s.run(3500 * time.Millisecond)
	if !slices.Equal(dist(a), []string{"p1"}) {
		t.Fatalf("after partner loss on p2: %v", dist(a))
	}
	s.run(4 * time.Second)
	if st := a.Status()[1]; st.Rx != RxDefaulted || !st.Actor.State.Has(Defaulted) || st.Selected {
		t.Errorf("p2 not defaulted: %+v", st)
	}
	// Restored: p2 joins again.
	delete(s.cut, "B/p2")
	s.run(4 * time.Second)
	if !slices.Equal(dist(a), []string{"p1", "p2"}) {
		t.Errorf("after restore: %v", dist(a))
	}
	// Link down: out at once.
	a.SetLink("p1", false, s.now)
	a.Tick(s.now)
	if !slices.Equal(dist(a), []string{"p2"}) {
		t.Errorf("after link down: %v", dist(a))
	}
}

// A port cabled to another system (a cabling error) stays out of the
// bundle; the ports to the bundle's partner are not affected.
func TestWrongPartner(t *testing.T) {
	s := newSim()
	a := s.add("A", Config{System: sys(1), Key: 1, Active: true, Fast: true}, "p1", "p2")
	s.add("B", Config{System: sys(2), Key: 1, Active: true, Fast: true}, "p1")
	s.add("C", Config{System: sys(3), Key: 1, Active: true, Fast: true}, "p1")
	s.connect("A/p1", "B/p1")
	s.connect("A/p2", "C/p1")
	s.run(5 * time.Second)
	if !slices.Equal(dist(a), []string{"p1"}) {
		t.Fatalf("distributing %v", dist(a))
	}
	if st := a.Status()[1]; st.Selected || st.Partner.System != sys(3) {
		t.Errorf("p2: %+v", st)
	}
	if len(dist(s.bundles["C"])) != 0 {
		t.Error("the wrong partner aggregates the port")
	}
}

// A partner that wants slow LACPDUs gets one every 30 s; the actor's
// short timeout still applies to what it receives.
func TestSlowPartner(t *testing.T) {
	s := newSim()
	s.add("A", Config{System: sys(1), Key: 1, Active: true, Fast: true}, "p1")
	b := s.add("B", Config{System: sys(2), Key: 1, Active: true, Fast: false}, "p1")
	s.connect("A/p1", "B/p1")
	s.run(5 * time.Second)
	before := s.sent["A/p1"]
	s.run(60 * time.Second)
	if n := s.sent["A/p1"] - before; n > 3 {
		t.Errorf("A sent %d LACPDUs in 60 s to a slow partner", n)
	}
	if n := s.sent["B/p1"]; n < 60 {
		t.Errorf("B sent only %d LACPDUs to a fast partner", n)
	}
	if len(dist(b)) != 1 {
		t.Errorf("slow partner not distributing: %+v", b.Status())
	}
}

func TestPDUFormat(t *testing.T) {
	p := &PDU{Actor: Info{System: sys(1), Key: 0x1234, PortPriority: 0x8000, Port: 0x0401, State: Activity | Timeout | Aggregation | Sync},
		Partner: Info{System: sys(2), Key: 7, PortPriority: 255, Port: 3, State: Aggregation}}
	f := p.Frame([]byte{2, 0, 0, 0, 0, 1})
	if len(f) != 124 || !bytes.Equal(f[:6], Dest) || f[12] != 0x88 || f[13] != 0x09 || f[14] != 1 || f[15] != 1 {
		t.Fatalf("header: % x", f[:16])
	}
	// Actor TLV at 16: type 1, length 20, system priority, MAC, key, port
	// priority, port, state.
	if !bytes.Equal(f[16:33], []byte{1, 20, 0x80, 0, 2, 0, 0, 0, 0, 1, 0x12, 0x34, 0x80, 0, 4, 1, 0x0f}) {
		t.Errorf("actor TLV: % x", f[16:33])
	}
	if f[36] != 2 || f[37] != 20 || f[56] != 3 || f[57] != 16 || f[72] != 0 || f[73] != 0 {
		t.Errorf("TLV layout: % x", f[36:74])
	}
	q, err := ParseFrame(f)
	if err != nil || *q != *p {
		t.Fatalf("round trip: %+v %v", q, err)
	}
	if _, err := ParseFrame(f[:40]); err == nil {
		t.Error("short frame accepted")
	}
}

// A restarted switch that restores its ports' state keeps the bundle: the
// partner never sees it out of sync.
func TestRestoreAfterRestart(t *testing.T) {
	s := newSim()
	a := s.add("A", Config{System: sys(1), Key: 1, Active: true, Fast: true}, "p1", "p2")
	b := s.add("B", Config{System: sys(2), Key: 1, Active: true, Fast: true}, "p1", "p2")
	s.connect("A/p1", "B/p1")
	s.connect("A/p2", "B/p2")
	s.run(4 * time.Second)
	snap := a.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot: %v", snap)
	}
	// "Restart" A: a new bundle, restored from the snapshot.
	a2 := s.add("A", Config{System: sys(1), Key: 1, Active: true, Fast: true}, "p1", "p2")
	for n, ps := range snap {
		a2.Restore(n, ps, s.now)
	}
	lost := false
	for i := 0; i < 50; i++ {
		s.run(100 * time.Millisecond)
		if len(dist(b)) != 2 || len(dist(a2)) != 2 {
			lost = true
		}
	}
	if lost {
		t.Errorf("bundle disturbed by the restart: A %v B %v", dist(a2), dist(b))
	}
	_ = a
}

func FuzzUnmarshal(f *testing.F) {
	p := &PDU{Actor: Info{System: sys(1), Key: 1, Port: 1, State: Activity}}
	f.Add(p.Marshal())
	f.Fuzz(func(t *testing.T, b []byte) {
		q, err := Unmarshal(b)
		if err != nil {
			return
		}
		r, err := Unmarshal(q.Marshal())
		if err != nil || *r != *q {
			t.Fatalf("round trip: %+v %+v %v", q, r, err)
		}
	})
}

// Without a partner, an active port keeps sending at its own rate.
func TestDefaultedRate(t *testing.T) {
	s := newSim()
	s.add("A", Config{System: sys(1), Key: 1, Active: true, Fast: true}, "p1")
	s.cable["A/p1"] = "nowhere/p1"
	s.bundles["A"].SetLink("p1", true, s.now)
	s.run(10 * time.Second)
	before := s.sent["A/p1"]
	s.run(10 * time.Second)
	if n := s.sent["A/p1"] - before; n < 9 {
		t.Errorf("%d LACPDUs in 10 s without a partner", n)
	}
}
