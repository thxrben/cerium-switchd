// Package docs holds the user documentation. Its tests keep generated
// sections in sync with the code.
package docs

import (
	"flag"
	"os"
	"strings"
	"testing"

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
