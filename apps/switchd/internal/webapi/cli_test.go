package webapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/lib/conf/config"
)

type nopApplier struct{}

func (nopApplier) Apply(context.Context, *config.Tree, *config.Tree) []commit.MemberResult {
	return []commit.MemberResult{{Member: "member1"}}
}

// withCLI gives the server a real commit engine and CLI.
func withCLI(t *testing.T, s *Server) *commit.Engine {
	t.Helper()
	st, _ := commit.OpenFileStore(filepath.Join(t.TempDir(), "state"), 50)
	quiet := slog.New(slog.DiscardHandler)
	e, err := commit.New(commit.Options{Store: st, Applier: nopApplier{}, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	e.Start(context.Background())
	t.Cleanup(e.Close)
	s.SetShells(func(u User) Shell {
		sh := cli.New(cli.Env{Engine: e, User: u.Name, Class: u.Class, Version: "t", Log: quiet, HostName: func() string { return "sw1" }})
		sh.SetPlainErrors()
		return sh
	})
	return e
}

func doRaw(t *testing.T, c *http.Client, method, url, user, pw, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.SetBasicAuth(user, pw)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A configuration session: open, load, compare, check, commit (with a
// comment); the commit reaches the engine and the session is gone.
func TestConfigSession(t *testing.T) {
	s, _, base, c := start(t, 1<<20)
	e := withCLI(t, s)
	code, body := doRaw(t, c, "POST", base+"/api/v1/config/sessions", "admin", "secret", "")
	if code != http.StatusCreated {
		t.Fatalf("open: %d %s", code, body)
	}
	var sess struct{ ID string }
	json.Unmarshal([]byte(body), &sess)
	at := base + "/api/v1/config/sessions/" + sess.ID
	if code, body := doRaw(t, c, "POST", at+"/load", "admin", "secret", `{"mode":"set","text":"set system host-name api1\n"}`); code != 200 || !strings.Contains(body, `"ok": true`) {
		t.Fatalf("load: %d %s", code, body)
	}
	if _, body := doRaw(t, c, "GET", at+"/compare", "admin", "secret", ""); !strings.Contains(body, "host-name api1") {
		t.Fatalf("compare: %s", body)
	}
	if _, body := doRaw(t, c, "POST", at+"/commands", "admin", "secret", `{"commands":["set system host-name"]}`); !strings.Contains(body, `"ok": false`) {
		t.Fatalf("a bad command reported fine: %s", body)
	}
	if _, body := doRaw(t, c, "POST", at+"/check", "admin", "secret", ""); !strings.Contains(body, `"ok": true`) {
		t.Fatalf("check: %s", body)
	}
	if _, body := doRaw(t, c, "POST", at+"/commit", "admin", "secret", `{"comment":"from the API"}`); !strings.Contains(body, "commit complete") {
		t.Fatalf("commit: %s", body)
	}
	if e.Active().Root.Leaf("system", "host-name") != "api1" {
		t.Fatal("the commit did not reach the engine")
	}
	if code, _ := doRaw(t, c, "GET", at+"/compare", "admin", "secret", ""); code != http.StatusNotFound {
		t.Fatalf("the session survived its commit: %d", code)
	}
	// The configuration as JSON, and the history.
	code, body = doRaw(t, c, "GET", base+"/api/v1/config", "op", "pw", "")
	var tree map[string]any
	if code != 200 || json.Unmarshal([]byte(body), &tree) != nil || tree["system"] == nil {
		t.Fatalf("config: %d %s", code, body)
	}
	if _, body := doRaw(t, c, "GET", base+"/api/v1/config/revisions", "op", "pw", ""); !strings.Contains(body, "from the API") {
		t.Fatalf("revisions: %s", body)
	}
}

// Commands run with the user's class; an operator (not super-user) cannot
// open a session, and a question needs its answer.
func TestCLIEndpoint(t *testing.T) {
	s, _, base, c := start(t, 1<<20)
	withCLI(t, s)
	code, body := doRaw(t, c, "POST", base+"/api/v1/cli", "op", "pw", `{"commands":["show version","show nonsense"]}`)
	var res []Result
	if code != 200 || json.Unmarshal([]byte(body), &res) != nil || len(res) != 2 || !res[0].OK || res[1].OK {
		t.Fatalf("cli: %d %s", code, body)
	}
	if code, _ := doRaw(t, c, "POST", base+"/api/v1/config/sessions", "op", "pw", ""); code != http.StatusForbidden {
		t.Fatalf("an operator opened a session: %d", code)
	}
	if code, body := doRaw(t, c, "POST", base+"/api/v1/cli", "op", "pw", `{"commands":["configure"]}`); code != 200 || !strings.Contains(body, "config/sessions") {
		t.Fatalf("configure through the command endpoint: %s", body)
	}
}
