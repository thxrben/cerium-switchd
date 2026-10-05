// Package pki holds the stack's minimal key infrastructure
// (docs/stack-protocol.md, "Keys and joining"): Ed25519 keys, certificates
// that never expire, clock-independent verification and join tokens.
package pki

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
)

// Validity of every certificate: nothing ever expires (RFC 5280 4.1.2.5
// uses 99991231235959Z for "no well-defined expiration date").
var (
	NotBefore = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	NotAfter  = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
)

// VerifyTime returns the current time clamped into the validity range, so a
// member with a wrong clock still verifies certificates.
func VerifyTime() time.Time { return clamp(time.Now()) }

func clamp(now time.Time) time.Time {
	lo, hi := NotBefore.Add(24*time.Hour), NotAfter.Add(-24*time.Hour)
	switch {
	case now.Before(lo):
		return lo
	case now.After(hi):
		return hi
	}
	return now
}

// Stack is the stack's signing key and its self-signed certificate.
type Stack struct {
	Key  ed25519.PrivateKey
	Cert *x509.Certificate
}

// NewStack creates a stack key; id is a random stack identifier shown in
// the certificate's subject.
func NewStack() (*Stack, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	var idb [8]byte
	rand.Read(idb[:])
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: fmt.Sprintf("ceros stack %x", idb)},
		NotBefore: NotBefore, NotAfter: NotAfter, IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		// The stack certificate also authenticates the admitting side of a
		// join.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Stack{Key: key, Cert: cert}, nil
}

// MemberName is the certificate subject of member id.
func MemberName(id int) string { return "member-" + strconv.Itoa(id) }

// ParseMemberName returns the member id of a certificate subject.
func ParseMemberName(cn string) (int, bool) {
	n, ok := strings.CutPrefix(cn, "member-")
	id, err := strconv.Atoi(n)
	return id, ok && err == nil && id >= 1 && id <= 16 && strconv.Itoa(id) == n
}

// SignMember issues the certificate of member id for its public key.
func (s *Stack) SignMember(id int, pub ed25519.PublicKey) (*x509.Certificate, error) {
	var serial [16]byte
	rand.Read(serial[:])
	tmpl := &x509.Certificate{
		SerialNumber: new(big.Int).SetBytes(serial[:]), Subject: pkix.Name{CommonName: MemberName(id)},
		NotBefore: NotBefore, NotAfter: NotAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.Cert, pub, s.Key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// SelfSigned makes a certificate for a key that has not joined yet.
func SelfSigned(key ed25519.PrivateKey) (*x509.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "joining"},
		NotBefore: NotBefore, NotAfter: NotAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// Allowed reports whether member id with this public key is a member of
// the stack (from the replicated member list).
type Allowed func(id int, pub ed25519.PublicKey) bool

// TLSConfig returns the configuration for a stacking link between members:
// TLS 1.3, both sides authenticate with certificates signed by the stack
// key, and the peer must be in the member list.
func TLSConfig(stackCert *x509.Certificate, own tls.Certificate, allowed Allowed) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(stackCert)
	verify := func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("stack peer sent no certificate")
		}
		leaf := cs.PeerCertificates[0]
		opts := x509.VerifyOptions{Roots: pool, CurrentTime: VerifyTime(),
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}
		for _, c := range cs.PeerCertificates[1:] {
			if opts.Intermediates == nil {
				opts.Intermediates = x509.NewCertPool()
			}
			opts.Intermediates.AddCert(c)
		}
		if _, err := leaf.Verify(opts); err != nil {
			return fmt.Errorf("stack peer certificate: %w", err)
		}
		id, ok := ParseMemberName(leaf.Subject.CommonName)
		pub, isEd := leaf.PublicKey.(ed25519.PublicKey)
		if !ok || !isEd || !allowed(id, pub) {
			return fmt.Errorf("%s is not a member of this stack", leaf.Subject.CommonName)
		}
		return nil
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{own},
		// Verification is done in VerifyConnection (clamped time, member
		// list); the standard checks would use the real clock.
		InsecureSkipVerify: true, ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: verify,
	}
}

// ALPN names of the stacking TLS sessions (docs/stack-protocol.md): they
// let a protocol analyser pick the dissector for the decrypted data.
const (
	ALPNMember = "swstack/1"
	ALPNJoin   = "swjoin/1"
)

// KeyLogEnv names the environment variable that makes switchd write the
// session keys of its stacking TLS sessions to a file (NSS key log format),
// so captures can be decrypted with Wireshark. For debugging only.
const KeyLogEnv = "SWITCHD_TLS_KEYLOG"

var (
	keyLogOnce sync.Once
	keyLog     io.Writer
)

// Wire completes a stacking TLS configuration: its ALPN name and, when
// KeyLogEnv is set, the key log.
func Wire(c *tls.Config, alpn string) *tls.Config {
	c.NextProtos = []string{alpn}
	keyLogOnce.Do(func() {
		if path := os.Getenv(KeyLogEnv); path != "" {
			if f, err := hwio.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
				keyLog = f
			}
		}
	})
	if keyLog != nil {
		c.KeyLogWriter = keyLog
	}
	return c
}

// KeyLogging reports whether session keys are written (see KeyLogEnv).
func KeyLogging() bool {
	Wire(&tls.Config{}, "")
	return keyLog != nil
}

// TLSCert combines a certificate and its key.
func TLSCert(cert *x509.Certificate, key ed25519.PrivateKey) tls.Certificate {
	return tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert}
}

// ---- join tokens ----

var tokenEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewToken returns a one-time join token (128 bits) in a form that can be
// typed on a console: groups of 4 base32 characters.
func NewToken() string {
	var b [16]byte
	rand.Read(b[:])
	s := tokenEnc.EncodeToString(b[:])
	var parts []string
	for len(s) > 4 {
		parts, s = append(parts, s[:4]), s[4:]
	}
	return strings.Join(append(parts, s), "-")
}

// NormalizeToken accepts a token typed with or without dashes, in any case.
func NormalizeToken(t string) (string, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(t), "-", ""))
	b, err := tokenEnc.DecodeString(s)
	if err != nil || len(b) != 16 {
		return "", errors.New("invalid join token")
	}
	return s, nil
}

// Proof binds a join step to the keys both sides saw in the TLS handshake:
// HMAC-SHA256(token, label | member key | stack key).
func Proof(token, label string, member, stack ed25519.PublicKey) []byte {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(label))
	m.Write([]byte{0})
	m.Write(member)
	m.Write(stack)
	return m.Sum(nil)
}

// CheckProof compares proofs in constant time.
func CheckProof(got, want []byte) bool { return subtle.ConstantTimeCompare(got, want) == 1 }

// ---- storage ----

// EncodeKey / DecodeKey store keys as PEM (PKCS #8).
func EncodeKey(k ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func DecodeKey(b []byte) (ed25519.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("no PEM key")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return ek, nil
}

func EncodeCert(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

func DecodeCert(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}
