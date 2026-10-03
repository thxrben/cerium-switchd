// Package layout checks the repository's layering (PLAN.md Phase 9a):
// libraries under pkg/ are reusable outside this repository and never
// import its internal packages, and every program builds on its own with
// only what it needs.
package layout

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/thxrben/cerium-switchd"

type pkg struct {
	ImportPath string
	Deps       []string
}

func list(t *testing.T, patterns ...string) []pkg {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list", "-e", "-json=ImportPath,Deps"}, patterns...)...).Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var ps []pkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		ps = append(ps, p)
	}
	return ps
}

func TestLibrariesDoNotImportInternal(t *testing.T) {
	for _, p := range list(t, module+"/pkg/...") {
		if !strings.HasPrefix(p.ImportPath, module+"/pkg/") {
			continue // no pkg/ yet: the pattern matches nothing
		}
		for _, d := range p.Deps {
			if strings.HasPrefix(d, module+"/internal/") {
				t.Errorf("%s imports %s: libraries under pkg/ must not use internal packages", p.ImportPath, d)
			}
		}
	}
}

// The CLI client needs the CLI protocol and its terminal only, not the
// switch daemon (it starts when switchd does not).
func TestSwcliIsSmall(t *testing.T) {
	allowed := map[string]bool{"internal/rpc": true, "internal/rshell": true, "internal/swcli": true, "cmd/swcli": true}
	for _, p := range list(t, module+"/cmd/swcli") {
		for _, d := range append(p.Deps, p.ImportPath) {
			rel, ok := strings.CutPrefix(d, module+"/")
			if ok && !allowed[rel] && !strings.HasPrefix(rel, "pkg/") {
				t.Errorf("swcli depends on %s", rel)
			}
		}
	}
}

// The cer- daemons never link the configuration engine, the CLI or the
// data plane: switchd computes their configuration.
func TestDaemonsDoNotLinkSwitchd(t *testing.T) {
	forbidden := []string{"internal/commit", "internal/cli", "internal/daemon", "internal/dataplane", "internal/rpcserver"}
	for _, p := range list(t, module+"/cmd/...") {
		name := strings.TrimPrefix(p.ImportPath, module+"/cmd/")
		if !strings.HasPrefix(name, "cer-") {
			continue
		}
		for _, d := range p.Deps {
			for _, f := range forbidden {
				if d == module+"/"+f {
					t.Errorf("%s links %s", name, f)
				}
			}
		}
	}
}
