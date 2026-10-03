package config

import (
	"errors"
	"fmt"
	"slices"

	"github.com/thxrben/cerium-switchd/internal/schema"
)

// Directive words that may prefix a statement in hierarchical text.
const (
	DirInactive = "inactive:"
	DirReplace  = "replace:"
	DirDelete   = "delete:"
)

// IsDirective reports whether s is one of the statement directives.
func IsDirective(s string) bool {
	return s == DirInactive || s == DirReplace || s == DirDelete
}

// SetActive marks the statement addressed by steps as active or inactive.
// A list without key addresses all of its entries. The root cannot be
// deactivated.
func (t *Tree) SetActive(steps []Step, active bool) error {
	if len(steps) == 0 {
		return errors.New("cannot deactivate the whole configuration")
	}
	last := steps[len(steps)-1]
	if last.Schema.Kind == schema.List && !last.HasKey {
		parent := t.Lookup(steps[:len(steps)-1])
		entries := parent.Entries(last.Schema.Name)
		if len(entries) == 0 {
			return ErrNotFound
		}
		for _, e := range entries {
			e.Inactive = !active
		}
		return nil
	}
	n := t.Lookup(steps)
	if n == nil {
		return ErrNotFound
	}
	n.Inactive = !active
	return nil
}

// Active returns a copy of t without inactive statements, i.e. the
// configuration that validation and the data plane act on. Containers
// left empty are removed as if their contents had been deleted.
func (t *Tree) Active() *Tree {
	c := &Tree{Root: activeCopy(t.Root)}
	return c
}

func activeCopy(n *Node) *Node {
	c := &Node{Schema: n.Schema, Key: n.Key, Value: n.Value}
	if n.Values != nil {
		c.Values = slices.Clone(n.Values)
	}
	for _, k := range n.Kids {
		if k.Inactive {
			continue
		}
		kc := activeCopy(k)
		if kc.Schema.Kind == schema.Container && !kc.Schema.Presence && len(kc.Kids) == 0 {
			continue
		}
		c.Kids = append(c.Kids, kc)
	}
	return c
}

// HasInactive reports whether any statement in t is inactive.
func (t *Tree) HasInactive() bool {
	var walk func(n *Node) bool
	walk = func(n *Node) bool {
		for _, k := range n.Kids {
			if k.Inactive || walk(k) {
				return true
			}
		}
		return false
	}
	return walk(t.Root)
}

// Normalize removes empty non-presence containers everywhere.
func (t *Tree) Normalize() { normalize(t.Root) }

func normalize(n *Node) {
	n.Kids = slices.DeleteFunc(n.Kids, func(k *Node) bool {
		normalize(k)
		return k.Schema.Kind == schema.Container && !k.Schema.Presence && len(k.Kids) == 0
	})
}

// remove deletes the statement addressed by steps without pruning parents
// (a list without key removes all of its entries). It reports whether
// anything was removed.
func (t *Tree) remove(steps []Step) bool {
	if len(steps) == 0 {
		had := len(t.Root.Kids) > 0
		t.Root.Kids = nil
		return had
	}
	last := steps[len(steps)-1]
	parent := t.Lookup(steps[:len(steps)-1])
	if parent == nil {
		return false
	}
	before := len(parent.Kids)
	parent.Kids = slices.DeleteFunc(parent.Kids, func(k *Node) bool {
		if k.Schema != last.Schema {
			return false
		}
		return last.Schema.Kind != schema.List || !last.HasKey || k.Key == last.Key
	})
	return len(parent.Kids) != before
}

// Copy duplicates the list entry addressed by from under the key to.
func (t *Tree) Copy(from []Step, to string) error {
	src, parent, key, err := t.entryOp(from, to)
	if err != nil {
		return err
	}
	c := src.clone()
	c.Key = key
	parent.insert(c)
	return nil
}

// Rename changes the key of the list entry addressed by from. References
// to the old name elsewhere are not changed.
func (t *Tree) Rename(from []Step, to string) error {
	src, parent, key, err := t.entryOp(from, to)
	if err != nil {
		return err
	}
	parent.Kids = slices.DeleteFunc(parent.Kids, func(k *Node) bool { return k == src })
	src.Key = key
	parent.insert(src)
	return nil
}

func (t *Tree) entryOp(from []Step, to string) (src, parent *Node, key string, err error) {
	if len(from) == 0 {
		return nil, nil, "", errors.New("expecting a list entry")
	}
	last := from[len(from)-1]
	if last.Schema.Kind != schema.List || !last.HasKey {
		return nil, nil, "", fmt.Errorf("%s is not a list entry", PathString(from))
	}
	key, err = last.Schema.Type.Validate(to)
	if err != nil {
		return nil, nil, "", err
	}
	parent = t.Lookup(from[:len(from)-1])
	if parent == nil {
		return nil, nil, "", ErrNotFound
	}
	src = parent.Entry(last.Schema.Name, last.Key)
	if src == nil {
		return nil, nil, "", ErrNotFound
	}
	if parent.Entry(last.Schema.Name, key) != nil {
		return nil, nil, "", fmt.Errorf("%s %s already exists", last.Schema.Name, Quote(key))
	}
	return src, parent, key, nil
}
