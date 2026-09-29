package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"
)

type member struct {
	id   int
	key  ed25519.PrivateKey
	cert tls.Certificate
}

func newMember(t *testing.T, s *Stack, id int) member {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	c, err := s.SignMember(id, key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return member{id, key, TLSCert(c, key)}
}

// handshake runs TLS between a (client) and b (server) over loopback TCP
// (buffered, like a stacking link).
func handshake(t *testing.T, ca, cb *tls.Config) (errA, errB error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		done <- tls.Server(c, cb).Handshake()
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	errA = tls.Client(c, ca).Handshake()
	select {
	case errB = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handshake hangs")
	}
	return errA, errB
}

func TestMemberTLS(t *testing.T) {
	s, err := NewStack()
	if err != nil {
		t.Fatal(err)
	}
	m1, m2, m3 := newMember(t, s, 1), newMember(t, s, 2), newMember(t, s, 3)
	members := map[int]ed25519.PublicKey{1: m1.key.Public().(ed25519.PublicKey), 2: m2.key.Public().(ed25519.PublicKey)}
	allowed := func(id int, pub ed25519.PublicKey) bool { k, ok := members[id]; return ok && k.Equal(pub) }
	cfg := func(m member) *tls.Config { return TLSConfig(s.Cert, m.cert, allowed) }

	if a, b := handshake(t, cfg(m1), cfg(m2)); a != nil || b != nil {
		t.Fatalf("members 1 and 2: %v / %v", a, b)
	}
	// Member 3 is signed but not in the member list (e.g. removed).
	if _, b := handshake(t, cfg(m3), cfg(m2)); b == nil || !strings.Contains(b.Error(), "not a member") {
		t.Errorf("removed member accepted: %v", b)
	}
	// Another stack's member is rejected, both ways.
	other, _ := NewStack()
	o1 := newMember(t, other, 1)
	if _, b := handshake(t, TLSConfig(other.Cert, o1.cert, allowed), cfg(m2)); b == nil {
		t.Error("member of another stack accepted")
	}
	// A key signed as member 1 but not the listed key of member 1.
	if _, b := handshake(t, cfg(newMember(t, s, 1)), cfg(m2)); b == nil {
		t.Error("impostor key accepted")
	}
	// A self-signed (joining) certificate is not a member.
	_, jk, _ := ed25519.GenerateKey(rand.Reader)
	jc, _ := SelfSigned(jk)
	if _, b := handshake(t, TLSConfig(s.Cert, TLSCert(jc, jk), allowed), cfg(m2)); b == nil {
		t.Error("self-signed certificate accepted")
	}
}

func TestNoExpiryAndClock(t *testing.T) {
	s, _ := NewStack()
	m := newMember(t, s, 1)
	if !m.cert.Leaf.NotAfter.Equal(NotAfter) || m.cert.Leaf.NotAfter.Year() != 9999 {
		t.Errorf("NotAfter %v", m.cert.Leaf.NotAfter)
	}
	for _, now := range []time.Time{time.Unix(0, 0), time.Date(1999, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} {
		v := clamp(now)
		if v.Before(NotBefore) || v.After(NotAfter) {
			t.Errorf("clamp(%v) = %v outside the validity", now, v)
		}
	}
	if clamp(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).Year() != 2026 {
		t.Error("a correct clock must be used as is")
	}
}

func TestTokens(t *testing.T) {
	tok := NewToken()
	if len(strings.ReplaceAll(tok, "-", "")) != 26 || strings.Count(tok, "-") != 6 {
		t.Errorf("token format %q", tok)
	}
	n1, err := NormalizeToken(strings.ToLower(tok))
	n2, _ := NormalizeToken(strings.ReplaceAll(tok, "-", ""))
	if err != nil || n1 != n2 {
		t.Errorf("normalize: %q %q %v", n1, n2, err)
	}
	if _, err := NormalizeToken("ABCD-EFGH"); err == nil {
		t.Error("short token accepted")
	}
	s, _ := NewStack()
	_, mk, _ := ed25519.GenerateKey(rand.Reader)
	mp, sp := mk.Public().(ed25519.PublicKey), s.Key.Public().(ed25519.PublicKey)
	p := Proof(n1, "join", mp, sp)
	if !CheckProof(p, Proof(n1, "join", mp, sp)) {
		t.Error("proof mismatch")
	}
	other, _ := NewStack()
	if CheckProof(p, Proof(n1, "join", mp, other.Key.Public().(ed25519.PublicKey))) {
		t.Error("proof must bind the stack key (man in the middle)")
	}
	if CheckProof(p, Proof(n1, "admit", mp, sp)) || CheckProof(p, Proof(NewToken(), "join", mp, sp)) {
		t.Error("proof must bind label and token")
	}
}

func TestStorage(t *testing.T) {
	s, _ := NewStack()
	kb, err := EncodeKey(s.Key)
	if err != nil {
		t.Fatal(err)
	}
	k, err := DecodeKey(kb)
	if err != nil || !k.Equal(s.Key) {
		t.Fatalf("key round trip: %v", err)
	}
	c, err := DecodeCert(EncodeCert(s.Cert))
	if err != nil || !c.Equal(s.Cert) {
		t.Fatalf("cert round trip: %v", err)
	}
	if _, ok := ParseMemberName("member-17"); ok {
		t.Error("member-17 accepted")
	}
	if id, ok := ParseMemberName(MemberName(16)); !ok || id != 16 {
		t.Error("member-16")
	}
}
