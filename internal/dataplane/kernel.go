package dataplane

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
)

// ErrUnsupported marks operations that are not implemented on this kernel
// yet. ExecuteLenient logs them instead of failing the commit.
var ErrUnsupported = errors.New("not supported")

// Kernel reads and changes the managed kernel state.
type Kernel interface {
	// Read returns the current state: the bridge and all links (unmanaged
	// ones with Kind Other or without Master included, so the planner can
	// tell what exists).
	Read() (*State, error)
	Apply(op Op) error
	// SyncSelfVLANs makes the bridge device itself a member of vids; with
	// prune it also leaves the others (hitless order: add before the IP
	// interfaces change, prune after).
	SyncSelfVLANs(vids []int, prune bool) (bool, error)
	// SyncL3 converges the routed interfaces and static routes; warnings
	// are routes that cannot be active yet.
	SyncL3(l *L3) (changed bool, warnings []string, err error)
	// SyncMirrors converges the port mirroring filters (by device).
	SyncMirrors(want map[string]*MirrorPort) (bool, error)
	// SyncVLANMTU converges the per-VLAN MTU filters (vid -> frame size).
	SyncVLANMTU(mtus map[int]int) (bool, error)
}

// Execute applies ops in order and stops at the first error.
func Execute(k Kernel, ops []Op) error {
	for i, op := range ops {
		if err := k.Apply(op); err != nil {
			return fmt.Errorf("step %d/%d (%s): %w", i+1, len(ops), op, err)
		}
	}
	return nil
}

// Fake is an in-memory kernel with Linux semantics, used by tests and by
// the dry-run mode. It rejects operations the real kernel would reject.
type Fake struct {
	S       *State
	Mirrors map[string]*MirrorPort
	VLANMTU map[int]int
}

// NewFake returns a fake kernel holding a copy of s.
func NewFake(s *State) *Fake {
	c := s.Clone()
	if c.Links == nil {
		c.Links = map[string]*Link{}
	}
	return &Fake{S: c}
}

func (f *Fake) Read() (*State, error) { return f.S.Clone(), nil }

// SyncSelfVLANs records the bridge self VLANs.
func (f *Fake) SyncSelfVLANs(vids []int, prune bool) (bool, error) {
	changed := !slices.Equal(f.S.SelfVLANs, vids)
	f.S.SelfVLANs = slices.Clone(vids)
	return changed, nil
}

// SyncVLANMTU records the VLAN MTU filters.
func (f *Fake) SyncVLANMTU(mtus map[int]int) (bool, error) {
	changed := !reflect.DeepEqual(f.VLANMTU, mtus)
	f.VLANMTU = mtus
	return changed, nil
}

// SyncMirrors records the mirror filters.
func (f *Fake) SyncMirrors(want map[string]*MirrorPort) (bool, error) {
	changed := !reflect.DeepEqual(f.Mirrors, want)
	f.Mirrors = want
	return changed, nil
}

// SyncL3 records the routed configuration.
func (f *Fake) SyncL3(l *L3) (bool, []string, error) {
	changed := !reflect.DeepEqual(f.S.L3, l)
	c := (&State{L3: l}).Clone()
	f.S.L3 = c.L3
	return changed, nil, nil
}

func (f *Fake) link(n string) (*Link, error) {
	l := f.S.Links[n]
	if l == nil || !l.Present {
		return nil, fmt.Errorf("%s: no such device", n)
	}
	return l, nil
}

func (f *Fake) Apply(op Op) error {
	switch op.Kind {
	case OpCreateBridge:
		if f.S.Bridge != nil {
			return fmt.Errorf("%s: exists", op.Link)
		}
		b := *op.Bridge
		f.S.Bridge = &b
		f.S.Links[BridgeName] = &Link{Name: BridgeName, Kind: BridgeKind, MTU: 1500, Present: true}
		return nil
	case OpSetBridge:
		if f.S.Bridge == nil {
			return fmt.Errorf("%s: no such device", op.Link)
		}
		b := *op.Bridge
		f.S.Bridge = &b
		return nil
	case OpCreateTunnel:
		if f.S.Links[op.Link] != nil {
			return fmt.Errorf("%s: exists", op.Link)
		}
		t := *op.Tunnel
		f.S.Links[op.Link] = &Link{Name: op.Link, Kind: Tunnel, MTU: op.MTU, Tunnel: &t, Present: true}
		return nil
	case OpCreateBond:
		if f.S.Links[op.Link] != nil {
			return fmt.Errorf("%s: exists", op.Link)
		}
		b := *op.Bond
		f.S.Links[op.Link] = &Link{Name: op.Link, Kind: Bond, MTU: 1500, Bond: &b, Present: true}
		return nil
	}
	l, err := f.link(op.Link)
	if err != nil {
		return err
	}
	switch op.Kind {
	case OpSetBond:
		if l.Kind != Bond {
			return fmt.Errorf("%s: not a bond", l.Name)
		}
		if l.Bond.Team() != op.Bond.Team() {
			return fmt.Errorf("%s: a bond cannot become a team or back", l.Name)
		}
		b := *op.Bond
		l.Bond = &b
	case OpDeleteLink:
		if l.Kind == Physical {
			return fmt.Errorf("%s: cannot delete a physical device", l.Name)
		}
		for _, o := range f.S.Links {
			if o.Master == l.Name {
				o.Master, o.VLANs = "", nil
			}
		}
		delete(f.S.Links, l.Name)
	case OpSetUp:
		l.Up = true
	case OpSetDown:
		l.Up = false
	case OpSetMaster:
		if op.Master == "" {
			l.Master, l.VLANs = "", nil
			return nil
		}
		if l.Master != "" {
			return fmt.Errorf("%s: already enslaved to %s", l.Name, l.Master)
		}
		m, err := f.link(op.Master)
		if err != nil {
			return err
		}
		switch m.Kind {
		case BridgeKind:
			l.VLANs = map[uint16]VlanFlags{} // default_pvid 0
		case Bond:
			if l.Kind != Physical {
				return fmt.Errorf("%s: only physical ports can join a bond", l.Name)
			}
			if l.Up {
				return fmt.Errorf("%s: device can not be enslaved while up", l.Name)
			}
			l.MTU = m.MTU
		default:
			return fmt.Errorf("%s: not a master device", m.Name)
		}
		l.Master = m.Name
	case OpSetMTU:
		if l.MaxMTU != 0 && op.MTU > l.MaxMTU {
			return fmt.Errorf("%s: mtu %d exceeds the maximum %d", l.Name, op.MTU, l.MaxMTU)
		}
		if l.Kind == Physical && l.Master != "" && f.S.Links[l.Master].Kind == Bond && op.MTU != f.S.Links[l.Master].MTU {
			return fmt.Errorf("%s: mtu of a bond port must equal the bond's", l.Name)
		}
		l.MTU = op.MTU
		if l.Kind == Bond {
			for _, o := range f.S.Links {
				if o.Master == l.Name {
					o.MTU = op.MTU
				}
			}
		}
	case OpSetAlias:
		l.Alias = op.Alias
	case OpVlanDel:
		if l.Master != BridgeName {
			return fmt.Errorf("%s: not a bridge port", l.Name)
		}
		if _, ok := l.VLANs[op.VID]; !ok {
			return fmt.Errorf("%s: vlan %d not configured", l.Name, op.VID)
		}
		delete(l.VLANs, op.VID)
	case OpVlanSet:
		if l.Master != BridgeName {
			return fmt.Errorf("%s: not a bridge port", l.Name)
		}
		if op.Flags.PVID {
			for v, fl := range l.VLANs {
				if fl.PVID && v != op.VID {
					fl.PVID = false
					l.VLANs[v] = fl
				}
			}
		}
		l.VLANs[op.VID] = op.Flags
	case OpSetFlowControl:
		b := op.Bool
		l.FlowControl = &b
	case OpSetLearning:
		if l.Master != BridgeName {
			return fmt.Errorf("%s: not a bridge port", l.Name)
		}
		l.NoLearning = !op.Bool
	case OpSetIsolated:
		if l.Master != BridgeName {
			return fmt.Errorf("%s: not a bridge port", l.Name)
		}
		l.Isolated = op.Bool
	case OpSetMaxLearned:
		l.MaxLearned = op.Int
	case OpSetDropTagged:
		l.DropTagged = op.Bool
	case OpSetStormBroadcast:
		l.StormBroadcast = op.Int
	case OpSetStormMulticast:
		l.StormMulticast = op.Int
	default:
		return fmt.Errorf("unknown op %d", op.Kind)
	}
	return nil
}

// clone of a VLAN map that is never nil (for comparisons).
func vlanSet(m map[uint16]VlanFlags) map[uint16]VlanFlags {
	if m == nil {
		return map[uint16]VlanFlags{}
	}
	return maps.Clone(m)
}

// ExecuteLenient is Execute, except that operations failing with
// ErrUnsupported are reported to warn and skipped.
func ExecuteLenient(k Kernel, ops []Op, warn func(Op, error)) error {
	for i, op := range ops {
		err := k.Apply(op)
		if errors.Is(err, ErrUnsupported) {
			warn(op, err)
			continue
		}
		if err != nil {
			return fmt.Errorf("step %d/%d (%s): %w", i+1, len(ops), op, err)
		}
	}
	return nil
}
