package link

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// cable is a simulated stacking cable between two endpoints.
type cable struct {
	mu                 sync.Mutex
	rnd                *rand.Rand
	loss, dup, reorder float64
	down               bool
}

type end struct {
	c    *cable
	in   chan []byte
	peer *end
	mtu  int
	sent int
}

func newCable(seed uint64, loss, dup, reorder float64) (*end, *end) {
	c := &cable{rnd: rand.New(rand.NewPCG(seed, 1)), loss: loss, dup: dup, reorder: reorder}
	a := &end{c: c, in: make(chan []byte, 4096), mtu: 1500}
	b := &end{c: c, in: make(chan []byte, 4096), mtu: 1500}
	a.peer, b.peer = b, a
	return a, b
}

func (e *end) MTU() int              { return e.mtu }
func (e *end) Frames() <-chan []byte { return e.in }

func (e *end) Send(p []byte) error {
	if len(p) > e.mtu {
		return errors.New("frame larger than the MTU")
	}
	c := e.c
	c.mu.Lock()
	e.sent++
	down := c.down
	lost := c.rnd.Float64() < c.loss
	dup := c.rnd.Float64() < c.dup
	delay := time.Duration(0)
	if c.rnd.Float64() < c.reorder {
		delay = time.Duration(c.rnd.IntN(5)+1) * time.Millisecond
	}
	c.mu.Unlock()
	if down || lost {
		return nil
	}
	deliver := func() {
		select {
		case e.peer.in <- append([]byte(nil), p...):
		default: // receive queue full: dropped, like a NIC ring
		}
	}
	if delay > 0 {
		time.AfterFunc(delay, deliver)
	} else {
		deliver()
	}
	if dup {
		deliver()
	}
	return nil
}

func waitUp(t *testing.T, ls ...*Link) {
	t.Helper()
	for _, l := range ls {
		select {
		case <-l.Up():
		case <-time.After(5 * time.Second):
			t.Fatal("link did not come up")
		}
	}
}

// transfer sends n random bytes each way and checks exact, ordered delivery.
func transfer(t *testing.T, a, b *Link, n int, seed uint64) {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, 2))
	mk := func() []byte {
		d := make([]byte, n)
		for i := range d {
			d[i] = byte(r.IntN(256))
		}
		return d
	}
	da, db := mk(), mk()
	var wg sync.WaitGroup
	check := func(from, to *Link, data []byte, dir string) {
		defer wg.Done()
		wr := rand.New(rand.NewPCG(seed, uint64(len(dir)+int(dir[0]))))
		go func() {
			// Writes in odd sizes.
			for off := 0; off < len(data); {
				k := min(len(data)-off, 1+wr.IntN(5000))
				if _, err := from.Write(data[off : off+k]); err != nil {
					t.Errorf("%s write: %v", dir, err)
					return
				}
				off += k
			}
		}()
		to.SetReadDeadline(time.Now().Add(60 * time.Second))
		got := make([]byte, len(data))
		if _, err := io.ReadFull(to, got); err != nil {
			t.Errorf("%s read: %v", dir, err)
			return
		}
		if !bytes.Equal(got, data) {
			t.Errorf("%s: data corrupted", dir)
		}
	}
	wg.Add(2)
	go check(a, b, da, "a->b")
	go check(b, a, db, "b->a")
	wg.Wait()
}

func TestCleanCable(t *testing.T) {
	ea, eb := newCable(1, 0, 0, 0)
	a, b := New(ea, Options{}), New(eb, Options{})
	defer a.Close()
	defer b.Close()
	waitUp(t, a, b)
	start := time.Now()
	transfer(t, a, b, 4<<20, 1)
	t.Logf("4 MiB each way in %v", time.Since(start))
}

func TestLossyCable(t *testing.T) {
	for _, c := range []struct {
		name               string
		loss, dup, reorder float64
	}{
		{"loss 5%", 0.05, 0, 0},
		{"loss 20%", 0.2, 0, 0},
		{"duplicates", 0, 0.2, 0},
		{"reordering", 0, 0, 0.2},
		{"everything", 0.1, 0.1, 0.1},
	} {
		t.Run(c.name, func(t *testing.T) {
			ea, eb := newCable(7, c.loss, c.dup, c.reorder)
			// Stream correctness under loss; a real cable this bad would
			// (correctly) be declared down by the default 100 ms x 3.
			o := Options{Multiplier: 20}
			a, b := New(ea, o), New(eb, o)
			defer a.Close()
			defer b.Close()
			waitUp(t, a, b)
			transfer(t, a, b, 512<<10, 3)
		})
	}
}

// A small receive window must not stall or corrupt the stream.
func TestSmallWindow(t *testing.T) {
	ea, eb := newCable(3, 0.05, 0, 0.05)
	a, b := New(ea, Options{RecvWindow: 3000, Multiplier: 20}), New(eb, Options{RecvWindow: 3000, Multiplier: 20})
	defer a.Close()
	defer b.Close()
	waitUp(t, a, b)
	transfer(t, a, b, 200<<10, 4)
}

// A restarted peer (new epoch) ends the old stream on both sides, and new
// links on the same cable find each other again.
func TestRestart(t *testing.T) {
	ea, eb := newCable(5, 0, 0, 0)
	a, b := New(ea, Options{}), New(eb, Options{})
	waitUp(t, a, b)
	transfer(t, a, b, 10000, 5)

	// b's process restarts: its old instance just stops, a new one starts.
	b.fail(errors.New("simulated crash"))
	b2 := New(eb, Options{})
	select {
	case <-a.Done():
		if !errors.Is(a.Err(), ErrPeerRestarted) {
			t.Errorf("a ended with %v", a.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart not detected")
	}
	a2 := New(ea, Options{})
	defer a2.Close()
	defer b2.Close()
	waitUp(t, a2, b2)
	transfer(t, a2, b2, 10000, 6)
}

func TestCloseAndDeadTime(t *testing.T) {
	ea, eb := newCable(9, 0, 0, 0)
	a, b := New(ea, Options{Interval: 50 * time.Millisecond, Multiplier: 3}), New(eb, Options{Interval: 50 * time.Millisecond, Multiplier: 3})
	waitUp(t, a, b)
	a.Close()
	select {
	case <-b.Done():
		if !errors.Is(b.Err(), ErrPeerReset) {
			t.Errorf("b ended with %v", b.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close not seen by the peer")
	}
	if _, err := a.Write([]byte("x")); err == nil {
		t.Error("write on a closed link")
	}

	// A cable that goes dead ends the link after DeadTime.
	ec, ed := newCable(10, 0, 0, 0)
	c, d := New(ec, Options{Interval: 50 * time.Millisecond, Multiplier: 3}), New(ed, Options{Interval: 50 * time.Millisecond, Multiplier: 3})
	waitUp(t, c, d)
	ec.c.mu.Lock()
	ec.c.down = true
	ec.c.mu.Unlock()
	select {
	case <-c.Done():
		if !errors.Is(c.Err(), ErrDead) {
			t.Errorf("c ended with %v", c.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dead cable not detected")
	}
	d.Close()
}

func TestReadDeadline(t *testing.T) {
	ea, eb := newCable(11, 0, 0, 0)
	a, b := New(ea, Options{}), New(eb, Options{})
	defer a.Close()
	defer b.Close()
	waitUp(t, a, b)
	a.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	var ne interface{ Timeout() bool }
	if _, err := a.Read(make([]byte, 10)); !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("read deadline: %v", err)
	}
}

// Frames from anything else on the cable never disturb a link.
func TestGarbageFrames(t *testing.T) {
	ea, eb := newCable(12, 0, 0, 0)
	a, b := New(ea, Options{}), New(eb, Options{})
	defer a.Close()
	defer b.Close()
	waitUp(t, a, b)
	r := rand.New(rand.NewPCG(1, 1))
	for i := 0; i < 2000; i++ {
		g := make([]byte, r.IntN(100))
		for j := range g {
			g[j] = byte(r.IntN(256))
		}
		if len(g) > 4 && i%2 == 0 {
			g[0], g[1], g[2] = 0x53, 0x54, 1 // valid magic, random rest
		}
		eb.in <- g
	}
	transfer(t, a, b, 50000, 7)
}

func FuzzDecode(f *testing.F) {
	f.Add((&frame{Type: tData, Epoch: 1, PeerEpoch: 2, Seq: 3, Ack: 4, Window: 6400, Payload: []byte("hello")}).encode())
	f.Add([]byte{0x53, 0x54, 1, 2})
	f.Fuzz(func(t *testing.T, b []byte) {
		fr, err := decode(b)
		if err != nil {
			return
		}
		again, err := decode(fr.encode())
		if err != nil || again.Type != fr.Type || again.Seq != fr.Seq || !bytes.Equal(again.Payload, fr.Payload) {
			t.Fatalf("round trip: %v %+v %+v", err, fr, again)
		}
	})
}

// TLS 1.3 with mutual authentication runs over a link (as in the stack),
// also on a lossy cable.
func TestTLSOverLink(t *testing.T) {
	ca, caKey := testCA(t)
	cert := func(name string) tls.Certificate { return testCert(t, ca, caKey, name) }
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	ea, eb := newCable(13, 0.05, 0.02, 0.05)
	a, b := New(ea, Options{Multiplier: 20}), New(eb, Options{Multiplier: 20})
	defer a.Close()
	defer b.Close()
	srv := tls.Server(b, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert("member-2")},
		ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})
	cli := tls.Client(a, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert("member-1")},
		RootCAs: pool, ServerName: "member-2"})
	errc := make(chan error, 1)
	go func() { errc <- srv.Handshake() }()
	if err := cli.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if n := srv.ConnectionState().PeerCertificates[0].Subject.CommonName; n != "member-1" {
		t.Errorf("peer %q", n)
	}
	msg := bytes.Repeat([]byte("stack"), 50000)
	go cli.Write(msg)
	got := make([]byte, len(msg))
	srv.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(srv, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("tls transfer: %v", err)
	}
}

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "stack CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(crand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return ca, key
}

func testCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(crand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// The larger interval of both sides is used, so a slow member is not
// declared dead by a fast one.
func TestIntervalNegotiation(t *testing.T) {
	ea, eb := newCable(14, 0, 0, 0)
	a := New(ea, Options{Interval: 20 * time.Millisecond, Multiplier: 3})
	b := New(eb, Options{Interval: 200 * time.Millisecond, Multiplier: 5})
	defer a.Close()
	defer b.Close()
	waitUp(t, a, b)
	time.Sleep(50 * time.Millisecond)
	for _, l := range []*Link{a, b} {
		l.mu.Lock()
		iv, m := l.interval, l.multiplier
		l.mu.Unlock()
		if iv != 200*time.Millisecond || m != 5 {
			t.Errorf("effective %v x %d", iv, m)
		}
	}
	// b sends only every 200 ms; a (20 ms x 3 on its own) must not drop it.
	time.Sleep(700 * time.Millisecond)
	select {
	case <-a.Done():
		t.Fatalf("a dropped the slow peer: %v", a.Err())
	default:
	}
}
