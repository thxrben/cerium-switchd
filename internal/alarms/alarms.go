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
}

// Set is the alarms of a member.
type Set struct {
	mu sync.Mutex
	m  map[string]Alarm
}

// Raise sets an alarm; it reports whether it is new (a raise with
// another text updates it and keeps its time).
func (s *Set) Raise(id, class, text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]Alarm{}
	}
	a, ok := s.m[id]
	if ok {
		a.Class, a.Text = class, text
		s.m[id] = a
		return false
	}
	s.m[id] = Alarm{ID: id, Class: class, Text: text, Since: time.Now()}
	return true
}

// Clear ends an alarm; it reports whether there was one.
func (s *Set) Clear(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[id]
	delete(s.m, id)
	return ok
}

// ClearPrefix ends every alarm whose id starts with prefix (a daemon that
// restarted raises again what still holds).
func (s *Set) ClearPrefix(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.m {
		if strings.HasPrefix(id, prefix) {
			delete(s.m, id)
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
