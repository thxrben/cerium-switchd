// Package schema defines the configuration data model. The same schema drives
// parsing, validation, CLI completion, help texts and rendering.
package schema

import (
	"strings"
)

// Kind is the kind of a schema node.
type Kind int

const (
	// Container groups child statements: "system { ... }".
	Container Kind = iota
	// List is a keyed collection: "interfaces 1/eth0 { ... }".
	List
	// Leaf holds a single value: "mtu 9216;".
	Leaf
	// LeafList holds a set of values: "members [ 10 20 ];".
	LeafList
	// Flag is a valueless statement: "disable;".
	Flag
)

func (k Kind) String() string {
	switch k {
	case Container:
		return "container"
	case List:
		return "list"
	case Leaf:
		return "leaf"
	case LeafList:
		return "leaf-list"
	case Flag:
		return "flag"
	}
	return "unknown"
}

// Node is one statement in the schema tree.
type Node struct {
	Name string
	Help string
	Kind Kind
	// Type is the value type for leaves and leaf-lists and the key type
	// for lists.
	Type *Type
	// Children of containers and list entries, in display order.
	Children []*Node
	// Default value of a leaf, shown in help only.
	Default string
	// Group makes siblings mutually exclusive: setting one removes the
	// other members of the same group.
	Group string
	// Presence marks containers whose mere existence carries meaning
	// (e.g. "protocols rstp"). Non-presence containers are removed when
	// they become empty.
	Presence bool
	// Wrapped lists are rendered with their entries grouped inside a
	// block named after the list ("interfaces { 1/eth0 { ... } }"), the
	// way Junos renders top level lists.
	Wrapped bool
	// MinAbbrev is the shortest prefix that may abbreviate this keyword.
	// It keeps common abbreviations unambiguous ("int" = interfaces, not
	// interface-range).
	MinAbbrev int

	parent *Node
	index  int
}

// Parent returns the parent node (nil for the root).
func (n *Node) Parent() *Node { return n.parent }

// Index returns the position among the parent's children, used for
// stable ordering.
func (n *Node) Index() int { return n.index }

// Child finds a child statement by exact name.
func (n *Node) Child(name string) *Node {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Lookup finds a child by exact name or unique prefix. It returns the
// matching nodes: one node on success, several when ambiguous, none when
// nothing matches.
func (n *Node) Lookup(word string) []*Node {
	if c := n.Child(word); c != nil {
		return []*Node{c}
	}
	if word == "" {
		return nil
	}
	var out []*Node
	for _, c := range n.Children {
		if strings.HasPrefix(c.Name, word) && len(word) >= c.MinAbbrev {
			out = append(out, c)
		}
	}
	return out
}

// Path returns the schema path from the root, e.g. "interfaces unit family".
func (n *Node) Path() string {
	var parts []string
	for p := n; p != nil && p.parent != nil; p = p.parent {
		parts = append(parts, p.Name)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, " ")
}

// HasChildren reports whether the node can contain statements.
func (n *Node) HasChildren() bool { return n.Kind == Container || n.Kind == List }

func link(n *Node) *Node {
	for i, c := range n.Children {
		c.parent = n
		c.index = i
		link(c)
	}
	return n
}

// Builders used by the schema definition.

// C creates a container.
func C(name, help string, children ...*Node) *Node {
	return &Node{Name: name, Help: help, Kind: Container, Children: children}
}

// P creates a presence container.
func P(name, help string, children ...*Node) *Node {
	n := C(name, help, children...)
	n.Presence = true
	return n
}

// L creates a keyed list.
func L(name, help string, key *Type, children ...*Node) *Node {
	return &Node{Name: name, Help: help, Kind: List, Type: key, Children: children}
}

// V creates a leaf.
func V(name, help string, t *Type) *Node {
	return &Node{Name: name, Help: help, Kind: Leaf, Type: t}
}

// VD creates a leaf with a documented default.
func VD(name, help string, t *Type, def string) *Node {
	n := V(name, help, t)
	n.Default = def
	return n
}

// LL creates a leaf-list.
func LL(name, help string, t *Type) *Node {
	return &Node{Name: name, Help: help, Kind: LeafList, Type: t}
}

// F creates a flag.
func F(name, help string) *Node {
	return &Node{Name: name, Help: help, Kind: Flag}
}

// Grouped assigns a mutual-exclusion group to nodes and returns them.
func Grouped(group string, nodes ...*Node) []*Node {
	for _, n := range nodes {
		n.Group = group
	}
	return nodes
}
