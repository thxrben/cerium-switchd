// Package docs holds the user documentation. Its tests keep generated
// sections in sync with the code.
package docs

import (
	"flag"
	"os"
	"strings"
	"testing"

	"mclag/internal/config"
	"mclag/internal/model"
	"mclag/internal/schema"
)

var update = flag.Bool("update", false, "rewrite generated documentation sections")

const (
	beginMarker = "<!-- BEGIN GENERATED STATEMENT INDEX (go test ./docs -update) -->\n"
	endMarker   = "<!-- END GENERATED STATEMENT INDEX -->"
)

func TestStatementIndexUpToDate(t *testing.T) {
	const file = "config-reference.md"
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, beginMarker)
	end := strings.Index(doc, endMarker)
	if start < 0 || end < start {
		t.Fatalf("%s: generated section markers missing", file)
	}
	want := schema.ReferenceIndex()
	got := doc[start+len(beginMarker) : end]
	if got == want {
		return
	}
	if !*update {
		t.Fatalf("%s: statement index is out of date; run 'go test ./docs -update'", file)
	}
	doc = doc[:start+len(beginMarker)] + want + doc[end:]
	if err := os.WriteFile(file, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEveryStatementDocumented requires each top-level-and-below statement
// keyword to be explained somewhere in the prose (outside the generated
// index), so new schema nodes cannot be added without documentation.
func TestEveryStatementDocumented(t *testing.T) {
	raw, err := os.ReadFile("config-reference.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if i := strings.Index(doc, beginMarker); i >= 0 {
		doc = doc[:i]
	}
	var walk func(n *schema.Node)
	walk = func(n *schema.Node) {
		for _, c := range n.Children {
			if !strings.Contains(doc, "`"+c.Name) && !strings.Contains(doc, " "+c.Name+" ") && !strings.Contains(doc, " "+c.Name+"`") {
				t.Errorf("statement %q (%s) is not described in the reference prose", c.Name, c.Path())
			}
			walk(c)
		}
	}
	walk(schema.Root())
}

// TestExamplesAreValid parses every complete example of section 7 and runs
// commit check on it: documented examples must be error free.
func TestExamplesAreValid(t *testing.T) {
	raw, err := os.ReadFile("config-reference.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "## 7. Complete examples")
	end := strings.Index(doc, "## 8. Implementation status")
	if start < 0 || end < start {
		t.Fatal("section 7 not found")
	}
	blocks := strings.Split(doc[start:end], "```")
	n := 0
	for i := 1; i < len(blocks); i += 2 {
		text := strings.TrimPrefix(blocks[i], "\n")
		tr := config.New()
		if strings.HasPrefix(text, "set ") || strings.HasPrefix(text, "#") {
			if err := config.ApplySetLines(tr, text); err != nil {
				t.Errorf("example %d: %v", n+1, err)
				continue
			}
		} else if err := config.ParseCurly(tr, text, nil); err != nil {
			t.Errorf("example %d: %v", n+1, err)
			continue
		}
		if _, issues := model.Build(tr, nil); issues.HasErrors() {
			t.Errorf("example %d has commit errors:\n%s", n+1, issues)
		}
		n++
	}
	if n < 2 {
		t.Fatalf("expected at least 2 examples, found %d", n)
	}
}
