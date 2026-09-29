package dataplane

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"mclag/internal/schema"
)

// OpKind is the type of a kernel operation.
type OpKind int

const (
	OpCreateBridge OpKind = iota
	OpSetBridge
	OpCreateBond
	OpSetBond
	OpDeleteLink
	OpSetUp
	OpSetDown
	OpSetMaster // Master "" releases the link
	OpSetMTU
	OpSetAlias
	OpVlanDel
	OpVlanSet // add or change flags
	OpSetFlowControl
	OpSetMaxLearned
	OpSetDropTagged
	OpSetStormBroadcast
	OpSetStormMulticast
)

var opNames = [...]string{"create-bridge", "set-bridge", "create-bond", "set-bond", "delete", "up", "down",
	"master", "mtu", "alias", "vlan-del", "vlan-set", "flow-control", "max-learned", "drop-tagged",
	"storm-broadcast", "storm-multicast"}

// Op is one kernel operation.
type Op struct {
	Kind   OpKind
	Link   string
	Master string
	MTU    int
	Alias  string
	VID    uint16
	Flags  VlanFlags
	Bond   *BondOpts
	Bridge *BridgeOpts
	Bool   bool
	Int    int
}

func (o Op) String() string {
	s := opNames[o.Kind] + " " + o.Link
	switch o.Kind {
	case OpSetMaster:
		s += fmt.Sprintf(" %q", o.Master)
	case OpSetMTU:
		s += fmt.Sprintf(" %d", o.MTU)
	case OpSetAlias:
		s += fmt.Sprintf(" %q", o.Alias)
	case OpVlanDel:
		s += fmt.Sprintf(" %d", o.VID)
	case OpVlanSet:
		s += fmt.Sprintf(" %d pvid=%v untagged=%v", o.VID, o.Flags.PVID, o.Flags.Untagged)
	case OpSetFlowControl, OpSetDropTagged:
		s += fmt.Sprintf(" %v", o.Bool)
	case OpSetMaxLearned, OpSetStormBroadcast, OpSetStormMulticast:
		s += fmt.Sprintf(" %d", o.Int)
	case OpCreateBond, OpSetBond:
		s += fmt.Sprintf(" %+v", *o.Bond)
	case OpCreateBridge, OpSetBridge:
		s += fmt.Sprintf(" %+v", *o.Bridge)
	}
	return s
}

// Owned reports whether switchd owns a link that is not in the desired
// state: it is a port of the switch bridge or of an ae bond, an ae bond
// itself, or listed in prev (links managed by the previous apply).
func owned(a *Link, actual *State, prev map[string]bool) bool {
	if prev[a.Name] {
		return true
	}
	if a.Kind == Bond && schema.IsAE(a.Name) {
		return true
	}
	if a.Master == BridgeName {
		return true
	}
	if m := actual.Links[a.Master]; m != nil && m.Kind == Bond && schema.IsAE(m.Name) {
		return true
	}
	return false
}

// Plan returns the operations that turn actual into desired. prev names the
// links managed by the previous apply (so plain ports can be released).
//
// Order (PLAN.md §4.14): first all restrictions (release ports, remove
// VLANs, take links down, add filters), then structure (bonds, MTU,
// enslaving), then permissions (VLANs, removed filters, links up), then
// cleanup. Links whose state already matches get no operation at all.
func Plan(actual, desired *State, prev map[string]bool) []Op {
	var tighten, structure, loosen, up, cleanup []Op

	// The bridge.
	switch {
	case desired.Bridge != nil && actual.Bridge == nil:
		structure = append(structure, Op{Kind: OpCreateBridge, Link: BridgeName, Bridge: desired.Bridge},
			Op{Kind: OpSetUp, Link: BridgeName})
	case desired.Bridge != nil && *desired.Bridge != *actual.Bridge:
		structure = append(structure, Op{Kind: OpSetBridge, Link: BridgeName, Bridge: desired.Bridge})
	}

	// Release links that are owned but no longer desired: down first,
	// then out of the bridge/bundle.
	for _, n := range actual.names() {
		a := actual.Links[n]
		if desired.Links[n] != nil || !a.Present || a.Kind == BridgeKind || a.Kind == Other || !owned(a, actual, prev) {
			continue
		}
		if a.Up {
			tighten = append(tighten, Op{Kind: OpSetDown, Link: n})
		}
		if a.Master != "" {
			tighten = append(tighten, Op{Kind: OpSetMaster, Link: n})
		}
		if a.DropTagged {
			cleanup = append(cleanup, Op{Kind: OpSetDropTagged, Link: n, Bool: false})
		}
		if a.Kind == Bond {
			cleanup = append(cleanup, Op{Kind: OpDeleteLink, Link: n})
		}
	}

	// Bonds before physical ports: a bond must exist (with its MTU) before
	// ports are enslaved.
	names := desired.names()
	slices.SortStableFunc(names, func(x, y string) int {
		return kindOrder(desired.Links[x].Kind, desired.Links[y].Kind)
	})
	for _, n := range names {
		d := desired.Links[n]
		a := actual.Links[n]
		if d.Kind == Physical && (a == nil || !a.Present) {
			continue // not plugged in: applied when it appears
		}
		var cur *Link
		restrict := &tighten
		if a == nil {
			structure = append(structure, Op{Kind: OpCreateBond, Link: n, Bond: d.Bond})
			cur = &Link{Name: n, Kind: Bond, MTU: 1500, Bond: d.Bond, Present: true}
			// A new device is down: its restrictions follow its creation.
			restrict = &structure
		} else {
			cur = a.Clone()
		}

		// Restrictions.
		if cur.Master != d.Master && cur.Master != "" {
			// Role change: leave the old bridge/bundle while down.
			if cur.Up {
				*restrict = append(*restrict, Op{Kind: OpSetDown, Link: n})
				cur.Up = false
			}
			*restrict = append(*restrict, Op{Kind: OpSetMaster, Link: n})
			cur.Master, cur.VLANs = "", nil
		}
		if cur.Master == BridgeName {
			for _, vid := range slices.Sorted(maps.Keys(cur.VLANs)) {
				f := cur.VLANs[vid]
				nf, keep := d.VLANs[vid]
				switch {
				case !keep:
					*restrict = append(*restrict, Op{Kind: OpVlanDel, Link: n, VID: vid})
					delete(cur.VLANs, vid)
				case nf != f && fewerFlags(nf, f):
					*restrict = append(*restrict, Op{Kind: OpVlanSet, Link: n, VID: vid, Flags: nf})
					cur.VLANs[vid] = nf
				}
			}
		}
		if !d.Up && cur.Up {
			*restrict = append(*restrict, Op{Kind: OpSetDown, Link: n})
			cur.Up = false
		}
		if d.DropTagged && !cur.DropTagged {
			*restrict = append(*restrict, Op{Kind: OpSetDropTagged, Link: n, Bool: true})
		}
		if stricter(d.MaxLearned, cur.MaxLearned) {
			*restrict = append(*restrict, Op{Kind: OpSetMaxLearned, Link: n, Int: d.MaxLearned})
		}
		if stricter(d.StormBroadcast, cur.StormBroadcast) {
			*restrict = append(*restrict, Op{Kind: OpSetStormBroadcast, Link: n, Int: d.StormBroadcast})
		}
		if stricter(d.StormMulticast, cur.StormMulticast) {
			*restrict = append(*restrict, Op{Kind: OpSetStormMulticast, Link: n, Int: d.StormMulticast})
		}

		// Structure.
		if d.Kind == Bond && a != nil && d.Bond != nil && (cur.Bond == nil || *cur.Bond != *d.Bond) {
			structure = append(structure, Op{Kind: OpSetBond, Link: n, Bond: d.Bond})
		}
		if d.MTU != cur.MTU {
			structure = append(structure, Op{Kind: OpSetMTU, Link: n, MTU: d.MTU})
		}
		if cur.Master != d.Master {
			structure = append(structure, Op{Kind: OpSetMaster, Link: n, Master: d.Master})
			if d.Master == BridgeName {
				cur.VLANs = map[uint16]VlanFlags{} // default_pvid 0: no VLAN yet
			}
		}

		// Permissions.
		if d.Master == BridgeName {
			for _, vid := range slices.Sorted(maps.Keys(d.VLANs)) {
				if f, ok := cur.VLANs[vid]; !ok || f != d.VLANs[vid] {
					loosen = append(loosen, Op{Kind: OpVlanSet, Link: n, VID: vid, Flags: d.VLANs[vid]})
				}
			}
		}
		if !d.DropTagged && cur.DropTagged {
			loosen = append(loosen, Op{Kind: OpSetDropTagged, Link: n, Bool: false})
		}
		if looser(d.MaxLearned, cur.MaxLearned) {
			loosen = append(loosen, Op{Kind: OpSetMaxLearned, Link: n, Int: d.MaxLearned})
		}
		if looser(d.StormBroadcast, cur.StormBroadcast) {
			loosen = append(loosen, Op{Kind: OpSetStormBroadcast, Link: n, Int: d.StormBroadcast})
		}
		if looser(d.StormMulticast, cur.StormMulticast) {
			loosen = append(loosen, Op{Kind: OpSetStormMulticast, Link: n, Int: d.StormMulticast})
		}
		if d.Alias != cur.Alias {
			loosen = append(loosen, Op{Kind: OpSetAlias, Link: n, Alias: d.Alias})
		}
		if d.FlowControl != nil && (cur.FlowControl == nil || *cur.FlowControl != *d.FlowControl) {
			loosen = append(loosen, Op{Kind: OpSetFlowControl, Link: n, Bool: *d.FlowControl})
		}
		if d.Up && !cur.Up {
			up = append(up, Op{Kind: OpSetUp, Link: n})
		}
	}
	// Ports are brought up before their bond so the bond starts with links.
	slices.SortStableFunc(up, func(x, y Op) int {
		return kindOrder(kindOf(desired, y.Link), kindOf(desired, x.Link))
	})
	return slices.Concat(tighten, structure, loosen, up, cleanup)
}

// stricter reports whether limit want (0 = unlimited) is a new or lower
// limit than have; looser whether it is higher or removed.
func stricter(want, have int) bool { return want != have && want != 0 && (have == 0 || want < have) }
func looser(want, have int) bool   { return want != have && !stricter(want, have) }

// fewerFlags reports whether flags a are a strict restriction of b.
func fewerFlags(a, b VlanFlags) bool {
	return (!a.PVID || b.PVID) && (!a.Untagged || b.Untagged)
}

func kindOf(s *State, n string) Kind {
	if l := s.Links[n]; l != nil {
		return l.Kind
	}
	return Physical
}

// kindOrder sorts bonds before physical ports.
func kindOrder(a, b Kind) int {
	rank := func(k Kind) int {
		if k == Bond {
			return 0
		}
		return 1
	}
	return rank(a) - rank(b)
}

// FormatPlan renders ops one per line (for logs and dry runs).
func FormatPlan(ops []Op) string {
	var b strings.Builder
	for _, o := range ops {
		b.WriteString(o.String())
		b.WriteByte('\n')
	}
	return b.String()
}
