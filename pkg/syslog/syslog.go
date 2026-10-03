// Package syslog collects switchd's log records into a local ring buffer
// ("show log") and forwards them to remote syslog servers (RFC 5424 over
// UDP, TCP or TLS; reference 5.1 system syslog).
package syslog

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Severities (RFC 5424).
const (
	Emergency = iota
	Alert
	Critical
	Error
	Warning
	Notice
	Info
	Debug
)

var severityNames = []string{"emergency", "alert", "critical", "error", "warning", "notice", "info", "debug"}

// facilityCodes maps configuration names to RFC 5424 facility numbers.
var facilityCodes = map[string]int{
	"kernel": 0, "daemon": 3, "authorization": 4, "change-log": 22, "interactive-commands": 23,
	"local0": 16, "local1": 17, "local2": 18, "local3": 19, "local4": 20, "local5": 21, "local6": 22, "local7": 23,
}

// FacilityAttr is the slog attribute key that selects a record's facility
// (default "daemon").
const FacilityAttr = "facility"

// Message is one log record.
type Message struct {
	Time     time.Time
	Facility string
	Severity int
	Text     string // message and attributes
	// Host is the host name of the member the message comes from when it
	// was relayed by another member ("" = this one).
	Host string
}

// Host is one remote server.
type Host struct {
	Host      string
	Port      int
	Transport string // udp, tcp, tls
	Facility  string // any or a facility name
	Severity  string // any or a severity name (this or more severe)
	CAFile    string
}

// Stats describe a forwarder for "show system syslog".
type Stats struct {
	Host      Host
	Connected bool
	Sent      uint64
	Dropped   uint64
	Queued    int
	LastError string
}

// Hub receives records and distributes them.
type Hub struct {
	next slog.Handler // e.g. stderr/journal

	mu       sync.Mutex
	ring     []Message
	ringPos  int
	ringFull bool
	fwds     map[Host]*forwarder
	hostName func() string
	// vrf is the device outgoing connections are bound to if it exists
	// (the management instance); see SetVRF.
	vrf string
	// relay, when set, receives every local message: a member that is not
	// the master hands its messages to the master (reference 1.8).
	relay func(Message)
}

// SetRelay sets (or clears, nil) the function that receives every local
// message besides the ring buffer and the forwarders.
func (h *Hub) SetRelay(f func(Message)) {
	h.mu.Lock()
	h.relay = f
	h.mu.Unlock()
}

// NewHub returns a hub that also passes every record to next.
func NewHub(next slog.Handler, bufSize int) *Hub {
	h := &Hub{next: next, fwds: map[Host]*forwarder{}, hostName: func() string { h, _ := os.Hostname(); return h }}
	h.ring = make([]Message, max(bufSize, 1))
	return h
}

// Configure sets the ring size and the forwarders. Unchanged forwarders keep
// their queue and connection.
// SetVRF sets the routing instance (VRF device) new connections to syslog
// servers use ("" = the default routing table).
func (h *Hub) SetVRF(vrf string) {
	h.mu.Lock()
	h.vrf = vrf
	h.mu.Unlock()
}

// VRF returns the VRF device of outgoing connections.
func (h *Hub) VRF() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.vrf
}

func (h *Hub) Configure(hosts []Host, hostName func() string, bufSize int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hostName != nil {
		h.hostName = hostName
	}
	if bufSize > 0 && bufSize != len(h.ring) {
		old := h.snapshotLocked()
		h.ring, h.ringPos, h.ringFull = make([]Message, bufSize), 0, false
		for _, m := range old[max(0, len(old)-bufSize):] {
			h.addLocked(m)
		}
	}
	want := map[Host]bool{}
	for _, c := range hosts {
		want[c] = true
		if h.fwds[c] == nil {
			f := newForwarder(c, h)
			h.fwds[c] = f
			go f.run()
		}
	}
	for c, f := range h.fwds {
		if !want[c] {
			f.stop()
			delete(h.fwds, c)
		}
	}
}

// Close stops all forwarders.
func (h *Hub) Close() { h.Configure(nil, nil, 0) }

func (h *Hub) addLocked(m Message) {
	h.ring[h.ringPos] = m
	h.ringPos = (h.ringPos + 1) % len(h.ring)
	if h.ringPos == 0 {
		h.ringFull = true
	}
}

func (h *Hub) snapshotLocked() []Message {
	if !h.ringFull {
		return slices.Clone(h.ring[:h.ringPos])
	}
	return append(slices.Clone(h.ring[h.ringPos:]), h.ring[:h.ringPos]...)
}

// Recent returns the buffered messages, oldest first.
func (h *Hub) Recent() []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshotLocked()
}

// Stats returns the state of every forwarder.
func (h *Hub) Stats() []Stats {
	h.mu.Lock()
	fs := make([]*forwarder, 0, len(h.fwds))
	for _, f := range h.fwds {
		fs = append(fs, f)
	}
	h.mu.Unlock()
	out := make([]Stats, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.stats())
	}
	slices.SortFunc(out, func(a, b Stats) int { return strings.Compare(a.Host.Host, b.Host.Host) })
	return out
}

// Log adds a message (used by the slog handler and for kernel messages).
func (h *Hub) Log(m Message) {
	h.mu.Lock()
	h.addLocked(m)
	fs := make([]*forwarder, 0, len(h.fwds))
	for _, f := range h.fwds {
		fs = append(fs, f)
	}
	relay := h.relay
	h.mu.Unlock()
	for _, f := range fs {
		f.offer(m)
	}
	if relay != nil && m.Host == "" {
		relay(m)
	}
}

// ---- slog.Handler ----

type handler struct {
	hub   *Hub
	next  slog.Handler
	attrs []slog.Attr
	group string
}

// Handler returns the slog handler feeding the hub.
func (h *Hub) Handler() slog.Handler { return &handler{hub: h, next: h.next} }

func (s *handler) Enabled(ctx context.Context, l slog.Level) bool { return s.next.Enabled(ctx, l) }

func (s *handler) WithAttrs(as []slog.Attr) slog.Handler {
	return &handler{hub: s.hub, next: s.next.WithAttrs(as), attrs: append(slices.Clone(s.attrs), as...), group: s.group}
}

func (s *handler) WithGroup(name string) slog.Handler {
	return &handler{hub: s.hub, next: s.next.WithGroup(name), attrs: s.attrs, group: name}
}

func severityOf(l slog.Level) int {
	switch {
	case l >= slog.LevelError:
		return Error
	case l >= slog.LevelWarn:
		return Warning
	case l >= slog.LevelInfo:
		return Info
	}
	return Debug
}

func (s *handler) Handle(ctx context.Context, r slog.Record) error {
	m := Message{Time: r.Time, Facility: "daemon", Severity: severityOf(r.Level)}
	var b strings.Builder
	b.WriteString(r.Message)
	add := func(a slog.Attr) bool {
		if a.Key == FacilityAttr {
			m.Facility = a.Value.String()
			return true
		}
		v := a.Value.Resolve().String()
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		if strings.ContainsAny(v, " \"=\n") || v == "" {
			v = strconv.Quote(v)
		}
		b.WriteString(v)
		return true
	}
	for _, a := range s.attrs {
		add(a)
	}
	r.Attrs(add)
	m.Text = b.String()
	s.hub.Log(m)
	return s.next.Handle(ctx, r)
}

// ---- forwarding ----

const queueLimit = 10000

type forwarder struct {
	cfg Host
	hub *Hub

	mu        sync.Mutex
	cond      *sync.Cond
	queue     []Message
	stopped   bool
	connected bool
	sent      uint64
	dropped   uint64
	lastErr   string
}

func newForwarder(c Host, hub *Hub) *forwarder {
	f := &forwarder{cfg: c, hub: hub}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// wants reports whether the message passes the facility/severity filters.
func (f *forwarder) wants(m Message) bool {
	if f.cfg.Facility != "" && f.cfg.Facility != "any" {
		want, ok := facilityCodes[f.cfg.Facility]
		if !ok || want != facilityCodes[m.Facility] {
			return false
		}
	}
	if f.cfg.Severity != "" && f.cfg.Severity != "any" {
		if i := slices.Index(severityNames, f.cfg.Severity); i >= 0 && m.Severity > i {
			return false
		}
	}
	return true
}

func (f *forwarder) offer(m Message) {
	if !f.wants(m) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return
	}
	if len(f.queue) >= queueLimit {
		f.queue = f.queue[1:] // the oldest message is dropped
		f.dropped++
	}
	f.queue = append(f.queue, m)
	f.cond.Signal()
}

func (f *forwarder) stop() {
	f.mu.Lock()
	f.stopped = true
	f.cond.Broadcast()
	f.mu.Unlock()
}

func (f *forwarder) stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Stats{Host: f.cfg, Connected: f.connected, Sent: f.sent, Dropped: f.dropped, Queued: len(f.queue), LastError: f.lastErr}
}

// Format renders m as RFC 5424. Octet-counted framing (RFC 6587) is added
// for stream transports by the caller.
func Format(m Message, host string) string {
	fac, ok := facilityCodes[m.Facility]
	if !ok {
		fac = 3
	}
	if host == "" {
		host = "-"
	}
	text := strings.ReplaceAll(m.Text, "\n", " ")
	return fmt.Sprintf("<%d>1 %s %s switchd %d - - %s", fac*8+m.Severity,
		m.Time.UTC().Format("2006-01-02T15:04:05.000000Z"), host, os.Getpid(), text)
}

func (f *forwarder) dial() (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	if vrf := f.hub.VRF(); vrf != "" {
		if _, err := net.InterfaceByName(vrf); err == nil {
			d.Control = func(_, _ string, c syscall.RawConn) error {
				var serr error
				err := c.Control(func(fd uintptr) { serr = syscall.BindToDevice(int(fd), vrf) })
				if err != nil {
					return err
				}
				return serr
			}
		}
	}
	addr := net.JoinHostPort(f.cfg.Host, strconv.Itoa(f.cfg.Port))
	switch f.cfg.Transport {
	case "udp":
		return d.Dial("udp", addr)
	case "tcp":
		return d.Dial("tcp", addr)
	case "tls":
		conf := &tls.Config{ServerName: f.cfg.Host, MinVersion: tls.VersionTLS12}
		if f.cfg.CAFile != "" {
			pem, err := os.ReadFile(f.cfg.CAFile)
			if err != nil {
				return nil, err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("%s: no certificates", f.cfg.CAFile)
			}
			conf.RootCAs = pool
		}
		return tls.DialWithDialer(&d, "tcp", addr, conf)
	}
	return nil, fmt.Errorf("unknown transport %q", f.cfg.Transport)
}

func (f *forwarder) run() {
	backoff := time.Second
	var conn net.Conn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	for {
		f.mu.Lock()
		for len(f.queue) == 0 && !f.stopped {
			f.cond.Wait()
		}
		if f.stopped {
			f.mu.Unlock()
			return
		}
		m := f.queue[0]
		f.mu.Unlock()

		if conn == nil {
			c, err := f.dial()
			if err != nil {
				f.fail(err)
				if f.cfg.Transport == "udp" {
					f.drop() // UDP has no queue guarantee
				}
				time.Sleep(backoff)
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			conn, backoff = c, time.Second
			f.mu.Lock()
			f.connected, f.lastErr = true, ""
			f.mu.Unlock()
		}
		host := m.Host
		if host == "" {
			host = f.hub.name()
		}
		line := Format(m, host)
		if f.cfg.Transport != "udp" {
			line = strconv.Itoa(len(line)) + " " + line
		}
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write([]byte(line)); err != nil {
			f.fail(err)
			conn.Close()
			conn = nil
			if f.cfg.Transport == "udp" {
				f.drop()
			}
			continue
		}
		f.mu.Lock()
		if len(f.queue) > 0 && f.queue[0].Time == m.Time && f.queue[0].Text == m.Text {
			f.queue = f.queue[1:]
		}
		f.sent++
		f.mu.Unlock()
	}
}

func (f *forwarder) fail(err error) {
	f.mu.Lock()
	f.connected, f.lastErr = false, err.Error()
	f.mu.Unlock()
}

func (f *forwarder) drop() {
	f.mu.Lock()
	if len(f.queue) > 0 {
		f.queue = f.queue[1:]
		f.dropped++
	}
	f.mu.Unlock()
}

func (h *Hub) name() string {
	h.mu.Lock()
	fn := h.hostName
	h.mu.Unlock()
	return fn()
}

// ErrNoHub is returned by show commands when logging is not wired up.
var ErrNoHub = errors.New("logging not available")
