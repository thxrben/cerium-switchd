// Package stack runs this switch's part of the virtual chassis (reference
// 5.2): its keys, its stacking ports (VC ports) and the authenticated
// sessions to the neighbours on them.
package stack

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"

	"mclag/internal/config"
	"mclag/internal/dataplane"
	"mclag/internal/stack/control"
	"mclag/internal/stack/link"
	"mclag/internal/stack/mesh"
	"mclag/internal/stack/pki"
)

// Manager owns the stack identity and the VC ports of this switch.
type Manager struct {
	Dir string // state directory for keys and VC ports
	Log *slog.Logger
	// Linux maps a local port "<card>/<port>" to its kernel name.
	Linux func(local string) (string, bool)
	// HostName is this member's host name (shown to neighbours).
	HostName func() string
	// ActiveConfig returns the active configuration (JSON) for a joining
	// member.
	ActiveConfig func() json.RawMessage
	// OnJoined is called after this switch joined another stack: member id
	// and the stack's configuration. switchd then restarts.
	OnJoined func(member int, config json.RawMessage) error
	// Control is the replicated stack state (member list, join tokens); set
	// before Start. nil: tokens are kept locally and every member
	// certificate of the stack is accepted.
	Control *control.Node

	mu         sync.Mutex
	stack      *pki.Stack
	member     int
	memberKey  ed25519.PrivateKey
	memberCert *x509.Certificate
	ports      map[string]*vcPort
	ctx        context.Context
	join       *joinReq
	pending    map[string]pendingMember // normalized token -> member
	kick       chan struct{}            // wakes waiting sessions (join started, member added)
	mesh       *mesh.Mesh
}

type joinReq struct {
	token string
	done  chan joinResult
}

type pendingMember struct {
	id      int
	expires time.Time
}

// joinAnswer is what the stack sends a joining switch.
type joinAnswer struct {
	Error      string          `json:"error,omitempty"`
	Member     int             `json:"member,omitempty"`
	MemberCert []byte          `json:"member_cert,omitempty"`
	StackKey   []byte          `json:"stack_key,omitempty"`
	StackCert  []byte          `json:"stack_cert,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"`
	Admit      []byte          `json:"admit,omitempty"`
}

type joinResult struct {
	answer *joinAnswer
	err    error
}

// PortStatus is one line of "show virtual-chassis vc-port".
type PortStatus struct {
	Port      string // "<card>/<port>"
	Linux     string
	State     string // up, down, absent
	Neighbor  string // "member 2 (sw2)", "other stack", "-"
	PeerPort  string
	UpSince   time.Time
	LastError string
	// The neighbour of an up member session (for the stack tunnels'
	// underlay, reference 5.2): its member id and its port's MAC address.
	NeighborID  int
	NeighborMAC net.HardwareAddr
	// PathMTU is the largest frame (Ethernet payload) the cable carried in
	// the last probe round (0: not known yet; docs/stack-protocol.md).
	PathMTU int
}

// StackLink is a stacking link with an up member session.
type StackLink struct {
	Linux       string
	Neighbor    int
	NeighborMAC net.HardwareAddr
}

// Links returns the stacking links whose member sessions are up.
func (m *Manager) Links() []StackLink {
	var out []StackLink
	for _, p := range m.Ports() {
		if p.State == "up" && p.NeighborID > 0 && p.Linux != "" && len(p.NeighborMAC) == 6 {
			out = append(out, StackLink{Linux: p.Linux, Neighbor: p.NeighborID, NeighborMAC: p.NeighborMAC})
		}
	}
	return out
}

type vcPort struct {
	local  string
	cancel context.CancelFunc
	mu     sync.Mutex
	st     PortStatus
}

type vcState struct {
	Ports []string `json:"ports"`
}

// errJoinDone ends a session that carried a join exchange.
var errJoinDone = errors.New("join exchange done")

// kicked returns a channel closed on the next kick.
func (m *Manager) kicked() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kick
}

// wake wakes all sessions waiting on another stack.
func (m *Manager) wake() {
	m.mu.Lock()
	close(m.kick)
	m.kick = make(chan struct{})
	m.mu.Unlock()
}

// errOtherStack: the neighbour's certificate is not from this stack.
var errOtherStack = errors.New("neighbour belongs to another stack (not joined)")

// hello is exchanged once a stacking session is authenticated.
type hello struct {
	Member int    `json:"member"`
	Host   string `json:"host"`
	Port   string `json:"port"`
}

func (m *Manager) path(n string) string { return filepath.Join(m.Dir, n) }

// Load reads (or, on first start, creates) the keys; afterwards Member is
// known.
func (m *Manager) Load() error {
	if m.Log == nil {
		m.Log = slog.Default()
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	if err := m.loadKeys(); err != nil {
		return err
	}
	m.mu.Lock()
	m.mesh = mesh.New(m.member, m.Log)
	m.mu.Unlock()
	return nil
}

// joinedFile marks a switch that joined a stack (it never starts a new
// stack control cluster by itself).
const joinedFile = "joined"

// Founder reports whether this switch created its stack (at first start,
// or after it was removed from another stack) and never joined one since
// (docs/stack-protocol.md, bootstrap).
func (m *Manager) Founder() bool {
	_, err := os.Stat(m.path(joinedFile))
	return errors.Is(err, os.ErrNotExist)
}

// PublicKey is this member's public key.
func (m *Manager) PublicKey() ed25519.PublicKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.memberKey.Public().(ed25519.PublicKey)
}

// allowed checks a member against the replicated member list. A member
// that has not received the list yet accepts every member of the stack.
func (m *Manager) allowed(id int, pub ed25519.PublicKey) bool {
	if m.Control == nil {
		return true
	}
	members := m.Control.Members()
	if len(members) == 0 {
		return true
	}
	mi, ok := members[id]
	return ok && pub.Equal(ed25519.PublicKey(mi.Key))
}

// Start starts the VC ports (after Load).
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.pending = map[string]pendingMember{}
	m.kick = make(chan struct{})
	m.ctx = ctx
	m.ports = map[string]*vcPort{}
	m.mu.Unlock()
	go m.mesh.Run(ctx.Done())
	var st vcState
	if raw, err := os.ReadFile(m.path("vc-ports.json")); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	for _, p := range st.Ports {
		m.startPort(p)
	}
	return nil
}

// loadKeys reads the stack and member keys, creating a new one-member stack
// on first start.
func (m *Manager) loadKeys() error {
	read := func(n string) []byte {
		b, _ := os.ReadFile(m.path(n))
		return b
	}
	sk, sc, mk, mc := read("stack.key"), read("stack.crt"), read("member.key"), read("member.crt")
	if sk == nil || sc == nil || mk == nil || mc == nil {
		if err := m.newStack(1); err != nil {
			return err
		}
		sk, sc, mk, mc = read("stack.key"), read("stack.crt"), read("member.key"), read("member.crt")
	}
	skey, err := pki.DecodeKey(sk)
	if err != nil {
		return fmt.Errorf("stack key: %w", err)
	}
	scert, err := pki.DecodeCert(sc)
	if err != nil {
		return fmt.Errorf("stack certificate: %w", err)
	}
	mkey, err := pki.DecodeKey(mk)
	if err != nil {
		return fmt.Errorf("member key: %w", err)
	}
	mcert, err := pki.DecodeCert(mc)
	if err != nil {
		return fmt.Errorf("member certificate: %w", err)
	}
	id, ok := pki.ParseMemberName(mcert.Subject.CommonName)
	if !ok {
		return fmt.Errorf("member certificate subject %q", mcert.Subject.CommonName)
	}
	m.mu.Lock()
	m.stack = &pki.Stack{Key: skey, Cert: scert}
	m.member, m.memberKey, m.memberCert = id, mkey, mcert
	m.mu.Unlock()
	return nil
}

// newStack creates the keys of a new stack with this switch as member id
// (new member key too).
func (m *Manager) newStack(id int) error {
	s, err := pki.NewStack()
	if err != nil {
		return err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	cert, err := s.SignMember(id, key.Public().(ed25519.PublicKey))
	if err != nil {
		return err
	}
	skb, _ := pki.EncodeKey(s.Key)
	mkb, _ := pki.EncodeKey(key)
	files := map[string][]byte{"stack.key": skb, "stack.crt": pki.EncodeCert(s.Cert), "member.key": mkb, "member.crt": pki.EncodeCert(cert)}
	for n, b := range files {
		if err := os.WriteFile(m.path(n+".new"), b, 0o600); err != nil {
			return err
		}
	}
	for n := range files {
		if err := os.Rename(m.path(n+".new"), m.path(n)); err != nil {
			return err
		}
	}
	m.Log.Info("stack: new one-member stack created", "stack", s.Cert.Subject.CommonName, "member", id)
	return nil
}

// Leave makes this switch the only member of a new stack after it was
// removed from its stack: new keys, the same member id, no replicated
// state. switchd has to restart afterwards.
func (m *Manager) Leave() error {
	if err := m.newStack(m.Member()); err != nil {
		return err
	}
	for _, name := range []string{"raft", "control.json", joinedFile} {
		if err := os.RemoveAll(m.path(name)); err != nil {
			return err
		}
	}
	m.Log.Warn("stack: removed from the virtual chassis; this switch is now a stack of its own", "facility", "change-log", "member", m.Member())
	return nil
}

// Member returns this switch's member id.
func (m *Manager) Member() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.member
}

// StackID names the stack (the stack certificate's subject).
func (m *Manager) StackID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stack == nil {
		return ""
	}
	return strings.TrimPrefix(m.stack.Cert.Subject.CommonName, "mclag stack ")
}

func (m *Manager) save() error {
	var st vcState
	for p := range m.ports {
		st.Ports = append(st.Ports, p)
	}
	sort.Slice(st.Ports, func(i, j int) bool { return config.NaturalLess(st.Ports[i], st.Ports[j]) })
	raw, _ := json.Marshal(st)
	tmp := m.path("vc-ports.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path("vc-ports.json"))
}

// SetPort designates (add) or releases a VC port "<card>/<port>".
func (m *Manager) SetPort(local string, add bool) error {
	m.mu.Lock()
	_, exists := m.ports[local]
	m.mu.Unlock()
	switch {
	case add && exists:
		return fmt.Errorf("%s is already a VC port", local)
	case !add && !exists:
		return fmt.Errorf("%s is not a VC port", local)
	case add:
		m.startPort(local)
	default:
		m.mu.Lock()
		p := m.ports[local]
		delete(m.ports, local)
		m.mu.Unlock()
		p.cancel()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.save()
}

// IsPort reports whether a kernel interface is a VC port.
func (m *Manager) IsPort(linux string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.ports {
		if l, ok := m.Linux(p.local); ok && l == linux {
			return true
		}
	}
	return false
}

// Ports returns the VC ports and their neighbours.
func (m *Manager) Ports() []PortStatus {
	m.mu.Lock()
	ps := make([]*vcPort, 0, len(m.ports))
	for _, p := range m.ports {
		ps = append(ps, p)
	}
	m.mu.Unlock()
	out := make([]PortStatus, 0, len(ps))
	for _, p := range ps {
		p.mu.Lock()
		out = append(out, p.st)
		p.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return config.NaturalLess(out[i].Port, out[j].Port) })
	return out
}

func (m *Manager) startPort(local string) {
	m.mu.Lock()
	ctx, cancel := context.WithCancel(m.ctx)
	p := &vcPort{local: local, cancel: cancel, st: PortStatus{Port: local, State: "down", Neighbor: "-"}}
	m.ports[local] = p
	m.mu.Unlock()
	go m.runPort(ctx, p)
}

func (p *vcPort) set(f func(s *PortStatus)) {
	p.mu.Lock()
	f(&p.st)
	p.mu.Unlock()
}

// runPort keeps a stacking session on one port for as long as the port is
// a VC port.
func (m *Manager) runPort(ctx context.Context, p *vcPort) {
	for ctx.Err() == nil {
		linux, ok := m.Linux(p.local)
		if !ok {
			p.set(func(s *PortStatus) { s.State, s.Linux, s.Neighbor = "absent", "", "-" })
			sleep(ctx, time.Second)
			continue
		}
		p.set(func(s *PortStatus) { s.Linux = linux })
		if err := preparePort(linux); err != nil {
			p.set(func(s *PortStatus) { s.State, s.LastError = "down", err.Error() })
			sleep(ctx, time.Second)
			continue
		}
		pio, err := link.OpenPacket(linux)
		if err != nil {
			p.set(func(s *PortStatus) { s.State, s.LastError = "down", err.Error() })
			sleep(ctx, time.Second)
			continue
		}
		m.sessions(ctx, p, pio, linux)
		pio.Close()
	}
	p.set(func(s *PortStatus) { s.State, s.Neighbor, s.NeighborID, s.NeighborMAC = "down", "-", 0, nil })
}

// sessions runs link + TLS sessions on an open port until ctx ends or the
// port disappears.
func (m *Manager) sessions(ctx context.Context, p *vcPort, pio *link.PacketIO, linux string) {
	for ctx.Err() == nil {
		if _, err := net.InterfaceByName(linux); err != nil {
			return // the port went away (unplugged NIC)
		}
		l := link.New(pio, link.Options{Name: p.local})
		select {
		case <-ctx.Done():
			l.Close()
			return
		case <-l.Done():
			continue
		case <-l.Up():
		}
		err := m.session(ctx, p, l, pio, linux)
		if errors.Is(err, errJoinDone) {
			l.Close()
			sleep(ctx, time.Second)
			continue
		}
		if errors.Is(err, errOtherStack) {
			// The cable works, the neighbour belongs to another stack: keep
			// showing it and try again now and then (it may join us).
			p.set(func(s *PortStatus) {
				s.PeerPort, s.UpSince, s.LastError = "", time.Time{}, err.Error()
				s.NeighborID, s.NeighborMAC = 0, nil
			})
			select {
			case <-ctx.Done():
			case <-l.Done():
			case <-m.kicked():
			case <-time.After(10 * time.Second):
			}
			l.Close()
			continue
		}
		l.Close()
		p.set(func(s *PortStatus) {
			s.State, s.Neighbor, s.PeerPort, s.UpSince = "down", "-", "", time.Time{}
			s.NeighborID, s.NeighborMAC, s.PathMTU = 0, nil, 0
			if err != nil {
				s.LastError = err.Error()
			}
		})
		if err != nil && ctx.Err() == nil {
			m.Log.Info("stack: session on VC port ended", "port", p.local, "err", err)
			sleep(ctx, 2*time.Second)
		}
	}
}

// session authenticates the neighbour on an established link and keeps
// the session until it ends.
func (m *Manager) session(ctx context.Context, p *vcPort, l *link.Link, pio *link.PacketIO, linux string) error {
	m.mu.Lock()
	cfg := pki.Wire(pki.TLSConfig(m.stack.Cert, pki.TLSCert(m.memberCert, m.memberKey), m.allowed), pki.ALPNMember)
	me := m.member
	join := m.join
	m.mu.Unlock()
	own, err := net.InterfaceByName(linux)
	if err != nil {
		return err
	}
	// Session mode: M (member) or J (joining with a token).
	mode := []byte{'M'}
	if join != nil {
		mode[0] = 'J'
	}
	l.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := l.Write(mode); err != nil {
		return err
	}
	var peer [1]byte
	if _, err := io.ReadFull(l, peer[:]); err != nil {
		return err
	}
	l.SetDeadline(time.Time{})
	switch {
	case mode[0] == 'J' && peer[0] == 'M':
		res := m.joinClient(l, join.token)
		select {
		case join.done <- res:
		default:
		}
		return errJoinDone
	case mode[0] == 'M' && peer[0] == 'J':
		m.joinServer(l, p.local)
		return errJoinDone
	case peer[0] != 'M':
		return fmt.Errorf("neighbour session mode %q", peer[0])
	}
	// Roles by MAC address: the lower one is the TLS client.
	var conn *tls.Conn
	if bytes.Compare(own.HardwareAddr, pio.Peer()) < 0 {
		conn = tls.Client(l, cfg)
	} else {
		conn = tls.Server(l, cfg)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := conn.Handshake(); err != nil {
		m.Log.Info("stack: TLS handshake with the neighbour failed", "port", p.local, "tls_client", bytes.Compare(own.HardwareAddr, pio.Peer()) < 0,
			"own_mac", own.HardwareAddr.String(), "peer_mac", pio.Peer().String(), "err", err)
		p.set(func(s *PortStatus) { s.State, s.Neighbor = "up", "other stack" })
		return fmt.Errorf("%w: %v", errOtherStack, err)
	}
	host := ""
	if m.HostName != nil {
		host = m.HostName()
	}
	out, _ := json.Marshal(hello{Member: me, Host: host, Port: p.local})
	if _, err := conn.Write(append(out, '\n')); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		// With TLS 1.3 a rejected certificate shows up here (the peer's
		// alert), not in the handshake.
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "remote error" {
			p.set(func(s *PortStatus) { s.State, s.Neighbor = "up", "other stack" })
			return fmt.Errorf("%w: %v", errOtherStack, err)
		}
		return err
	}
	var h hello
	if err := json.Unmarshal(line, &h); err != nil {
		return fmt.Errorf("neighbour hello: %w", err)
	}
	if h.Member == me {
		return fmt.Errorf("neighbour has this switch's member id %d", me)
	}
	conn.SetDeadline(time.Time{})
	p.set(func(s *PortStatus) {
		s.State, s.PeerPort, s.UpSince, s.LastError = "up", h.Port, time.Now(), ""
		s.NeighborID, s.NeighborMAC = h.Member, slices.Clone(pio.Peer())
		s.Neighbor = fmt.Sprintf("member %d", h.Member)
		if h.Host != "" {
			s.Neighbor += " (" + h.Host + ")"
		}
	})
	m.Log.Info("stack: neighbour on VC port", "port", p.local, "member", h.Member, "host", h.Host, "peer_port", h.Port)
	var peerPub ed25519.PublicKey
	if pc := conn.ConnectionState().PeerCertificates; len(pc) > 0 {
		peerPub, _ = pc[0].PublicKey.(ed25519.PublicKey)
		if id, _ := pki.ParseMemberName(pc[0].Subject.CommonName); id != h.Member {
			return fmt.Errorf("neighbour says member %d, its certificate is %s", h.Member, pc[0].Subject.CommonName)
		}
	}
	// The session carries the mesh (topology, relay, streams) from now on.
	ended := make(chan struct{})
	go func() {
		m.mesh.AddPeer(h.Member, sessionConn{r, conn})
		close(ended)
	}()
	// A member removed from the member list loses its sessions.
	check := time.NewTicker(2 * time.Second)
	defer check.Stop()
	probe := time.NewTicker(500 * time.Millisecond)
	defer probe.Stop()
	for {
		select {
		case <-probe.C:
			if n := l.PathMTU(); n != 0 {
				p.set(func(s *PortStatus) { s.PathMTU = n })
			}
		case <-ctx.Done():
			conn.Close()
			<-ended
			return nil
		case <-l.Done():
			conn.Close()
			<-ended
			return l.Err()
		case <-ended:
			conn.Close()
			return errors.New("stack session closed")
		case <-check.C:
			if !m.allowed(h.Member, peerPub) {
				conn.Close()
				<-ended
				return fmt.Errorf("%w: member %d was removed from the stack", errOtherStack, h.Member)
			}
		}
	}
}

// sessionConn reads through the buffer that read the hello.
type sessionConn struct {
	r *bufio.Reader
	*tls.Conn
}

func (c sessionConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Mesh returns the stack's message layer (after Start).
func (m *Manager) Mesh() *mesh.Mesh {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mesh
}

// preparePort makes a port usable for stacking and nothing else: up, out
// of any bridge or bond, no addresses, IPv6 off.
func preparePort(linux string) error {
	ln, err := netlink.LinkByName(linux)
	if err != nil {
		return err
	}
	// The only master a stacking port has is the hidden instance of the
	// stack tunnels (reference 5.2). Joining it restarts the port once, so
	// it happens before the stacking session starts.
	if err := dataplane.EnsureStackPort(linux); err != nil {
		return err
	}
	p := "/proc/sys/net/ipv6/conf/" + linux + "/disable_ipv6"
	if raw, err := os.ReadFile(p); err == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("1")) {
		if err := os.WriteFile(p, []byte("1"), 0o644); err != nil {
			return err
		}
	}
	addrs, _ := netlink.AddrList(ln, netlink.FAMILY_ALL)
	for _, a := range addrs {
		_ = netlink.AddrDel(ln, &a)
	}
	// The largest frames the NIC can carry, once: the stack tunnels need
	// room for the largest data frame plus 58 bytes, and a later MTU change
	// could restart the link (reference 5.2, stack MTU).
	if want := dataplane.StackPortMTU(linux); want > ln.Attrs().MTU {
		if err := netlink.LinkSetMTU(ln, want); err != nil {
			return fmt.Errorf("setting the MTU to %d: %w", want, err)
		}
	}
	if ln.Attrs().Flags&net.FlagUp == 0 {
		return netlink.LinkSetUp(ln)
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ParseLocalPort validates "<card>/<port>" (as given by pic-slot and port).
func ParseLocalPort(card, port int) (string, error) {
	if card < 0 || card > 99 || port < 0 || port > 999 {
		return "", errors.New("pic-slot 0-99, port 0-999")
	}
	return fmt.Sprintf("%d/%d", card, port), nil
}
