package alarms

import "testing"

func TestSet(t *testing.T) {
	var s Set
	if !s.Raise("a", Major, "one") || s.Raise("a", Major, "one again") {
		t.Fatal("raise: new once")
	}
	s.Raise("cer-bgpd/x", Minor, "two")
	if l := s.List(); len(l) != 2 || l[0].ID != "a" || l[0].Text != "one again" {
		t.Fatalf("list %+v", l)
	}
	s.ClearPrefix("cer-bgpd/")
	if !s.Clear("a") || s.Clear("a") || len(s.List()) != 0 {
		t.Fatalf("clear: %+v", s.List())
	}
}
