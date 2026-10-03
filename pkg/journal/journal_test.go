package journal

import (
	"bytes"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHandlerWritesNativeProtocol(t *testing.T) {
	dir, err := os.MkdirTemp("", "journal")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "socket")
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	h := NewHandler(Options{Identifier: "cer-lacpd", Socket: path, Fields: map[string]string{"CEROS_MEMBER": "2"}})
	if !h.Journal() {
		t.Fatal("journal socket not used")
	}
	log := slog.New(h).With("bundle", "ae1")
	log.Warn("port left the bundle", "port", "1/0/3", "reason", "partner\nexpired")
	log.Info("commit", "facility", "change-log", "user", "thorben")

	read := func() map[string]string {
		buf := make([]byte, 65536)
		l.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, err := l.ReadFromUnix(buf)
		if err != nil {
			t.Fatal(err)
		}
		f, err := Parse(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	f := read()
	want := map[string]string{
		"MESSAGE":           `port left the bundle bundle=ae1 port=1/0/3 reason="partner\nexpired"`,
		"PRIORITY":          "4",
		"SYSLOG_FACILITY":   "3",
		"CEROS_FACILITY":    "daemon",
		"SYSLOG_IDENTIFIER": "cer-lacpd",
		"CEROS_MEMBER":      "2",
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("%s = %q, want %q", k, f[k], v)
		}
	}
	f = read()
	if f["SYSLOG_FACILITY"] != "22" || f["CEROS_FACILITY"] != "change-log" || f["PRIORITY"] != "6" || strings.Contains(f["MESSAGE"], "facility") {
		t.Errorf("facility: %v", f)
	}
}

func TestBinaryField(t *testing.T) {
	var b bytes.Buffer
	field(&b, "MESSAGE", "two\nlines")
	field(&b, "PRIORITY", "6")
	f, err := Parse(b.Bytes())
	if err != nil || f["MESSAGE"] != "two\nlines" || f["PRIORITY"] != "6" {
		t.Fatalf("%v %v", f, err)
	}
}

func TestFallbackWithoutJournal(t *testing.T) {
	var out bytes.Buffer
	h := NewHandler(Options{Identifier: "x", Socket: "/nonexistent/socket", Fallback: &out})
	if h.Journal() {
		t.Fatal("journal without socket")
	}
	slog.New(h).Info("hello", "k", "v")
	if !strings.Contains(out.String(), "msg=hello k=v") {
		t.Fatalf("fallback: %q", out.String())
	}
}

func TestFacilityNames(t *testing.T) {
	for name, n := range map[string]int{"kernel": 0, "daemon": 3, "authorization": 4, "change-log": 22, "interactive-commands": 23, "local3": 19} {
		if Facilities[name] != n || FacilityName(n) != name {
			t.Errorf("%s: %d %s", name, Facilities[name], FacilityName(n))
		}
	}
}
