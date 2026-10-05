package config

import (
	"slices"
	"strings"

	"github.com/thxrben/cerium-switchd/lib/conf/schema"
)

// Patch returns set-format commands that turn a into b when applied with
// ApplySetLines. Order: set lines, then deletions, then (de)activations.
// Setting before deleting keeps parents alive (a delete never prunes a
// container that b still has), and leaf-list values are added before the
// old ones are removed. Applied to a different tree (e.g. a newer commit),
// the patch replays exactly the statement-level changes from a to b.
func Patch(a, b *Tree) string {
	p := &patcher{}
	p.node(a.Root, b.Root, nil, nil)
	lines := append(append(p.sets, p.dels...), p.acts...)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

type patcher struct {
	sets, dels, acts []string
}

func words(path []string, n *Node) []string {
	w := append(append([]string(nil), path...), n.Schema.Name)
	if n.Schema.Kind == schema.List {
		w = append(w, Quote(n.Key))
	}
	return w
}

// node diffs the children of a and b. inactive holds the paths of inactive
// ancestors, which must be re-deactivated if a rewrite recreates them.
func (p *patcher) node(a, b *Node, path []string, inactive [][]string) {
	idx := map[string]*Node{}
	for _, k := range b.Kids {
		idx[identity(k)] = k
	}
	seen := map[string]bool{}
	for _, ka := range a.Kids {
		id := identity(ka)
		seen[id] = true
		kb := idx[id]
		w := words(path, ka)
		if kb == nil {
			p.dels = append(p.dels, "delete "+strings.Join(w, " "))
			continue
		}
		switch ka.Schema.Kind {
		case schema.Leaf:
			if ka.Value != kb.Value {
				p.sets = append(p.sets, "set "+strings.Join(append(w, Quote(kb.Value)), " "))
			}
		case schema.LeafList:
			if !appendOnly(ka.Values, kb.Values) {
				// Order changed: rewrite the list. The delete may prune empty
				// parents, so their inactive markers are restored afterwards.
				p.sets = append(p.sets, "delete "+strings.Join(w, " "))
				for _, v := range kb.Values {
					p.sets = append(p.sets, "set "+strings.Join(append(w, Quote(v)), " "))
				}
				for _, anc := range inactive {
					p.acts = append(p.acts, "deactivate "+strings.Join(anc, " "))
				}
				if kb.Inactive {
					p.acts = append(p.acts, "deactivate "+strings.Join(w, " "))
				}
				continue
			}
			for _, v := range kb.Values {
				if !slices.Contains(ka.Values, v) {
					p.sets = append(p.sets, "set "+strings.Join(append(w, Quote(v)), " "))
				}
			}
			for _, v := range ka.Values {
				if !slices.Contains(kb.Values, v) {
					p.dels = append(p.dels, "delete "+strings.Join(append(w, Quote(v)), " "))
				}
			}
		case schema.Container, schema.List:
			anc := inactive
			if kb.Inactive {
				anc = append(slices.Clone(inactive), w)
			}
			p.node(ka, kb, w, anc)
		}
		if ka.Inactive != kb.Inactive {
			verb := "activate "
			if kb.Inactive {
				verb = "deactivate "
			}
			p.acts = append(p.acts, verb+strings.Join(w, " "))
		}
	}
	for _, kb := range b.Kids {
		if seen[identity(kb)] {
			continue
		}
		// SetLines renders children of its argument; wrap kb in a parent.
		lines := SetLines(&Node{Schema: b.Schema, Kids: []*Node{kb}}, path)
		for _, l := range lines {
			if strings.HasPrefix(l, "deactivate ") {
				p.acts = append(p.acts, l)
			} else {
				p.sets = append(p.sets, l)
			}
		}
	}
}

// NodeEqual reports whether two subtrees are identical (nil equals nil).
func NodeEqual(a, b *Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	return nodeEqual(a, b)
}

// appendOnly reports whether b is a with some values removed and new ones
// appended, i.e. whether per-value set/delete commands produce b's order.
func appendOnly(a, b []string) bool {
	var kept []string
	for _, v := range a {
		if slices.Contains(b, v) {
			kept = append(kept, v)
		}
	}
	if !slices.Equal(kept, b[:len(kept)]) {
		return false
	}
	for _, v := range b[len(kept):] {
		if slices.Contains(a, v) {
			return false
		}
	}
	return true
}
