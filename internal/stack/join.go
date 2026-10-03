package stack

import (
	"bufio"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/thxrben/cerium-switchd/internal/stack/pki"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// JoinTimeout bounds a join attempt.
const JoinTimeout = 60 * time.Second

// AddMember prepares the join of member id and returns the one-time token
// for "request virtual-chassis join token <t>" on the new switch.
func (m *Manager) AddMember(id int) (string, error) {
	if id < 1 || id > 16 {
		return "", errors.New("member id 1-16")
	}
	if id == m.Member() {
		return "", fmt.Errorf("member %d is this switch", id)
	}
	tok := pki.NewToken()
	n, _ := pki.NormalizeToken(tok)
	if m.Control != nil {
		if _, taken := m.Control.Members()[id]; taken {
			return "", fmt.Errorf("member %d is already in the stack (request virtual-chassis member remove %d first)", id, id)
		}
		if err := m.Control.AddToken(id, n, time.Hour); err != nil {
			return "", fmt.Errorf("storing the token in the stack: %w", err)
		}
		m.Log.Info("stack: join token issued", "facility", "authorization", "member", id)
		return tok, nil
	}
	m.mu.Lock()
	for k, p := range m.pending {
		if p.id == id || time.Now().After(p.expires) {
			delete(m.pending, k) // one token per member; expired ones go
		}
	}
	m.pending[n] = pendingMember{id: id, expires: time.Now().Add(time.Hour)}
	m.mu.Unlock()
	m.Log.Info("stack: join token issued", "facility", "authorization", "member", id)
	return tok, nil
}

// Join makes this switch join the stack on its VC ports that issued the
// token. On success OnJoined is called with the new member id.
func (m *Manager) Join(token string) (int, error) {
	n, err := pki.NormalizeToken(token)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	if m.join != nil {
		m.mu.Unlock()
		return 0, errors.New("a join is already in progress")
	}
	if len(m.ports) == 0 {
		m.mu.Unlock()
		return 0, errors.New("no VC ports: set them first (request virtual-chassis vc-port set …)")
	}
	req := &joinReq{token: n, done: make(chan joinResult, 1)}
	m.join = req
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.join = nil
		m.mu.Unlock()
	}()
	m.wake() // sessions waiting on "other stack" retry at once, in join mode
	var res joinResult
	deadline := time.After(JoinTimeout)
	for {
		select {
		case res = <-req.done:
		case <-deadline:
			return 0, errors.New("no stack answered on the VC ports (cabled? token issued on the stack?)")
		}
		if res.err == nil || res.answer != nil {
			break
		}
		// A neighbour that is not the right stack: other ports may still answer.
		m.Log.Info("stack: join attempt failed", "err", res.err)
	}
	if res.err != nil {
		return 0, res.err
	}
	a := res.answer
	for name, b := range map[string][]byte{"stack.key": a.StackKey, "stack.crt": a.StackCert, "member.crt": a.MemberCert} {
		if err := hwio.WriteFile(m.path(name+".new"), b, 0o600); err != nil {
			return 0, err
		}
	}
	for _, name := range []string{"stack.key", "stack.crt", "member.crt"} {
		if err := hwio.Rename(m.path(name+".new"), m.path(name)); err != nil {
			return 0, err
		}
	}
	// The replicated state of the previous (own) stack is gone; the new
	// stack's master adds this member.
	for _, name := range []string{"raft", "control.json"} {
		hwio.RemoveAll(m.path(name))
	}
	if err := hwio.WriteFile(m.path(joinedFile), nil, 0o600); err != nil {
		return 0, err
	}
	m.Log.Warn("stack: joined virtual chassis", "facility", "change-log", "member", a.Member)
	if m.OnJoined != nil {
		if err := m.OnJoined(a.Member, a.Config); err != nil {
			return a.Member, err
		}
	}
	return a.Member, nil
}

// joinClient runs the joining side of a join exchange (docs/stack-protocol.md).
func (m *Manager) joinClient(l net.Conn, token string) joinResult {
	m.mu.Lock()
	key := m.memberKey
	m.mu.Unlock()
	self, err := pki.SelfSigned(key)
	if err != nil {
		return joinResult{err: err}
	}
	var stackPub ed25519.PublicKey
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pki.TLSCert(self, key)},
		// The stack is verified by the admit proof (only the token holder
		// can compute it), not by a certificate chain we do not have yet.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no stack certificate")
			}
			pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
			if !ok {
				return errors.New("stack key is not Ed25519")
			}
			stackPub = pub
			return nil
		}}
	conn := tls.Client(l, pki.Wire(cfg, pki.ALPNJoin))
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := conn.Handshake(); err != nil {
		return joinResult{err: fmt.Errorf("join handshake: %w", err)}
	}
	myPub := key.Public().(ed25519.PublicKey)
	req, _ := json.Marshal(map[string][]byte{"proof": pki.Proof(token, "join", myPub, stackPub)})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return joinResult{err: err}
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return joinResult{err: fmt.Errorf("join answer: %w", err)}
	}
	var a joinAnswer
	if err := json.Unmarshal(line, &a); err != nil {
		return joinResult{err: fmt.Errorf("join answer: %w", err)}
	}
	if a.Error != "" {
		return joinResult{err: fmt.Errorf("the stack refused: %s", a.Error)}
	}
	if !pki.CheckProof(a.Admit, pki.Proof(token, "admit", myPub, stackPub)) {
		return joinResult{err: errors.New("the answering switch does not know the token (not the stack that issued it)")}
	}
	sc, err := pki.DecodeCert(a.StackCert)
	if err != nil || !stackPub.Equal(sc.PublicKey) {
		return joinResult{err: errors.New("stack certificate does not match the handshake")}
	}
	mc, err := pki.DecodeCert(a.MemberCert)
	if err != nil || mc.CheckSignatureFrom(sc) != nil || !myPub.Equal(mc.PublicKey) {
		return joinResult{err: errors.New("member certificate invalid")}
	}
	if id, ok := pki.ParseMemberName(mc.Subject.CommonName); !ok || id != a.Member {
		return joinResult{err: errors.New("member certificate does not name the member id")}
	}
	if _, err := pki.DecodeKey(a.StackKey); err != nil {
		return joinResult{err: fmt.Errorf("stack key: %w", err)}
	}
	return joinResult{answer: &a}
}

// joinServer runs the stack side of a join exchange.
func (m *Manager) joinServer(l net.Conn, port string) {
	m.mu.Lock()
	st := m.stack
	m.mu.Unlock()
	var memberPub ed25519.PublicKey
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pki.TLSCert(st.Cert, st.Key)},
		ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no member certificate")
			}
			pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
			if !ok {
				return errors.New("member key is not Ed25519")
			}
			memberPub = pub
			return nil
		}}
	conn := tls.Server(l, pki.Wire(cfg, pki.ALPNJoin))
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := conn.Handshake(); err != nil {
		m.Log.Info("stack: join handshake failed", "port", port, "err", err)
		return
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req map[string][]byte
	if json.Unmarshal(line, &req) != nil {
		return
	}
	stackPub := st.Key.Public().(ed25519.PublicKey)
	answer := func(a joinAnswer) {
		b, _ := json.Marshal(a)
		conn.Write(append(b, '\n'))
	}
	var token string
	var pm pendingMember
	if m.Control != nil {
		for _, t := range m.Control.Tokens() {
			if time.Now().Before(t.Expires) && pki.CheckProof(req["proof"], pki.Proof(t.Token, "join", memberPub, stackPub)) {
				token, pm = t.Token, pendingMember{id: t.Member, expires: t.Expires}
			}
		}
		// Used up through the master, so a token admits one switch only.
		if token != "" {
			if err := m.Control.Admit(token, memberPub); err != nil {
				m.Log.Warn("stack: join refused", "facility", "authorization", "port", port, "err", err)
				answer(joinAnswer{Error: "the stack could not admit the member: " + err.Error()})
				return
			}
		}
	} else {
		m.mu.Lock()
		for t, p := range m.pending {
			if time.Now().Before(p.expires) && pki.CheckProof(req["proof"], pki.Proof(t, "join", memberPub, stackPub)) {
				token, pm = t, p
				delete(m.pending, t) // one-time
			}
		}
		m.mu.Unlock()
	}
	if token == "" {
		m.Log.Warn("stack: join refused: no valid token", "facility", "authorization", "port", port)
		answer(joinAnswer{Error: "invalid or expired token"})
		return
	}
	cert, err := st.SignMember(pm.id, memberPub)
	if err != nil {
		answer(joinAnswer{Error: "signing failed"})
		return
	}
	skey, _ := pki.EncodeKey(st.Key)
	var cfgJSON json.RawMessage
	if m.ActiveConfig != nil {
		cfgJSON = m.ActiveConfig()
	}
	answer(joinAnswer{Member: pm.id, MemberCert: pki.EncodeCert(cert), StackKey: skey, StackCert: pki.EncodeCert(st.Cert),
		Config: cfgJSON, Admit: pki.Proof(token, "admit", memberPub, stackPub)})
	m.Log.Warn("stack: member joined", "facility", "change-log", "member", pm.id, "port", port)
}
