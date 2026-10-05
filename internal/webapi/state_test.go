package webapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeState struct{ ready string }

func (fakeState) StateNames() []string { return []string{"uptime"} }
func (fakeState) State(name string, _ url.Values) (any, error) {
	if name != "uptime" {
		return nil, ErrUnknown
	}
	return map[string]int{"seconds": 42}, nil
}
func (f *fakeState) Ready() string { return f.ready }
func (fakeState) Metrics(w io.Writer) error {
	fmt.Fprintln(w, "ceros_alarms{class=\"Major\"} 0")
	return nil
}

func TestStateHealthEvents(t *testing.T) {
	s, _, base, c := start(t, 1<<20)
	get := func(path, user, pw string) (int, string) {
		req, _ := http.NewRequest("GET", base+path, nil)
		if user != "" {
			req.SetBasicAuth(user, pw)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/healthz", "", ""); code != 200 {
		t.Fatalf("healthz %d", code)
	}
	if code, _ := get("/readyz", "", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before the switch is set up: %d", code)
	}
	fs := &fakeState{ready: "the virtual chassis has no master"}
	s.SetState(fs, fs)
	if code, body := get("/readyz", "", ""); code != http.StatusServiceUnavailable || !strings.Contains(body, "no master") {
		t.Fatalf("readyz %d %s", code, body)
	}
	fs.ready = ""
	if code, _ := get("/readyz", "", ""); code != 200 {
		t.Fatalf("readyz when ready: %d", code)
	}
	if code, _ := get("/api/v1/state/uptime", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("state without login: %d", code)
	}
	if code, body := get("/api/v1/state/uptime", "op", "pw"); code != 200 || !strings.Contains(body, `"seconds": 42`) {
		t.Fatalf("state %d %s", code, body)
	}
	if code, _ := get("/api/v1/state/nope", "op", "pw"); code != http.StatusNotFound {
		t.Fatalf("unknown state %d", code)
	}
	if code, body := get("/metrics", "op", "pw"); code != 200 || !strings.Contains(body, "ceros_alarms") {
		t.Fatalf("metrics %d %s", code, body)
	}
	// Events: a notice reaches the stream.
	req, _ := http.NewRequest("GET", base+"/api/v1/events", nil)
	req.SetBasicAuth("op", "pw")
	sc := &http.Client{Transport: c.Transport} // no timeout: a stream
	resp, err := sc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 16)
	go func() {
		r := bufio.NewScanner(resp.Body)
		for r.Scan() {
			lines <- r.Text()
		}
		close(lines)
	}()
	for deadline := time.Now().Add(5 * time.Second); ; {
		s.Publish("member 2: ALARM (Major): swap is active\nsecond line")
		select {
		case l := <-lines:
			if l == "event: notice" {
				if d := <-lines; d != "data: member 2: ALARM (Major): swap is active" {
					t.Fatalf("data %q", d)
				}
				if d := <-lines; d != "data: second line" {
					t.Fatalf("second data line %q", d)
				}
				return
			}
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("no event")
		}
	}
}

// API tokens (Bearer) and the OpenAPI description of every route.
func TestTokensAndOpenAPI(t *testing.T) {
	s, _, base, c := start(t, 1<<20)
	s.TokenAuth = func(tok string) (User, bool) {
		if tok == "good-token" {
			return User{Name: "orchestrator", Class: 2}, true // read-only
		}
		return User{}, false
	}
	fs := &fakeState{}
	s.SetState(fs, fs)
	call := func(path, auth string) int {
		method := "GET"
		if strings.HasSuffix(path, "/install") {
			method = "POST"
		}
		req, _ := http.NewRequest(method, base+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := call("/api/v1/state/uptime", "Bearer good-token"); code != 200 {
		t.Fatalf("good token: %d", code)
	}
	if code := call("/api/v1/state/uptime", "Bearer bad"); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", code)
	}
	if code := call("/api/v1/software/install", "Bearer good-token"); code != http.StatusForbidden {
		t.Fatalf("install with a read-only token: %d", code)
	}
	req, _ := http.NewRequest("GET", base+"/api/v1/openapi.json", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&doc) != nil || doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi %d %+v", resp.StatusCode, doc)
	}
	for _, rt := range s.table() {
		if doc.Paths[rt.path][strings.ToLower(rt.method)] == nil {
			t.Errorf("%s %s missing from the description", rt.method, rt.path)
		}
	}
}
