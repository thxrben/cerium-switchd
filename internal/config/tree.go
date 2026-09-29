// Package config implements the hierarchical configuration tree, its
// textual formats (set commands, curly braces, JSON) and diffs.
package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"mclag/internal/schema"
)

// Node is an instance of a schema node in a configuration tree.
type Node struct {
	Schema *schema.Node
	Key    string   // list entries only
	Value  string   // leaves only
	Values []string // leaf-lists only
	Kids   []*Node
	// Inactive marks a statement as deactivated: it is kept and displayed
	// but ignored by validation and the data plane (see Tree.Active).
	Inactive bool
}

// Tree is a complete configuration.
type Tree struct {
	Root *Node
}

// New returns an empty configuration.
func New() *Tree {
	return &Tree{Root: &Node{Schema: schema.Root()}}
}

// Clone returns a deep copy.
func (t *Tree) Clone() *Tree {
	return &Tree{Root: t.Root.clone()}
}

func (n *Node) clone() *Node {
	c := &Node{Schema: n.Schema, Key: n.Key, Value: n.Value, Inactive: n.Inactive}
	if n.Values != nil {
		c.Values = slices.Clone(n.Values)
	}
	if n.Kids != nil {
		c.Kids = make([]*Node, len(n.Kids))
		for i, k := range n.Kids {
			c.Kids[i] = k.clone()
		}
	}
	return c
}

// Name returns the statement keyword.
func (n *Node) Name() string { return n.Schema.Name }

// IsEntry reports whether n is a list entry.
func (n *Node) IsEntry() bool { return n.Schema.Kind == schema.List }

// Child returns the child container/leaf/leaf-list/flag with the given name.
func (n *Node) Child(name string) *Node {
	if n == nil {
		return nil
	}
	for _, k := range n.Kids {
		if k.Schema.Name == name && k.Schema.Kind != schema.List {
			return k
		}
	}
	return nil
}

// Entries returns all entries of the named list below n.
func (n *Node) Entries(list string) []*Node {
	if n == nil {
		return nil
	}
	var out []*Node
	for _, k := range n.Kids {
		if k.Schema.Name == list && k.Schema.Kind == schema.List {
			out = append(out, k)
		}
	}
	return out
}

// Entry returns one entry of the named list.
func (n *Node) Entry(list, key string) *Node {
	for _, e := range n.Entries(list) {
		if e.Key == key {
			return e
		}
	}
	return nil
}

// Get walks a sequence of names from n. List entries are addressed as
// "listname", "key" pairs. It returns nil if the path does not exist.
func (n *Node) Get(path ...string) *Node {
	cur := n
	for i := 0; i < len(path) && cur != nil; i++ {
		sn := cur.Schema.Child(path[i])
		if sn == nil {
			return nil
		}
		if sn.Kind == schema.List {
			if i+1 >= len(path) {
				return nil
			}
			cur = cur.Entry(path[i], path[i+1])
			i++
			continue
		}
		cur = cur.Child(path[i])
	}
	return cur
}

// Leaf returns the value of a leaf below n, or "" if absent.
func (n *Node) Leaf(path ...string) string {
	if c := n.Get(path...); c != nil {
		return c.Value
	}
	return ""
}

// Has reports whether the statement exists (useful for flags and presence
// containers).
func (n *Node) Has(path ...string) bool { return n.Get(path...) != nil }

// List returns the leaf-list values below n.
func (n *Node) List(path ...string) []string {
	if c := n.Get(path...); c != nil {
		return c.Values
	}
	return nil
}

// Step is one resolved element of a configuration path.
type Step struct {
	Schema *schema.Node
	Key    string   // list key (if HasKey)
	HasKey bool     // a key was given for a list
	Values []string // leaf / leaf-list values
	Tok    int      // index of the token that started this step
}

// Words renders the step back into command words.
func (s Step) Words() []string {
	w := []string{s.Schema.Name}
	if s.HasKey {
		w = append(w, Quote(s.Key))
	}
	switch s.Schema.Kind {
	case schema.Leaf:
		for _, v := range s.Values {
			w = append(w, Quote(v))
		}
	case schema.LeafList:
		if len(s.Values) == 1 {
			w = append(w, Quote(s.Values[0]))
		} else if len(s.Values) > 1 {
			w = append(w, "[")
			for _, v := range s.Values {
				w = append(w, Quote(v))
			}
			w = append(w, "]")
		}
	}
	return w
}

// PathString renders steps as a space separated path.
func PathString(steps []Step) string {
	var w []string
	for _, s := range steps {
		w = append(w, s.Words()...)
	}
	return strings.Join(w, " ")
}

// ResolveMode controls how Resolve treats the end of a path.
type ResolveMode int

const (
	// ModeSet requires values for leaves and leaf-lists.
	ModeSet ResolveMode = iota
	// ModeDelete allows paths to end anywhere; values are optional.
	ModeDelete
	// ModeNav is for edit/show: the path must not carry values.
	ModeNav
)

// PathError describes a problem with a token in a path.
type PathError struct {
	Tok    int          // index of the offending token (may equal len(tokens) for "missing")
	Msg    string       //
	Expect *schema.Node // schema node whose children/values were expected
}

func (e *PathError) Error() string { return e.Msg }

// Resolve parses tokens into steps starting at the schema node base.
func Resolve(base *schema.Node, toks []Token, mode ResolveMode) ([]Step, error) {
	var steps []Step
	cur := base
	i := 0
	for i < len(toks) {
		tk := toks[i]
		if !cur.HasChildren() {
			return steps, &PathError{Tok: i, Msg: "syntax error"}
		}
		if tk.Punct {
			return steps, &PathError{Tok: i, Msg: fmt.Sprintf("syntax error, unexpected %q", tk.Text), Expect: cur}
		}
		var matches []*schema.Node
		if !tk.Quoted {
			matches = cur.Lookup(tk.Text)
		}
		if len(matches) == 0 {
			return steps, &PathError{Tok: i, Msg: "syntax error", Expect: cur}
		}
		if len(matches) > 1 {
			names := make([]string, len(matches))
			for j, m := range matches {
				names[j] = m.Name
			}
			return steps, &PathError{Tok: i, Msg: fmt.Sprintf("syntax error, %q is ambiguous: %s", tk.Text, strings.Join(names, ", ")), Expect: cur}
		}
		sn := matches[0]
		st := Step{Schema: sn, Tok: i}
		i++
		switch sn.Kind {
		case schema.Container:
			steps = append(steps, st)
			cur = sn
		case schema.List:
			if i == len(toks) {
				steps = append(steps, st)
				if mode == ModeSet {
					return steps, &PathError{Tok: i, Msg: "missing " + sn.Type.Name, Expect: sn}
				}
				return steps, nil
			}
			if toks[i].Punct {
				return steps, &PathError{Tok: i, Msg: "syntax error, expecting " + sn.Type.Name, Expect: sn}
			}
			key, err := sn.Type.Validate(toks[i].Text)
			if err != nil {
				return steps, &PathError{Tok: i, Msg: err.Error(), Expect: sn}
			}
			st.Key, st.HasKey = key, true
			steps = append(steps, st)
			cur = sn
			i++
		case schema.Leaf:
			if i == len(toks) {
				steps = append(steps, st)
				if mode == ModeSet {
					return steps, &PathError{Tok: i, Msg: "missing argument " + sn.Type.Name, Expect: sn}
				}
				return steps, nil
			}
			if mode == ModeNav {
				return steps, &PathError{Tok: i, Msg: "syntax error"}
			}
			if toks[i].Punct {
				return steps, &PathError{Tok: i, Msg: "syntax error, expecting " + sn.Type.Name, Expect: sn}
			}
			v, err := sn.Type.Validate(toks[i].Text)
			if err != nil {
				return steps, &PathError{Tok: i, Msg: err.Error(), Expect: sn}
			}
			st.Values = []string{v}
			steps = append(steps, st)
			i++
			if i < len(toks) {
				return steps, &PathError{Tok: i, Msg: "syntax error"}
			}
			return steps, nil
		case schema.LeafList:
			if i == len(toks) {
				steps = append(steps, st)
				if mode == ModeSet {
					return steps, &PathError{Tok: i, Msg: "missing argument " + sn.Type.Name, Expect: sn}
				}
				return steps, nil
			}
			if mode == ModeNav {
				return steps, &PathError{Tok: i, Msg: "syntax error"}
			}
			var raw []int // token indices of the values
			if toks[i].Punct && toks[i].Text == "[" {
				open := i
				i++
				closed := false
				for i < len(toks) {
					if toks[i].Punct {
						if toks[i].Text == "]" {
							closed = true
							i++
							break
						}
						return steps, &PathError{Tok: i, Msg: fmt.Sprintf("syntax error, unexpected %q", toks[i].Text)}
					}
					raw = append(raw, i)
					i++
				}
				if !closed {
					return steps, &PathError{Tok: i, Msg: "missing ']'", Expect: sn}
				}
				if len(raw) == 0 {
					return steps, &PathError{Tok: open, Msg: "empty value list", Expect: sn}
				}
			} else if toks[i].Punct {
				return steps, &PathError{Tok: i, Msg: "syntax error, expecting " + sn.Type.Name, Expect: sn}
			} else {
				raw = []int{i}
				i++
			}
			for _, idx := range raw {
				v, err := sn.Type.Validate(toks[idx].Text)
				if err != nil {
					return steps, &PathError{Tok: idx, Msg: err.Error(), Expect: sn}
				}
				if !slices.Contains(st.Values, v) {
					st.Values = append(st.Values, v)
				}
			}
			steps = append(steps, st)
			if i < len(toks) {
				return steps, &PathError{Tok: i, Msg: "syntax error"}
			}
			return steps, nil
		case schema.Flag:
			steps = append(steps, st)
			if i < len(toks) {
				return steps, &PathError{Tok: i, Msg: "syntax error"}
			}
			return steps, nil
		}
	}
	return steps, nil
}

// ErrNotFound is returned by Delete when the statement does not exist.
var ErrNotFound = errors.New("statement not found")

// ErrIncomplete is returned by Set when the path ends at a statement that
// needs more arguments.
type ErrIncomplete struct{ Want string }

func (e *ErrIncomplete) Error() string { return "missing argument " + e.Want }

// Set applies a fully resolved path (from the root) to the tree.
func (t *Tree) Set(steps []Step) error {
	if len(steps) == 0 {
		return &ErrIncomplete{Want: "<statement>"}
	}
	last := steps[len(steps)-1]
	switch last.Schema.Kind {
	case schema.Container:
		if !last.Schema.Presence {
			return &ErrIncomplete{Want: "<statement>"}
		}
	case schema.List:
		if !last.HasKey {
			return &ErrIncomplete{Want: last.Schema.Type.Name}
		}
	case schema.Leaf, schema.LeafList:
		if len(last.Values) == 0 {
			return &ErrIncomplete{Want: last.Schema.Type.Name}
		}
	}
	for _, s := range steps[:len(steps)-1] {
		if s.Schema.Kind == schema.List && !s.HasKey {
			return &ErrIncomplete{Want: s.Schema.Type.Name}
		}
	}
	n := t.Root
	for _, s := range steps {
		n = n.obtain(s)
	}
	return nil
}

// obtain finds or creates the child described by s and applies values.
func (n *Node) obtain(s Step) *Node {
	sn := s.Schema
	var c *Node
	if sn.Kind == schema.List {
		c = n.Entry(sn.Name, s.Key)
	} else {
		c = n.Child(sn.Name)
	}
	if c == nil {
		if sn.Group != "" {
			n.Kids = slices.DeleteFunc(n.Kids, func(k *Node) bool {
				return k.Schema.Group == sn.Group && k.Schema != sn
			})
		}
		c = &Node{Schema: sn, Key: s.Key}
		n.insert(c)
	}
	switch sn.Kind {
	case schema.Leaf:
		c.Value = s.Values[0]
	case schema.LeafList:
		for _, v := range s.Values {
			if !slices.Contains(c.Values, v) {
				c.Values = append(c.Values, v)
			}
		}
	}
	return c
}

func (n *Node) insert(c *Node) {
	pos := len(n.Kids)
	for i, k := range n.Kids {
		if nodeLess(c, k) {
			pos = i
			break
		}
	}
	n.Kids = slices.Insert(n.Kids, pos, c)
}

func nodeLess(a, b *Node) bool {
	if a.Schema.Index() != b.Schema.Index() {
		return a.Schema.Index() < b.Schema.Index()
	}
	return NaturalLess(a.Key, b.Key)
}

// Lookup returns the node addressed by a fully resolved path, or nil.
// A path ending at a list without key returns the parent.
func (t *Tree) Lookup(steps []Step) *Node {
	n := t.Root
	for _, s := range steps {
		if s.Schema.Kind == schema.List {
			if !s.HasKey {
				return n
			}
			n = n.Entry(s.Schema.Name, s.Key)
		} else {
			n = n.Child(s.Schema.Name)
		}
		if n == nil {
			return nil
		}
	}
	return n
}

// Delete removes the statement addressed by steps. Values on leaf-lists
// remove only those values.
func (t *Tree) Delete(steps []Step) error {
	if len(steps) == 0 {
		// "delete" at the top removes everything.
		t.Root.Kids = nil
		return nil
	}
	chain := []*Node{t.Root}
	n := t.Root
	for i, s := range steps {
		last := i == len(steps)-1
		if s.Schema.Kind == schema.List && !s.HasKey {
			if !last {
				return ErrNotFound
			}
			before := len(n.Kids)
			n.Kids = slices.DeleteFunc(n.Kids, func(k *Node) bool { return k.Schema == s.Schema })
			if before == len(n.Kids) {
				return ErrNotFound
			}
			prune(chain)
			return nil
		}
		var c *Node
		if s.Schema.Kind == schema.List {
			c = n.Entry(s.Schema.Name, s.Key)
		} else {
			c = n.Child(s.Schema.Name)
		}
		if c == nil {
			return ErrNotFound
		}
		if last {
			if s.Schema.Kind == schema.LeafList && len(s.Values) > 0 {
				found := false
				c.Values = slices.DeleteFunc(c.Values, func(v string) bool {
					if slices.Contains(s.Values, v) {
						found = true
						return true
					}
					return false
				})
				if !found {
					return ErrNotFound
				}
				if len(c.Values) > 0 {
					return nil
				}
			}
			n.Kids = slices.DeleteFunc(n.Kids, func(k *Node) bool { return k == c })
			prune(chain)
			return nil
		}
		chain = append(chain, c)
		n = c
	}
	return nil
}

// prune removes empty non-presence containers from the end of chain upward.
func prune(chain []*Node) {
	for i := len(chain) - 1; i > 0; i-- {
		n := chain[i]
		if len(n.Kids) > 0 || n.Schema.Kind != schema.Container || n.Schema.Presence {
			return
		}
		p := chain[i-1]
		p.Kids = slices.DeleteFunc(p.Kids, func(k *Node) bool { return k == n })
	}
}

// Equal reports whether two trees hold the same configuration.
func Equal(a, b *Tree) bool { return nodeEqual(a.Root, b.Root) }

func nodeEqual(a, b *Node) bool {
	if a.Schema != b.Schema || a.Key != b.Key || a.Value != b.Value || a.Inactive != b.Inactive || !slices.Equal(a.Values, b.Values) || len(a.Kids) != len(b.Kids) {
		return false
	}
	for i := range a.Kids {
		if !nodeEqual(a.Kids[i], b.Kids[i]) {
			return false
		}
	}
	return true
}

// NaturalLess compares strings so that embedded numbers sort numerically
// ("1/eth2" < "1/eth10", "ae2" < "ae10").
func NaturalLess(a, b string) bool {
	for a != "" && b != "" {
		ad, bd := isDigit(a[0]), isDigit(b[0])
		if ad && bd {
			ea, eb := digitRun(a), digitRun(b)
			na, nb := strings.TrimLeft(a[:ea], "0"), strings.TrimLeft(b[:eb], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			if ea != eb {
				return ea < eb
			}
			a, b = a[ea:], b[eb:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func digitRun(s string) int {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return i
}

// MergeDefaults copies statements from src into n where n does not define
// them itself. Explicit statements in n always win: leaves and leaf-lists
// are never combined, and a statement is skipped if n already has a
// mutually exclusive sibling. Nodes are matched by name (and key), so src
// may come from a different but structurally identical schema subtree.
func (n *Node) MergeDefaults(src *Node) {
	for _, s := range src.Kids {
		var d *Node
		for _, k := range n.Kids {
			if k.Schema.Name == s.Schema.Name && k.Key == s.Key {
				d = k
				break
			}
		}
		if d != nil {
			if s.Schema.Kind == schema.Container || s.Schema.Kind == schema.List {
				d.MergeDefaults(s)
			}
			continue
		}
		if s.Schema.Group != "" && slices.ContainsFunc(n.Kids, func(k *Node) bool { return k.Schema.Group == s.Schema.Group }) {
			continue
		}
		c := s.clone()
		n.Kids = append(n.Kids, c)
	}
	slices.SortStableFunc(n.Kids, func(a, b *Node) int {
		switch {
		case nodeLess(a, b):
			return -1
		case nodeLess(b, a):
			return 1
		}
		return 0
	})
}
