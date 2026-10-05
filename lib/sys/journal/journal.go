// Package journal writes log records into the systemd journal through its
// native protocol, with the program name, severity and syslog facility as
// fields, so that a log reader (cer-syslogd) forwards them faithfully.
// Without a journal (a test, a development machine) records go to a
// fallback writer as text.
package journal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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

// DefaultSocket is the journal's native socket.
const DefaultSocket = "/run/systemd/journal/socket"

// FacilityAttr is the slog attribute key that selects a record's facility
// (reference 5.1 system syslog; default "daemon").
const FacilityAttr = "facility"

// Facilities are the syslog facilities by name, as configured (reference
// 5.1): Junos names map to the standard numbers.
var Facilities = map[string]int{
	"kernel": 0, "kern": 0, "user": 1, "mail": 2, "daemon": 3, "authorization": 4, "auth": 4, "syslog": 5,
	"lpr": 6, "news": 7, "uucp": 8, "cron": 9, "authpriv": 10, "ftp": 11,
	"local0": 16, "local1": 17, "local2": 18, "local3": 19, "local4": 20, "local5": 21,
	"change-log": 22, "local6": 22, "interactive-commands": 23, "local7": 23,
}

// FacilityName returns the configuration name of a facility number.
func FacilityName(n int) string {
	switch n {
	case 0:
		return "kernel"
	case 3:
		return "daemon"
	case 4, 10:
		return "authorization"
	case 22:
		return "change-log"
	case 23:
		return "interactive-commands"
	}
	for name, v := range Facilities {
		if v == n && !strings.HasPrefix(name, "local") {
			return name
		}
	}
	return "local" + strconv.Itoa(n-16)
}

// Severity of a slog level (syslog numbers: 3 error, 4 warning, 6 info,
// 7 debug). Levels above error are critical (2).
func Severity(l slog.Level) int {
	switch {
	case l > slog.LevelError:
		return 2
	case l >= slog.LevelError:
		return 3
	case l >= slog.LevelWarn:
		return 4
	case l >= slog.LevelInfo:
		return 6
	}
	return 7
}

// Options configure a handler.
type Options struct {
	// Identifier is the program name (SYSLOG_IDENTIFIER).
	Identifier string
	Level      slog.Leveler
	// Fields are added to every record (e.g. CEROS_MEMBER).
	Fields map[string]string
	// Socket overrides DefaultSocket (tests).
	Socket string
	// Fallback receives the records as text when there is no journal
	// (default stderr).
	Fallback io.Writer
}

type sink struct {
	mu       sync.Mutex
	conn     *net.UnixConn
	addr     *net.UnixAddr
	fallback slog.Handler
	ok       bool
}

// Handler writes records to the journal.
type Handler struct {
	o     Options
	s     *sink
	attrs []slog.Attr
	group string
}

// NewHandler returns a handler; it uses the journal when its socket
// exists, the fallback writer otherwise.
func NewHandler(o Options) *Handler {
	if o.Socket == "" {
		o.Socket = DefaultSocket
	}
	if o.Fallback == nil {
		o.Fallback = os.Stderr
	}
	s := &sink{addr: &net.UnixAddr{Name: o.Socket, Net: "unixgram"},
		fallback: slog.NewTextHandler(o.Fallback, &slog.HandlerOptions{Level: o.Level})}
	if c, err := net.DialUnix("unixgram", nil, s.addr); err == nil {
		s.conn, s.ok = c, true
	}
	return &Handler{o: o, s: s}
}

// Journal reports whether records go to the journal.
func (h *Handler) Journal() bool { return h.s.ok }

func (h *Handler) Enabled(_ context.Context, l slog.Level) bool {
	min := slog.LevelInfo
	if h.o.Level != nil {
		min = h.o.Level.Level()
	}
	return l >= min
}

func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(slices.Clone(h.attrs), as...)
	return &c
}

func (h *Handler) WithGroup(name string) slog.Handler {
	c := *h
	c.group = name
	return &c
}

// Text formats a record's message and attributes as one line (key=value,
// quoted where needed) and returns its facility.
func Text(msg string, attrs []slog.Attr) (text, facility string) {
	facility = "daemon"
	var b strings.Builder
	b.WriteString(msg)
	for _, a := range attrs {
		if a.Key == FacilityAttr {
			facility = a.Value.String()
			continue
		}
		v := a.Value.Resolve().String()
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		if strings.ContainsAny(v, " \"=\n") || v == "" {
			v = strconv.Quote(v)
		}
		b.WriteString(v)
	}
	return b.String(), facility
}

// maxMessage bounds MESSAGE (a datagram's limit is about 200 KB; larger
// records would need file descriptor passing).
const maxMessage = 48 << 10

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if !h.s.ok {
		return h.s.fallback.WithAttrs(h.attrs).Handle(ctx, r)
	}
	attrs := slices.Clone(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		if h.group != "" && a.Key != FacilityAttr {
			a.Key = h.group + "." + a.Key
		}
		attrs = append(attrs, a)
		return true
	})
	text, facility := Text(r.Message, attrs)
	if len(text) > maxMessage {
		text = text[:maxMessage] + " …(truncated)"
	}
	fac, ok := Facilities[facility]
	if !ok {
		fac, facility = 3, "daemon"
	}
	var b bytes.Buffer
	field(&b, "MESSAGE", text)
	field(&b, "PRIORITY", strconv.Itoa(Severity(r.Level)))
	field(&b, "SYSLOG_FACILITY", strconv.Itoa(fac))
	field(&b, "CEROS_FACILITY", facility)
	if h.o.Identifier != "" {
		field(&b, "SYSLOG_IDENTIFIER", h.o.Identifier)
	}
	if !r.Time.IsZero() {
		field(&b, "SYSLOG_TIMESTAMP", r.Time.UTC().Format(time.RFC3339Nano))
	}
	keys := make([]string, 0, len(h.o.Fields))
	for k := range h.o.Fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		field(&b, k, h.o.Fields[k])
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	_, err := h.s.conn.Write(b.Bytes())
	if err != nil && !errors.Is(err, syscall.EAGAIN) {
		// The journal restarted: connect to its new socket once.
		if c, derr := net.DialUnix("unixgram", nil, h.s.addr); derr == nil {
			h.s.conn.Close()
			h.s.conn = c
			_, err = c.Write(b.Bytes())
		}
	}
	if err != nil && !errors.Is(err, syscall.EAGAIN) {
		// The journal is gone (restarting): this record goes to the
		// fallback rather than nowhere.
		return h.s.fallback.WithAttrs(h.attrs).Handle(ctx, r)
	}
	return nil
}

// field appends one field in the native protocol: NAME=value, or for a
// value with a newline NAME, its length (64-bit little endian) and the
// value.
func field(b *bytes.Buffer, name, value string) {
	if !strings.Contains(value, "\n") {
		fmt.Fprintf(b, "%s=%s\n", name, value)
		return
	}
	b.WriteString(name)
	b.WriteByte('\n')
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(value)))
	b.Write(n[:])
	b.WriteString(value)
	b.WriteByte('\n')
}

// Parse decodes a datagram of the native protocol (tests, and readers of
// a captured stream).
func Parse(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for len(b) > 0 {
		nl := bytes.IndexByte(b, '\n')
		if nl < 0 {
			return nil, errors.New("journal: no newline")
		}
		line := b[:nl]
		if eq := bytes.IndexByte(line, '='); eq >= 0 {
			out[string(line[:eq])] = string(line[eq+1:])
			b = b[nl+1:]
			continue
		}
		rest := b[nl+1:]
		if len(rest) < 8 {
			return nil, errors.New("journal: short binary field")
		}
		n := binary.LittleEndian.Uint64(rest)
		if uint64(len(rest)-8) < n+1 {
			return nil, errors.New("journal: short binary field")
		}
		out[string(line)] = string(rest[8 : 8+n])
		b = rest[8+n+1:]
	}
	return out, nil
}
