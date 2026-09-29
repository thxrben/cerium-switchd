package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"mclag/internal/schema"
)

// FormatCurly renders the children of n in curly-brace notation.
func FormatCurly(n *Node) string {
	var b strings.Builder
	writeKids(&b, n, 0)
	return b.String()
}

// FormatNode renders n itself (including its own statement line).
func FormatNode(n *Node) string {
	var b strings.Builder
	writeNode(&b, n, 0)
	return b.String()
}

func indent(b *strings.Builder, depth int) {
	for i := 0; i < depth; i++ {
		b.WriteString("    ")
	}
}

func writeKids(b *strings.Builder, n *Node, depth int) {
	kids := n.Kids
	for i := 0; i < len(kids); i++ {
		k := kids[i]
		if k.Schema.Kind == schema.List && k.Schema.Wrapped {
			// Group all entries of this list into one block.
			j := i
			for j < len(kids) && kids[j].Schema == k.Schema {
				j++
			}
			indent(b, depth)
			b.WriteString(k.Schema.Name + " {\n")
			for _, e := range kids[i:j] {
				writeEntry(b, e, depth+1, false)
			}
			indent(b, depth)
			b.WriteString("}\n")
			i = j - 1
			continue
		}
		writeNode(b, k, depth)
	}
}

func writeEntry(b *strings.Builder, e *Node, depth int, withName bool) {
	indent(b, depth)
	if withName {
		b.WriteString(e.Schema.Name + " ")
	}
	b.WriteString(Quote(e.Key))
	if len(e.Kids) == 0 {
		b.WriteString(";\n")
		return
	}
	b.WriteString(" {\n")
	writeKids(b, e, depth+1)
	indent(b, depth)
	b.WriteString("}\n")
}

func writeNode(b *strings.Builder, n *Node, depth int) {
	switch n.Schema.Kind {
	case schema.List:
		writeEntry(b, n, depth, !n.Schema.Wrapped)
	case schema.Container:
		indent(b, depth)
		if len(n.Kids) == 0 {
			b.WriteString(n.Schema.Name + ";\n")
			return
		}
		b.WriteString(n.Schema.Name + " {\n")
		writeKids(b, n, depth+1)
		indent(b, depth)
		b.WriteString("}\n")
	case schema.Leaf:
		indent(b, depth)
		fmt.Fprintf(b, "%s %s;\n", n.Schema.Name, Quote(n.Value))
	case schema.LeafList:
		indent(b, depth)
		if len(n.Values) == 1 {
			fmt.Fprintf(b, "%s %s;\n", n.Schema.Name, Quote(n.Values[0]))
			return
		}
		q := make([]string, len(n.Values))
		for i, v := range n.Values {
			q[i] = Quote(v)
		}
		fmt.Fprintf(b, "%s [ %s ];\n", n.Schema.Name, strings.Join(q, " "))
	case schema.Flag:
		indent(b, depth)
		b.WriteString(n.Schema.Name + ";\n")
	}
}

// SetLines renders the subtree below n as "set" commands. prefix holds the
// path words of n itself.
func SetLines(n *Node, prefix []string) []string {
	var out []string
	var walk func(n *Node, path []string)
	walk = func(n *Node, path []string) {
		for _, k := range n.Kids {
			p := append(append([]string(nil), path...), k.Schema.Name)
			switch k.Schema.Kind {
			case schema.List:
				p = append(p, Quote(k.Key))
				if len(k.Kids) == 0 {
					out = append(out, "set "+strings.Join(p, " "))
				}
				walk(k, p)
			case schema.Container:
				if len(k.Kids) == 0 {
					out = append(out, "set "+strings.Join(p, " "))
				}
				walk(k, p)
			case schema.Leaf:
				out = append(out, "set "+strings.Join(append(p, Quote(k.Value)), " "))
			case schema.LeafList:
				for _, v := range k.Values {
					out = append(out, "set "+strings.Join(append(p, Quote(v)), " "))
				}
			case schema.Flag:
				out = append(out, "set "+strings.Join(p, " "))
			}
		}
	}
	walk(n, prefix)
	return out
}

// FormatSet renders the whole tree as set commands.
func FormatSet(t *Tree) string {
	lines := SetLines(t.Root, nil)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// LoadError reports a problem while parsing configuration text.
type LoadError struct {
	Line int
	Msg  string
}

func (e *LoadError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// ApplySetLines applies "set"/"delete" commands (one per line) to t.
// Empty lines and lines starting with '#' are ignored.
func ApplySetLines(t *Tree, text string) error {
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		toks, err := Lex(line, LexCommand)
		if err != nil {
			return &LoadError{Line: i + 1, Msg: err.Error()}
		}
		if len(toks) == 0 {
			continue
		}
		verb := toks[0].Text
		mode := ModeSet
		switch verb {
		case "set":
		case "delete":
			mode = ModeDelete
		default:
			return &LoadError{Line: i + 1, Msg: fmt.Sprintf("unknown command %q", verb)}
		}
		steps, err := Resolve(schema.Root(), toks[1:], mode)
		if err != nil {
			return &LoadError{Line: i + 1, Msg: err.Error()}
		}
		if verb == "set" {
			err = t.Set(steps)
		} else {
			err = t.Delete(steps)
			if err == ErrNotFound {
				err = nil
			}
		}
		if err != nil {
			return &LoadError{Line: i + 1, Msg: err.Error()}
		}
	}
	return nil
}

// ParseSet parses set commands into a new tree.
func ParseSet(text string) (*Tree, error) {
	t := New()
	if err := ApplySetLines(t, text); err != nil {
		return nil, err
	}
	return t, nil
}

// ParseCurly parses curly-brace configuration text relative to the schema
// node at base (whose resolved path from the root is basePath) and applies
// it to t.
func ParseCurly(t *Tree, text string, basePath []Step) error {
	toks, err := Lex(text, LexConfig)
	if err != nil {
		return &LoadError{Line: lineOf(text, errPos(err)), Msg: err.Error()}
	}
	type frame struct {
		path   []Step
		schema *schema.Node
		list   *schema.Node // set when inside a wrapped list block
	}
	baseSchema := schema.Root()
	if len(basePath) > 0 {
		baseSchema = basePath[len(basePath)-1].Schema
	}
	stack := []frame{{path: basePath, schema: baseSchema}}
	var stmt []Token
	for _, tk := range toks {
		top := stack[len(stack)-1]
		if tk.Punct && (tk.Text == ";" || tk.Text == "{" || tk.Text == "}") {
			if tk.Text == "}" {
				if len(stmt) > 0 {
					return &LoadError{Line: lineOf(text, tk.Pos), Msg: "missing ';'"}
				}
				if len(stack) == 1 {
					return &LoadError{Line: lineOf(text, tk.Pos), Msg: "unbalanced '}'"}
				}
				stack = stack[:len(stack)-1]
				continue
			}
			if len(stmt) == 0 {
				return &LoadError{Line: lineOf(text, tk.Pos), Msg: fmt.Sprintf("unexpected %q", tk.Text)}
			}
			words := stmt
			if top.list != nil {
				words = append([]Token{{Text: top.list.Name}}, stmt...)
			}
			mode := ModeSet
			if tk.Text == "{" {
				mode = ModeNav
			}
			steps, err := Resolve(top.schema, words, mode)
			line := lineOf(text, stmt[0].Pos)
			if err != nil {
				return &LoadError{Line: line, Msg: err.Error()}
			}
			full := append(append([]Step(nil), top.path...), steps...)
			last := steps[len(steps)-1].Schema
			if tk.Text == "{" {
				if len(steps) == 0 || !last.HasChildren() {
					return &LoadError{Line: line, Msg: "statement cannot have a block"}
				}
				if last.Kind == schema.List && !steps[len(steps)-1].HasKey {
					if !last.Wrapped {
						return &LoadError{Line: line, Msg: "missing " + last.Type.Name}
					}
					stack = append(stack, frame{path: full[:len(full)-1], schema: top.schema, list: last})
					if len(steps) > 1 {
						stack[len(stack)-1].schema = steps[len(steps)-2].Schema
					}
				} else {
					stack = append(stack, frame{path: full, schema: last})
				}
			} else {
				if err := t.Set(full); err != nil {
					return &LoadError{Line: line, Msg: err.Error()}
				}
			}
			stmt = stmt[:0]
			continue
		}
		stmt = append(stmt, tk)
	}
	if len(stmt) > 0 {
		return &LoadError{Line: lineOf(text, stmt[0].Pos), Msg: "missing ';'"}
	}
	if len(stack) != 1 {
		return &LoadError{Line: lineOf(text, len(text)), Msg: "missing '}'"}
	}
	return nil
}

func errPos(err error) int {
	if le, ok := err.(*LexError); ok {
		return le.Pos
	}
	return 0
}

func lineOf(text string, pos int) int {
	if pos > len(text) {
		pos = len(text)
	}
	return strings.Count(text[:pos], "\n") + 1
}

// ToJSON renders the subtree below n as a JSON-compatible value.
func ToJSON(n *Node) map[string]any {
	out := map[string]any{}
	for _, k := range n.Kids {
		switch k.Schema.Kind {
		case schema.List:
			m, _ := out[k.Schema.Name].(map[string]any)
			if m == nil {
				m = map[string]any{}
				out[k.Schema.Name] = m
			}
			m[k.Key] = ToJSON(k)
		case schema.Container:
			out[k.Schema.Name] = ToJSON(k)
		case schema.Leaf:
			out[k.Schema.Name] = k.Value
		case schema.LeafList:
			out[k.Schema.Name] = append([]string(nil), k.Values...)
		case schema.Flag:
			out[k.Schema.Name] = true
		}
	}
	return out
}

// MarshalJSON renders the whole tree as indented JSON.
func (t *Tree) MarshalJSON() ([]byte, error) {
	return json.Marshal(ToJSON(t.Root))
}

// FromJSON builds a tree from the representation produced by ToJSON.
func FromJSON(data []byte) (*Tree, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	t := New()
	if err := fromJSON(t, schema.Root(), nil, m); err != nil {
		return nil, err
	}
	return t, nil
}

func fromJSON(t *Tree, sn *schema.Node, path []Step, m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, name := range keys {
		v := m[name]
		c := sn.Child(name)
		if c == nil {
			return fmt.Errorf("%s: unknown statement %q", PathString(path), name)
		}
		step := Step{Schema: c}
		switch c.Kind {
		case schema.Container:
			sub, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s %s: expecting object", PathString(path), name)
			}
			p := append(append([]Step(nil), path...), step)
			if len(sub) == 0 {
				if c.Presence {
					if err := t.Set(p); err != nil {
						return err
					}
				}
				continue
			}
			if err := fromJSON(t, c, p, sub); err != nil {
				return err
			}
		case schema.List:
			entries, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s %s: expecting object", PathString(path), name)
			}
			ekeys := make([]string, 0, len(entries))
			for k := range entries {
				ekeys = append(ekeys, k)
			}
			sort.Strings(ekeys)
			for _, key := range ekeys {
				canon, err := c.Type.Validate(key)
				if err != nil {
					return fmt.Errorf("%s %s: %v", PathString(path), name, err)
				}
				es := Step{Schema: c, Key: canon, HasKey: true}
				p := append(append([]Step(nil), path...), es)
				if err := t.Set(p); err != nil {
					return err
				}
				sub, ok := entries[key].(map[string]any)
				if !ok {
					return fmt.Errorf("%s: expecting object", PathString(p))
				}
				if err := fromJSON(t, c, p, sub); err != nil {
					return err
				}
			}
		case schema.Leaf:
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("%s %s: expecting string", PathString(path), name)
			}
			val, err := c.Type.Validate(s)
			if err != nil {
				return fmt.Errorf("%s %s: %v", PathString(path), name, err)
			}
			step.Values = []string{val}
			if err := t.Set(append(append([]Step(nil), path...), step)); err != nil {
				return err
			}
		case schema.LeafList:
			arr, ok := v.([]any)
			if !ok {
				return fmt.Errorf("%s %s: expecting array", PathString(path), name)
			}
			for _, x := range arr {
				s, ok := x.(string)
				if !ok {
					return fmt.Errorf("%s %s: expecting strings", PathString(path), name)
				}
				val, err := c.Type.Validate(s)
				if err != nil {
					return fmt.Errorf("%s %s: %v", PathString(path), name, err)
				}
				step.Values = append(step.Values, val)
			}
			if len(step.Values) > 0 {
				if err := t.Set(append(append([]Step(nil), path...), step)); err != nil {
					return err
				}
			}
		case schema.Flag:
			if b, ok := v.(bool); !ok || !b {
				return fmt.Errorf("%s %s: expecting true", PathString(path), name)
			}
			if err := t.Set(append(append([]Step(nil), path...), step)); err != nil {
				return err
			}
		}
	}
	return nil
}
