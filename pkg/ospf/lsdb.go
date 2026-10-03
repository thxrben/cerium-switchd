package ospf

import (
	"bytes"
	"cmp"
	"slices"
	"time"
)

// dbEntry is an LSA in the database with its arrival time (for aging:
// the current age is Age + elapsed seconds, at most MaxAge).
type dbEntry struct {
	lsa     *LSA
	arrived time.Time
}

// LSDB is a link-state database of one flooding scope (an area, or the
// AS for external LSAs). It is not safe for concurrent use.
type LSDB struct {
	m map[LSRef]*dbEntry
}

// NewLSDB returns an empty database.
func NewLSDB() *LSDB { return &LSDB{m: map[LSRef]*dbEntry{}} }

func (e *dbEntry) age(now time.Time) uint16 {
	a := int(e.lsa.Age) + int(now.Sub(e.arrived)/time.Second)
	if a > MaxAge || a < 0 {
		return MaxAge
	}
	return uint16(a)
}

// Get returns the database copy of an LSA with its current age (nil: not
// in the database).
func (d *LSDB) Get(r LSRef, now time.Time) *LSA {
	e := d.m[r]
	if e == nil {
		return nil
	}
	if a := e.age(now); a != e.lsa.Age {
		return e.lsa.WithAge(a)
	}
	return e.lsa
}

// Install puts an LSA into the database (replacing an older instance) and
// reports whether its contents changed, which means the routing table must
// be recalculated (RFC 2328 §13.2).
func (d *LSDB) Install(l *LSA, now time.Time) (changed bool) {
	r := l.Ref()
	old := d.m[r]
	d.m[r] = &dbEntry{lsa: l, arrived: now}
	if old == nil {
		return true
	}
	oa := old.age(now)
	switch {
	case l.Options != old.lsa.Options, l.Length != old.lsa.Length,
		(oa >= MaxAge) != (l.Age >= MaxAge),
		!bytes.Equal(l.Raw[lsaHeaderLen:], old.lsa.Raw[lsaHeaderLen:]):
		return true
	}
	return false
}

// Delete removes an LSA.
func (d *LSDB) Delete(r LSRef) { delete(d.m, r) }

// Len returns the number of LSAs.
func (d *LSDB) Len() int { return len(d.m) }

// All returns every LSA with its current age, sorted by type, ID and
// advertising router.
func (d *LSDB) All(now time.Time) []*LSA {
	out := make([]*LSA, 0, len(d.m))
	for r := range d.m {
		out = append(out, d.Get(r, now))
	}
	slices.SortFunc(out, func(a, b *LSA) int { return compareRef(a.Ref(), b.Ref()) })
	return out
}

// OfType returns the LSAs of one type with their current ages, sorted.
func (d *LSDB) OfType(t LSType, now time.Time) []*LSA {
	var out []*LSA
	for r := range d.m {
		if r.Type == t {
			out = append(out, d.Get(r, now))
		}
	}
	slices.SortFunc(out, func(a, b *LSA) int { return compareRef(a.Ref(), b.Ref()) })
	return out
}

// MaxAged returns the LSAs whose current age reached MaxAge.
func (d *LSDB) MaxAged(now time.Time) []LSRef {
	var out []LSRef
	for r, e := range d.m {
		if e.age(now) >= MaxAge {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, compareRef)
	return out
}

// Checksum returns the sum of the LSA checksums (show ospf database
// summary, a cheap way to compare two databases).
func (d *LSDB) Checksum() uint32 {
	var s uint32
	for _, e := range d.m {
		s += uint32(e.lsa.Checksum)
	}
	return s
}

func compareRef(a, b LSRef) int {
	if a.Type != b.Type {
		return cmp.Compare(a.Type, b.Type)
	}
	if a.ID != b.ID {
		return cmp.Compare(a.ID, b.ID)
	}
	return cmp.Compare(a.AdvRtr, b.AdvRtr)
}
