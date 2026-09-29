package config

import (
	"strings"
	"testing"
)

func applyPatch(t *testing.T, a *Tree, patch string) *Tree {
	t.Helper()
	c := a.Clone()
	if err := ApplySetLines(c, patch); err != nil {
		t.Fatalf("patch does not apply: %v\n%s", err, patch)
	}
	return c
}

func TestPatch(t *testing.T) {
	a := sampleTree(t)
	b := inactiveSample(t)
	if err := Load(b, LoadReplace, `
system { host-name edge; replace: name-server [ 8.8.8.8 9.9.9.9 ]; }
vlans { delete: users; new { vlan-id 44; } }
interfaces { 1/eth10 { delete: disable; } }`, nil); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]*Tree{{a, b}, {b, a}, {New(), b}, {b, New()}, {a, a}} {
		got := applyPatch(t, pair[0], Patch(pair[0], pair[1]))
		if !Equal(got, pair[1]) {
			t.Errorf("patch result differs:\n%s\npatch:\n%s", Diff(got, pair[1]), Patch(pair[0], pair[1]))
		}
	}
	if Patch(a, a) != "" {
		t.Error("patch between equal trees is not empty")
	}
	// A deactivated parent keeps its state when its last old child is
	// replaced by a new one.
	x, _ := ParseSet("set system syslog host 1.2.3.4 port 514\ndeactivate system syslog\n")
	y, _ := ParseSet("set system syslog local-buffer-size 1000\ndeactivate system syslog\n")
	if got := applyPatch(t, x, Patch(x, y)); !Equal(got, y) {
		t.Errorf("inactive parent lost:\n%s", Diff(got, y))
	}
}

func TestPatchRebase(t *testing.T) {
	// A private edit (base -> mine) replayed on a newer commit keeps the
	// other user's changes.
	base := sampleTree(t)
	mine := base.Clone()
	set(t, mine, "set vlans storage mtu 9014")
	if err := del(t, mine, "delete vlans users"); err != nil {
		t.Fatal(err)
	}
	theirs := base.Clone()
	set(t, theirs, "set system host-name other")
	got := applyPatch(t, theirs, Patch(base, mine))
	if got.Root.Leaf("system", "host-name") != "other" || got.Root.Leaf("vlans", "storage", "mtu") != "9014" || got.Root.Has("vlans", "users") {
		t.Errorf("rebase result:\n%s", FormatSet(got))
	}
}

func FuzzPatch(f *testing.F) {
	f.Add(FormatCurly(mustSample(f).Root), "system { host-name x; } vlans { v { vlan-id 3; } }")
	f.Add("inactive: protocols { rstp; }", "protocols { rstp { bridge-priority 4096; } }")
	f.Fuzz(func(t *testing.T, ta, tb string) {
		a, b := New(), New()
		if Load(a, LoadMerge, ta, nil) != nil || Load(b, LoadMerge, tb, nil) != nil {
			return
		}
		patch := Patch(a, b)
		c := a.Clone()
		if err := ApplySetLines(c, patch); err != nil {
			t.Fatalf("patch does not apply: %v\n%s", err, patch)
		}
		if !Equal(c, b) {
			t.Fatalf("patch result differs:\n%s\npatch:\n%s", Diff(c, b), patch)
		}
		if strings.Contains(Patch(b, b), "set") {
			t.Fatal("non-empty self patch")
		}
	})
}

func mustSample(f *testing.F) *Tree {
	tr, err := ParseSet(sample)
	if err != nil {
		f.Fatal(err)
	}
	return tr
}
