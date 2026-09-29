package schema

import (
	"fmt"
	"strings"
)

// ReferenceIndex renders every statement of the schema as a Markdown table.
// It is embedded in docs/config-reference.md and kept in sync by a test.
func ReferenceIndex() string {
	var b strings.Builder
	b.WriteString("| Statement | Kind | Value | Default | Description |\n")
	b.WriteString("|---|---|---|---|---|\n")
	var walk func(n *Node, path []string)
	walk = func(n *Node, path []string) {
		for _, c := range n.Children {
			p := append(append([]string(nil), path...), c.Name)
			value := ""
			kind := c.Kind.String()
			switch c.Kind {
			case List:
				p = append(p, c.Type.Name)
				value = typeDesc(c.Type)
			case Leaf, LeafList:
				value = typeDesc(c.Type)
			case Container:
				if c.Presence {
					kind = "presence"
				}
			}
			def := c.Default
			if c.Group != "" {
				kind += " (excl. " + c.Group + ")"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n",
				strings.Join(p, " "), kind, esc(value), esc(def), esc(c.Help))
			walk(c, p)
		}
	}
	walk(Root(), nil)
	return b.String()
}

func typeDesc(t *Type) string {
	if t == nil {
		return ""
	}
	if len(t.Enum) > 0 {
		n := make([]string, len(t.Enum))
		for i, e := range t.Enum {
			n[i] = e.Name
		}
		return strings.Join(n, " \\| ")
	}
	if t.Desc != "" {
		return t.Name + " " + t.Desc
	}
	return t.Name
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}
