// Package layout checks the repository's layering (PLAN.md "Modular code
// base"): libraries under lib/ never import a program, every program is a
// module of its own under apps/ with only what it needs, and no program
// links another one's implementation.
package layout

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/thxrben/cerium-switchd"

type pkg struct {
	ImportPath string
	Deps       []string
}

// list runs go list in a module directory (relative to the repository).
func list(t *testing.T, dir string, patterns ...string) []pkg {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list", "-e", "-json=ImportPath,Deps"}, patterns...)...)
	cmd.Dir = filepath.Join("..", "..", dir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list in %s: %v", dir, err)
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

// modules lists the module directories below dir that have a go.mod.
func modules(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.Walk(filepath.Join("..", "..", dir), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Name() == "go.mod" {
			rel, _ := filepath.Rel(filepath.Join("..", ".."), filepath.Dir(p))
			out = append(out, rel)
		}
		return nil
	})
	if len(out) == 0 {
		t.Fatalf("no modules under %s", dir)
	}
	return out
}

func TestLibrariesDoNotImportPrograms(t *testing.T) {
	for _, m := range modules(t, "lib") {
		for _, p := range list(t, m, "./...") {
			for _, d := range p.Deps {
				if strings.HasPrefix(d, module+"/apps/") {
					t.Errorf("%s imports %s: libraries never use a program", p.ImportPath, d)
				}
			}
		}
	}
}

// No program links another program's implementation (only the libraries
// and the APIs in lib/platform/api).
func TestProgramsAreSeparate(t *testing.T) {
	for _, m := range modules(t, "apps") {
		own := module + "/" + m
		for _, p := range list(t, m, "./...") {
			for _, d := range p.Deps {
				if strings.HasPrefix(d, module+"/apps/") && d != own && !strings.HasPrefix(d, own+"/") {
					t.Errorf("%s links %s (another program)", p.ImportPath, d)
				}
			}
		}
	}
}

// The CLI client needs the CLI protocol, its terminal and the system
// library only (it starts when switchd does not).
func TestSwcliIsSmall(t *testing.T) {
	allowed := []string{"apps/swcli", "lib/platform/rpc", "lib/platform/rshell", "lib/sys/"}
	for _, p := range list(t, "apps/swcli", "./...") {
		for _, d := range append(p.Deps, p.ImportPath) {
			rel, ok := strings.CutPrefix(d, module+"/")
			if !ok {
				continue
			}
			fine := false
			for _, a := range allowed {
				fine = fine || strings.HasPrefix(rel, a)
			}
			if !fine {
				t.Errorf("swcli depends on %s", rel)
			}
		}
	}
}
