package webapi

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeSoftware struct {
	got      []byte
	user     string
	install  *InstallRequest
	conflict bool
}

func (f *fakeSoftware) Status() (any, error) { return map[string]string{"version": "1.0"}, nil }

func (f *fakeSoftware) Upload(body io.Reader, size, limit int64, user string) (any, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(b, []byte("BUNDLE")) {
		return nil, fmt.Errorf("%w: not a cerOS bundle", ErrInvalid)
	}
	f.got, f.user = b, user
	return map[string]any{"version": "1.1", "size": len(b)}, nil
}

func (f *fakeSoftware) Install(req InstallRequest, user string) error {
	if f.conflict {
		return fmt.Errorf("%w: an update is running", ErrConflict)
	}
	f.install = &req
	return nil
}

func start(t *testing.T, limit int64) (*Server, *fakeSoftware, string, *http.Client) {
	t.Helper()
	sw := &fakeSoftware{}
	s := &Server{Log: slog.New(slog.DiscardHandler), Software: sw,
		Auth: func(u, p string) (User, bool) {
			switch {
			case u == "admin" && p == "secret":
				return User{Name: u, SuperUser: true}, true
			case u == "op" && p == "pw":
				return User{Name: u}, true
			}
			return User{}, false
		}}
	// A free port on the loopback.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	s.Sync(&Config{Port: port, Addr: "127.0.0.1", HostName: "sw1", Names: []net.IP{net.ParseIP("127.0.0.1")}, UploadLimit: limit})
	t.Cleanup(func() { s.Sync(nil) })
	info := s.Info()
	if !info.Running || !strings.HasPrefix(info.Pin, "sha256//") || info.Certificate != "temporary self-signed" {
		t.Fatalf("info %+v", info)
	}
	// The client trusts exactly the server's certificate (as a pinned
	// client would).
	var pin string
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			info := fingerprint(tls.Certificate{Certificate: raw}, Info{})
			pin = info.Pin
			if pin != s.Info().Pin {
				return fmt.Errorf("pin %s", pin)
			}
			return nil
		}}}}
	return s, sw, fmt.Sprintf("https://127.0.0.1:%d", port), client
}

func do(t *testing.T, c *http.Client, method, url, user, pw string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, pw)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestAPI(t *testing.T) {
	_, sw, base, c := start(t, 64)
	if code, _ := do(t, c, "GET", base+"/api/v1/software", "", "", nil); code != 401 {
		t.Fatalf("no credentials: %d", code)
	}
	if code, m := do(t, c, "GET", base+"/api/v1/software", "op", "pw", nil); code != 200 || m["version"] != "1.0" {
		t.Fatalf("status: %d %v", code, m)
	}
	if code, _ := do(t, c, "PUT", base+"/api/v1/software/upload", "op", "pw", []byte("BUNDLE")); code != 403 {
		t.Fatalf("operator upload: %d", code)
	}
	if code, m := do(t, c, "PUT", base+"/api/v1/software/upload", "admin", "secret", []byte("BUNDLE1")); code != 200 || m["version"] != "1.1" || sw.user != "admin" {
		t.Fatalf("upload: %d %v", code, m)
	}
	if code, m := do(t, c, "PUT", base+"/api/v1/software/upload", "admin", "secret", []byte("garbage")); code != 422 || !strings.Contains(fmt.Sprint(m["error"]), "not a cerOS bundle") {
		t.Fatalf("bad bundle: %d %v", code, m)
	}
	if code, _ := do(t, c, "PUT", base+"/api/v1/software/upload", "admin", "secret", bytes.Repeat([]byte("B"), 65)); code != 413 {
		t.Fatalf("too large: %d", code)
	}
	if code, _ := do(t, c, "POST", base+"/api/v1/software/install", "admin", "secret", []byte(`{"member": 2, "no_validate": true}`)); code != 202 || sw.install == nil || sw.install.Member != 2 || !sw.install.NoValidate {
		t.Fatalf("install: %d %+v", code, sw.install)
	}
	sw.conflict = true
	if code, _ := do(t, c, "POST", base+"/api/v1/software/install", "admin", "secret", nil); code != 409 {
		t.Fatalf("conflict: %d", code)
	}
	if code, _ := do(t, c, "GET", base+"/api/v1/nothing", "admin", "secret", nil); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
}

func TestLoginLimit(t *testing.T) {
	_, _, base, c := start(t, 64)
	for i := 0; i < maxFails; i++ {
		if code, _ := do(t, c, "GET", base+"/api/v1/software", "admin", "wrong", nil); code != 401 {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	if code, _ := do(t, c, "GET", base+"/api/v1/software", "admin", "secret", nil); code != 429 {
		t.Fatalf("after %d failures: %d", maxFails, code)
	}
}

func TestSyncRestartsOnlyOnChange(t *testing.T) {
	s, _, _, _ := start(t, 64)
	before := s.Info()
	cfg := s.cfg
	cfg.UploadLimit = 128
	s.Sync(&cfg)
	if s.Info().Fingerprint != before.Fingerprint || s.cfg.UploadLimit != 128 {
		t.Fatal("restarted for an upload-limit change")
	}
	s.Sync(nil)
	if s.Info().Running {
		t.Fatal("still running")
	}
}
