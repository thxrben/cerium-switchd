package bgp

import (
	"net/netip"
	"sync"
)

// Limits are the memory slots' capacities for BGP (system memory,
// reference 5.1), shared by every instance's speaker: prefixes per family
// and further paths. A prefix or path beyond them is not stored, as if it
// had been withdrawn; the neighbours stay up.
type Limits struct {
	// Max4, Max6 and MaxPaths are the capacities (0: no limit).
	Max4, Max6, MaxPaths int
	// OnFull is told when a purpose ("bgp-ipv4", "bgp-ipv6", "bgp-paths")
	// becomes full, and when it is below 95 % again.
	OnFull func(purpose string, full bool)

	mu            sync.Mutex
	n4, n6, paths int
	full4, full6  bool
	fullPaths     bool
}

// SetMax changes the capacities (the memory slots of a new run).
func (l *Limits) SetMax(v4, v6, paths int) {
	l.mu.Lock()
	l.Max4, l.Max6, l.MaxPaths = v4, v6, paths
	l.mu.Unlock()
}

// Counts are the entries now: IPv4 and IPv6 prefixes, further paths.
func (l *Limits) Counts() (v4, v6, paths int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n4, l.n6, l.paths
}

// take reserves room for a new prefix (first) or a further path.
func (l *Limits) take(v6, first bool) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n, max, full, name := &l.paths, l.MaxPaths, &l.fullPaths, "bgp-paths"
	switch {
	case first && v6:
		n, max, full, name = &l.n6, l.Max6, &l.full6, "bgp-ipv6"
	case first:
		n, max, full, name = &l.n4, l.Max4, &l.full4, "bgp-ipv4"
	}
	if max > 0 && *n >= max {
		if !*full {
			*full = true
			if l.OnFull != nil {
				go l.OnFull(name, true)
			}
		}
		return false
	}
	*n++
	return true
}

// release gives back what take reserved.
func (l *Limits) release(v6, first bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n, max, full, name := &l.paths, l.MaxPaths, &l.fullPaths, "bgp-paths"
	switch {
	case first && v6:
		n, max, full, name = &l.n6, l.Max6, &l.full6, "bgp-ipv6"
	case first:
		n, max, full, name = &l.n4, l.Max4, &l.full4, "bgp-ipv4"
	}
	if *n > 0 {
		*n--
	}
	if *full && *n*100 < max*95 {
		*full = false
		if l.OnFull != nil {
			go l.OnFull(name, false)
		}
	}
}

// room reports whether a received path for pf may be stored; received
// counts it (+1) or its removal (-1). A prefix's first path counts as a
// prefix, every further one as a path.
func (s *Speaker) room(pf netip.Prefix) bool {
	return s.Limits.take(pf.Addr().Is6(), s.refs[pf] == 0)
}

func (s *Speaker) received(pf netip.Prefix, d int) {
	if s.refs == nil {
		s.refs = map[netip.Prefix]int{}
	}
	if d < 0 {
		r := s.refs[pf]
		if r == 0 {
			return
		}
		s.Limits.release(pf.Addr().Is6(), r == 1)
		if r == 1 {
			delete(s.refs, pf)
		} else {
			s.refs[pf] = r - 1
		}
		return
	}
	s.refs[pf]++
}
