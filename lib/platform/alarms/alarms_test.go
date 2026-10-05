package alarms

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestSet(t *testing.T) {
	var s Set
	if !s.Raise("a", Major, "one") || s.Raise("a", Major, "one again") {
		t.Fatal("raise: new once")
	}
	s.Raise("cer-bgpd/x", Minor, "two")
	if l := s.List(); len(l) != 2 || l[0].ID != "a" || l[0].Text != "one again" {
		t.Fatalf("list %+v", l)
	}
	s.ClearStale("cer-bgpd/", time.Now().Add(time.Second))
	if !s.Clear("a") || s.Clear("a") || len(s.List()) != 0 {
		t.Fatalf("clear: %+v", s.List())
	}
}

// TestOnChange: a new alarm, a class change and a clear are reported; a
// raise of a held alarm (same class) is not.
func TestOnChange(t *testing.T) {
	var s Set
	var got []string
	s.OnChange(func(a Alarm, raised bool) { got = append(got, fmt.Sprintf("%s %s %v", a.ID, a.Class, raised)) })
	s.Raise("a", Minor, "x")
	s.Raise("a", Minor, "x, still")
	s.Raise("a", Major, "x, worse")
	s.Clear("a")
	s.Clear("a")
	want := []string{"a Minor true", "a Major true", "a Major false"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestClearStale: a daemon's alarm raised again after its reconnect
// stays (no clear/raise pair announced); one not raised again ends.
func TestClearStale(t *testing.T) {
	var s Set
	s.Raise("cer-bgpd/held", Major, "held")
	s.Raise("cer-bgpd/gone", Major, "gone")
	s.Raise("cer-ospfd/other", Major, "other")
	time.Sleep(time.Millisecond)
	reconnect := time.Now()
	s.Raise("cer-bgpd/held", Major, "held")
	var cleared []string
	s.OnChange(func(a Alarm, raised bool) {
		if !raised {
			cleared = append(cleared, a.ID)
		}
	})
	s.ClearStale("cer-bgpd/", reconnect)
	if !slices.Equal(cleared, []string{"cer-bgpd/gone"}) || len(s.List()) != 2 {
		t.Fatalf("cleared %q, left %+v", cleared, s.List())
	}
}
