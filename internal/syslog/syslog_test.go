package syslog

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newHub(t *testing.T) (*Hub, *slog.Logger) {
	h := NewHub(slog.NewTextHandler(io.Discard, nil), 100)
	t.Cleanup(h.Close)
	return h, slog.New(h.Handler())
}

func port(a net.Addr) int {
	_, p, _ := net.SplitHostPort(a.String())
	n, _ := strconv.Atoi(p)
	return n
}

func TestFormat(t *testing.T) {
	m := Message{Time: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC), Facility: "change-log", Severity: Notice, Text: "a\nb"}
	got := Format(m, "sw1")
	want := "<181>1 2026-09-29T12:00:00.000000Z sw1 switchd " + strconv.Itoa(os.Getpid()) + " - - a b"
	if got != want {
		t.Errorf("Format:\n%s\n%s", got, want)
	}
}

func TestRingAndHandler(t *testing.T) {
	h, log := newHub(t)
	log.Info("hello", "user", "alice", "note", "two words")
	log.Warn("careful", FacilityAttr, "authorization")
	r := h.Recent()
	if len(r) != 2 || r[0].Text != `hello user=alice note="two words"` || r[0].Severity != Info || r[0].Facility != "daemon" {
		t.Fatalf("ring: %+v", r)
	}
	if r[1].Facility != "authorization" || r[1].Severity != Warning || strings.Contains(r[1].Text, "facility") {
		t.Errorf("facility attr: %+v", r[1])
	}
	for i := 0; i < 150; i++ {
		log.Info("m" + strconv.Itoa(i))
	}
	r = h.Recent()
	if len(r) != 100 || r[0].Text != "m50" || r[99].Text != "m149" {
		t.Errorf("ring wrap: %d %q %q", len(r), r[0].Text, r[len(r)-1].Text)
	}
	h.Configure(nil, nil, 10)
	if r = h.Recent(); len(r) != 10 || r[9].Text != "m149" {
		t.Errorf("resize kept %d, last %q", len(r), r[len(r)-1].Text)
	}
}

func TestUDPAndFilters(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	h, log := newHub(t)
	h.Configure([]Host{{Host: "127.0.0.1", Port: port(pc.LocalAddr()), Transport: "udp", Facility: "change-log", Severity: "warning"}},
		func() string { return "sw1" }, 0)
	log.Warn("wrong facility")
	log.Info("too low", FacilityAttr, "change-log")
	log.Warn("committed", FacilityAttr, "change-log")
	pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); !strings.HasPrefix(got, "<180>1 ") || !strings.HasSuffix(got, " sw1 switchd "+strconv.Itoa(os.Getpid())+" - - committed") {
		t.Errorf("udp message: %q", got)
	}
	pc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := pc.ReadFrom(buf); err == nil {
		t.Error("filtered message was sent")
	}
}

// readFramed reads octet-counted messages.
func readFramed(t *testing.T, r *bufio.Reader, n int) []string {
	t.Helper()
	var out []string
	for len(out) < n {
		l, err := r.ReadString(' ')
		if err != nil {
			t.Fatal(err)
		}
		size, err := strconv.Atoi(strings.TrimSpace(l))
		if err != nil {
			t.Fatalf("bad frame length %q", l)
		}
		msg := make([]byte, size)
		if _, err := io.ReadFull(r, msg); err != nil {
			t.Fatal(err)
		}
		out = append(out, string(msg))
	}
	return out
}

func TestTCPQueuesWhileServerIsDown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := port(l.Addr())
	l.Close() // server down
	h, log := newHub(t)
	h.Configure([]Host{{Host: "127.0.0.1", Port: p, Transport: "tcp", Facility: "any", Severity: "any"}}, nil, 0)
	for i := 0; i < 5; i++ {
		log.Info("queued" + strconv.Itoa(i))
	}
	time.Sleep(200 * time.Millisecond)
	st := h.Stats()
	if len(st) != 1 || st[0].Connected || st[0].Queued != 5 || st[0].LastError == "" {
		t.Fatalf("stats while down: %+v", st)
	}
	l, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msgs := readFramed(t, bufio.NewReader(c), 5)
	for i, m := range msgs {
		if !strings.HasSuffix(m, "queued"+strconv.Itoa(i)) {
			t.Errorf("message %d out of order: %q", i, m)
		}
	}
}

func selfSigned(t *testing.T, dir string) (tls.Certificate, string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, ca
}

func TestTLS(t *testing.T) {
	cert, ca := selfSigned(t, t.TempDir())
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	h, log := newHub(t)
	h.Configure([]Host{{Host: "localhost", Port: port(l.Addr()), Transport: "tls", Facility: "any", Severity: "info", CAFile: ca}}, nil, 0)
	log.Info("over tls")
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if m := readFramed(t, bufio.NewReader(c), 1); !strings.HasSuffix(m[0], "over tls") {
		t.Errorf("tls message: %q", m[0])
	}

	// A server whose certificate does not verify gets nothing.
	h2, log2 := newHub(t)
	h2.Configure([]Host{{Host: "localhost", Port: port(l.Addr()), Transport: "tls", Facility: "any", Severity: "info"}}, nil, 0)
	log2.Info("secret")
	go func() {
		if c, err := l.Accept(); err == nil {
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	time.Sleep(500 * time.Millisecond)
	if st := h2.Stats(); len(st) != 1 || st[0].Connected || !strings.Contains(st[0].LastError, "certificate") {
		t.Errorf("unverified server: %+v", st)
	}
}

func TestReconfigureKeepsForwarders(t *testing.T) {
	h, _ := newHub(t)
	a := Host{Host: "127.0.0.1", Port: 1, Transport: "tcp"}
	b := Host{Host: "127.0.0.1", Port: 2, Transport: "tcp"}
	h.Configure([]Host{a, b}, nil, 0)
	fa := h.fwds[a]
	h.Configure([]Host{a}, nil, 0)
	if h.fwds[a] != fa || len(h.fwds) != 1 {
		t.Error("reconfiguration replaced an unchanged forwarder or kept a removed one")
	}
}
