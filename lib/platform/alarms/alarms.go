// Package alarms keeps the alarms active on one member (show system
// alarms, reference 3.5): raised by switchd and the cer- daemons when a
// fault starts, cleared when it ends.
package alarms

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// Classes.
const (
	Major = "Major"
	Minor = "Minor"
)

// Alarm is one active alarm.
type Alarm struct {
	ID     string    `json:"id"` // stable per cause ("hang /var", "cer-bgpd/bfd …")
	Class  string    `json:"class"`
	Text   string    `json:"text"`
	Since  time.Time `json:"since"`
	Member int       `json:"member,omitempty"`

	last time.Time // the latest Raise (ClearStale)
}

// Set is the alarms of a member.
type Set struct {
	mu       sync.Mutex
	m        map[string]Alarm
	onChange func(a Alarm, raised bool)
}

// OnChange sets the function called for a new alarm, a class change and a
// clear (outside the set's lock, in the raiser's goroutine): the owner
// logs and announces it (reference 3.5).
func (s *Set) OnChange(f func(a Alarm, raised bool)) {
	s.mu.Lock()
	s.onChange = f
	s.mu.Unlock()
}

// Raise sets an alarm; it reports whether it is new (a raise with
// another text updates it and keeps its time).
func (s *Set) Raise(id, class, text string) bool {
	s.mu.Lock()
	if s.m == nil {
		s.m = map[string]Alarm{}
	}
	a, ok := s.m[id]
	changed := !ok || a.Class != class
	now := time.Now()
	if ok {
		a.Class, a.Text = class, text
	} else {
		a = Alarm{ID: id, Class: class, Text: text, Since: now}
	}
	a.last = now
	s.m[id] = a
	f := s.onChange
	s.mu.Unlock()
	if changed && f != nil {
		f(a, true)
	}
	return !ok
}

// Clear ends an alarm; it reports whether there was one.
func (s *Set) Clear(id string) bool {
	s.mu.Lock()
	a, ok := s.m[id]
	delete(s.m, id)
	f := s.onChange
	s.mu.Unlock()
	if ok && f != nil {
		f(a, false)
	}
	return ok
}

// ClearStale ends every alarm whose id starts with prefix and that was
// not raised again since before (a daemon that restarted raises again
// what still holds; the rest ends).
func (s *Set) ClearStale(prefix string, before time.Time) {
	s.mu.Lock()
	var gone []Alarm
	for id, a := range s.m {
		if strings.HasPrefix(id, prefix) && a.last.Before(before) {
			delete(s.m, id)
			gone = append(gone, a)
		}
	}
	f := s.onChange
	s.mu.Unlock()
	if f != nil {
		for _, a := range gone {
			f(a, false)
		}
	}
}

// List returns the active alarms, oldest first.
func (s *Set) List() []Alarm {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Alarm, 0, len(s.m))
	for _, a := range s.m {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Alarm) int {
		if c := a.Since.Compare(b.Since); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}
