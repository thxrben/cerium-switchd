package config

import (
	"slices"
	"strings"

	"github.com/thxrben/cerium-switchd/internal/schema"
)

// Diff renders the differences from a to b in Junos "show | compare"
// style. An empty string means no differences.
func Diff(a, b *Tree) string {
	d := &differ{}
	d.node(nil, a.Root, b.Root)
	return d.b.String()
}

// DiffNodes compares two subtrees whose position is described by path.
func DiffNodes(path []string, a, b *Node) string {
	d := &differ{}
	d.node(path, a, b)
	return d.b.String()
}

type differ struct {
	b         strings.Builder
	curHeader string
}

func (d *differ) header(path []string) {
	h := "[edit"
	if len(path) > 0 {
		h += " " + strings.Join(path, " ")
	}
	h += "]"
	if h != d.curHeader {
		d.b.WriteString(h + "\n")
		d.curHeader = h
	}
}

// emit writes all lines of a rendered statement with a +/- marker.
func (d *differ) emit(path []string, sign byte, rendered string) {
	d.header(path)
	for _, l := range strings.Split(strings.TrimRight(rendered, "\n"), "\n") {
		d.b.WriteByte(sign)
		d.b.WriteString("   ")
		d.b.WriteString(l)
		d.b.WriteByte('\n')
	}
}

func identity(n *Node) string {
	return n.Schema.Name + "\x00" + n.Key
}

func (d *differ) node(path []string, a, b *Node) {
	var aKids, bKids []*Node
	if a != nil {
		aKids = a.Kids
	}
	if b != nil {
		bKids = b.Kids
	}
	// Merge the two sorted child lists.
	type pair struct{ a, b *Node }
	var pairs []pair
	idx := map[string]int{}
	for _, k := range aKids {
		idx[identity(k)] = len(pairs)
		pairs = append(pairs, pair{a: k})
	}
	for _, k := range bKids {
		if i, ok := idx[identity(k)]; ok {
			pairs[i].b = k
		} else {
			pairs = append(pairs, pair{b: k})
		}
	}
	slices.SortStableFunc(pairs, func(x, y pair) int {
		nx, ny := x.a, y.a
		if nx == nil {
			nx = x.b
		}
		if ny == nil {
			ny = y.b
		}
		switch {
		case nodeLess(nx, ny):
			return -1
		case nodeLess(ny, nx):
			return 1
		}
		return 0
	})

	// First the statements directly at this level, then nested blocks.
	var nested []pair
	for _, p := range pairs {
		switch {
		case p.a == nil:
			d.stmt(path, '+', p.b)
		case p.b == nil:
			d.stmt(path, '-', p.a)
		default:
			switch p.a.Schema.Kind {
			case schema.Leaf, schema.LeafList, schema.Flag:
				if p.a.Value != p.b.Value || !slices.Equal(p.a.Values, p.b.Values) || p.a.Inactive != p.b.Inactive {
					d.stmt(path, '-', p.a)
					d.stmt(path, '+', p.b)
				}
			case schema.Container, schema.List:
				nested = append(nested, p)
			}
		}
	}
	for _, p := range nested {
		if p.a.Inactive != p.b.Inactive {
			d.activation(path, p.b)
		}
		sub := append(append([]string(nil), path...), p.a.Schema.Name)
		if p.a.Schema.Kind == schema.List {
			sub = append(sub, Quote(p.a.Key))
		}
		d.node(sub, p.a, p.b)
	}
}

// stmt emits one added or removed statement. Entries of wrapped lists are
// shown below a header that includes the list name.
func (d *differ) stmt(path []string, sign byte, n *Node) {
	if n.Schema.Kind == schema.List && n.Schema.Wrapped {
		var b strings.Builder
		writeEntry(&b, n, 0, false)
		d.emit(append(append([]string(nil), path...), n.Schema.Name), sign, b.String())
		return
	}
	d.emit(path, sign, renderStmt(n))
}

// renderStmt renders a node the way it appears inside its parent block.
func renderStmt(n *Node) string {
	var b strings.Builder
	if n.Schema.Kind == schema.List {
		writeEntry(&b, n, 0, true)
		return b.String()
	}
	writeNode(&b, n, 0)
	return b.String()
}

// activation emits a "!" line for a block whose inactive state changed.
func (d *differ) activation(path []string, n *Node) {
	word := "active:"
	if n.Inactive {
		word = DirInactive
	}
	head := n.Schema.Name
	if n.Schema.Kind == schema.List {
		if n.Schema.Wrapped {
			path = append(append([]string(nil), path...), n.Schema.Name)
			head = Quote(n.Key)
		} else {
			head += " " + Quote(n.Key)
		}
	}
	d.header(path)
	d.b.WriteString("!   " + word + " " + head + "\n")
}
