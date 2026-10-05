package config

import (
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/lib/conf/schema"
)

func resolve(t *testing.T, mode ResolveMode, path string) []Step {
	t.Helper()
	steps, err := Resolve(schema.Root(), mustLex(t, path), mode)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", path, err)
	}
	return steps
}

func inactiveSample(t *testing.T) *Tree {
	t.Helper()
	tr := sampleTree(t)
	for _, p := range []string{
		"interfaces 1/0/2",            // wrapped list entry
		"system syslog host 10.0.0.5", // keyword list entry
		"system name-server",          // leaf-list
		"interfaces 1/0/10 disable",   // flag
		"vlans storage mtu",           // leaf
		"protocols rstp",              // presence container
		"vlans users vxlan",           // container
	} {
		if err := tr.SetActive(resolve(t, ModeNav, p), false); err != nil {
			t.Fatalf("deactivate %s: %v", p, err)
		}
	}
	return tr
}

func TestInactiveRoundTrip(t *testing.T) {
	tr := inactiveSample(t)

	set := FormatSet(tr)
	for _, want := range []string{
		"\ndeactivate interfaces 1/0/2\n", "\ndeactivate system syslog host 10.0.0.5\n",
		"\ndeactivate system name-server\n", "\ndeactivate protocols rstp\n",
	} {
		if !strings.Contains(set, want) {
			t.Errorf("set output lacks %q:\n%s", want, set)
		}
	}
	if i, j := strings.Index(set, "deactivate"), strings.LastIndex(set, "set "); i < j {
		t.Errorf("deactivate lines must follow all set lines:\n%s", set)
	}
	again, err := ParseSet(set)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(tr, again) {
		t.Fatalf("set round trip differs:\n%s", Diff(tr, again))
	}

	curly := FormatCurly(tr.Root)
	for _, want := range []string{
		"    inactive: 1/0/2 {", "inactive: host 10.0.0.5 {", "inactive: name-server [ 1.1.1.1 9.9.9.9 ];",
		"inactive: disable;", "inactive: mtu 9000;", "inactive: rstp;", "inactive: vxlan {",
	} {
		if !strings.Contains(curly, want) {
			t.Errorf("curly output lacks %q:\n%s", want, curly)
		}
	}
	fromCurly := New()
	if err := ParseCurly(fromCurly, curly, nil); err != nil {
		t.Fatalf("%v\n%s", err, curly)
	}
	if !Equal(tr, fromCurly) {
		t.Fatalf("curly round trip differs:\n%s", Diff(tr, fromCurly))
	}

	js, err := tr.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"@inactive":true`, `"@inactive:disable":true`, `"@inactive:name-server":true`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("json lacks %s: %s", want, js)
		}
	}
	fromJSON, err := FromJSON(js)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(tr, fromJSON) {
		t.Fatalf("json round trip differs:\n%s", Diff(tr, fromJSON))
	}
	for _, bad := range []string{`{"@inactive":true}`, `{"system":{"@inactive:ntp":true}}`, `{"system":{"@inactive:nope":true}}`, `{"system":{"@foo":true}}`} {
		if _, err := FromJSON([]byte(bad)); err == nil {
			t.Errorf("FromJSON(%s) accepted", bad)
		}
	}
}

func TestActiveView(t *testing.T) {
	tr := inactiveSample(t)
	a := tr.Active()
	for _, p := range [][]string{
		{"interfaces", "1/0/2"}, {"system", "syslog"}, {"system", "name-server"},
		{"interfaces", "1/0/10", "disable"}, {"vlans", "storage", "mtu"}, {"protocols"}, {"vlans", "users", "vxlan"},
	} {
		if a.Root.Has(p...) {
			t.Errorf("active view still has %v", p)
		}
	}
	if !a.Root.Has("interfaces", "1/0/10", "description") || !a.Root.Has("vlans", "storage", "vlan-id") {
		t.Error("active view lost active statements")
	}
	if !tr.HasInactive() || a.HasInactive() {
		t.Error("HasInactive wrong")
	}
	// Reactivation restores everything, and child markers survive the parent.
	set(t, tr, "set interfaces 1/0/2 description x")
	if err := tr.SetActive(resolve(t, ModeNav, "interfaces 1/0/2 description"), false); err != nil {
		t.Fatal(err)
	}
	if err := tr.SetActive(resolve(t, ModeNav, "interfaces 1/0/2"), true); err != nil {
		t.Fatal(err)
	}
	if !tr.Active().Root.Has("interfaces", "1/0/2", "mtu") || tr.Active().Root.Has("interfaces", "1/0/2", "description") {
		t.Error("child inactive marker not kept on parent activation")
	}
	if err := tr.SetActive(resolve(t, ModeNav, "system ntp"), false); err != ErrNotFound {
		t.Errorf("deactivating a missing statement: %v", err)
	}
	if err := tr.SetActive(nil, false); err == nil {
		t.Error("deactivating the root accepted")
	}
	// A list without key addresses all entries.
	if err := tr.SetActive(resolve(t, ModeNav, "vlans"), false); err != nil {
		t.Fatal(err)
	}
	if tr.Active().Root.Has("vlans") {
		t.Error("deactivating a list did not deactivate its entries")
	}
}

func TestInactiveDiff(t *testing.T) {
	a := sampleTree(t)
	b := inactiveSample(t)
	d := Diff(a, b)
	for _, want := range []string{
		"[edit interfaces]\n!   inactive: 1/0/2\n",
		"[edit protocols]\n!   inactive: rstp\n",
		"[edit vlans storage]\n-   mtu 9000;\n+   inactive: mtu 9000;\n",
		"[edit vlans users]\n!   inactive: vxlan\n",
		"[edit system syslog]\n!   inactive: host 10.0.0.5\n",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("diff lacks %q:\n%s", want, d)
		}
	}
	if back := Diff(b, a); !strings.Contains(back, "!   active: 1/0/2") {
		t.Errorf("reverse diff:\n%s", back)
	}
}

func TestApplySetLinesVerbs(t *testing.T) {
	tr := New()
	base := resolve(t, ModeNav, "interfaces 1/0/3")
	err := ApplySetLinesAt(tr, "set mtu 9014\nset description x\ndeactivate description\n", base)
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatSet(tr); got != "set interfaces 1/0/3 description x\nset interfaces 1/0/3 mtu 9014\ndeactivate interfaces 1/0/3 description\n" {
		t.Errorf("relative set lines:\n%s", got)
	}
	if err := ApplySetLinesAt(tr, "activate description\ndelete\n", base); err != nil {
		t.Fatal(err)
	}
	if tr.Root.Has("interfaces", "1/0/3", "mtu") {
		t.Error("bare delete at an edit level did not clear it")
	}
	for _, bad := range []string{"deactivate system ntp", "deactivate", "activate system host-name x", "frobnicate system"} {
		if err := ApplySetLines(New(), bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLoadModes(t *testing.T) {
	cand := sampleTree(t)

	// merge: leaves overwritten, leaf-lists extended, nothing removed.
	err := Load(cand, LoadMerge, `
system { host-name edge; name-server 8.8.8.8; }
vlans { inactive: guests { vlan-id 30; } }`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cand.Root.Leaf("system", "host-name") != "edge" || len(cand.Root.List("system", "name-server")) != 3 ||
		!cand.Root.Get("vlans", "guests").Inactive || !cand.Root.Has("vlans", "storage") {
		t.Errorf("merge result:\n%s", FormatCurly(cand.Root))
	}

	// replace: replace: and delete: directives.
	err = Load(cand, LoadReplace, `
system { replace: name-server 9.9.9.9; }
vlans {
    replace: storage { vlan-id 21; }
    delete: empty;
    delete: nonexistent;
}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cand.Root.List("system", "name-server"); len(got) != 1 || got[0] != "9.9.9.9" {
		t.Errorf("replace: leaf-list = %v", got)
	}
	if cand.Root.Has("vlans", "storage", "mtu") || cand.Root.Leaf("vlans", "storage", "vlan-id") != "21" || cand.Root.Has("vlans", "empty") {
		t.Errorf("replace result:\n%s", FormatCurly(cand.Root))
	}

	// replace: on a wrapped list replaces all entries.
	c2 := sampleTree(t)
	if err := Load(c2, LoadReplace, "replace: vlans { only { vlan-id 5; } }", nil); err != nil {
		t.Fatal(err)
	}
	if n := len(c2.Root.Entries("vlans")); n != 1 {
		t.Errorf("replace: vlans left %d entries", n)
	}

	// override replaces everything; set format is detected.
	if err := Load(cand, LoadOverride, "# comment\nset system host-name fresh\n", nil); err != nil {
		t.Fatal(err)
	}
	if FormatSet(cand) != "set system host-name fresh\n" {
		t.Errorf("override result:\n%s", FormatSet(cand))
	}

	// load set relative to an edit level.
	if err := Load(cand, LoadSet, "set host-name again", resolve(t, ModeNav, "system")); err != nil {
		t.Fatal(err)
	}
	if cand.Root.Leaf("system", "host-name") != "again" {
		t.Error("load set at edit level")
	}

	// Emptied containers are removed.
	if err := Load(cand, LoadReplace, "system { delete: host-name; }", nil); err != nil {
		t.Fatal(err)
	}
	if cand.Root.Has("system") {
		t.Errorf("empty container kept:\n%s", FormatCurly(cand.Root))
	}
}

func TestLoadIsAtomic(t *testing.T) {
	cases := []struct {
		mode LoadMode
		text string
		base string
		want string // error substring
	}{
		{LoadMerge, "system { host-name x; }\nvlans { v { vlan-id 99999; } }", "", "line 2"},
		{LoadMerge, "set system host-name x\nset bogus", "", "line 2"},
		{LoadMerge, "vlans { delete: storage; }", "", "only valid with load replace"},
		{LoadMerge, "system { replace: host-name x; }", "", "only valid with load replace"},
		{LoadReplace, "vlans { delete: storage { vlan-id 1; } }", "", "cannot have a block"},
		{LoadReplace, "vlans { delete: inactive: storage; }", "", "cannot be combined"},
		{LoadMerge, "system { inactive: inactive: host-name x; }", "", "duplicate"},
		{LoadMerge, "system { inactive: ; }", "", "missing statement"},
		{LoadOverride, "host-name x;", "system", "top level"},
		{LoadSet, "system { host-name x; }", "", "unknown command"},
	}
	for _, c := range cases {
		cand := sampleTree(t)
		before := FormatSet(cand)
		var base []Step
		if c.base != "" {
			base = resolve(t, ModeNav, c.base)
		}
		err := Load(cand, c.mode, c.text, base)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("load %s %q: error %v, want %q", c.mode, c.text, err, c.want)
		}
		if FormatSet(cand) != before {
			t.Errorf("load %s %q modified the candidate on error", c.mode, c.text)
		}
	}
}

func TestDirectiveWordsAreQuoted(t *testing.T) {
	tr := New()
	set(t, tr, `set system login message "inactive:"`)
	curly := FormatCurly(tr.Root)
	if !strings.Contains(curly, `message "inactive:";`) {
		t.Errorf("directive value not quoted:\n%s", curly)
	}
	back := New()
	if err := ParseCurly(back, curly, nil); err != nil || !Equal(tr, back) {
		t.Errorf("round trip: %v", err)
	}
}

func TestCopyRename(t *testing.T) {
	tr := sampleTree(t)
	if err := tr.Copy(resolve(t, ModeNav, "interfaces 1/0/2"), "1/0/3"); err != nil {
		t.Fatal(err)
	}
	if tr.Root.Leaf("interfaces", "1/0/3", "mtu") != "9216" {
		t.Error("copy lost contents")
	}
	set(t, tr, "set interfaces 1/0/3 mtu 1514")
	if tr.Root.Leaf("interfaces", "1/0/2", "mtu") != "9216" {
		t.Error("copy is not deep")
	}
	if err := tr.Rename(resolve(t, ModeNav, "vlans storage"), "san"); err != nil {
		t.Fatal(err)
	}
	if tr.Root.Has("vlans", "storage") || tr.Root.Leaf("vlans", "san", "vlan-id") != "20" {
		t.Error("rename failed")
	}
	names := []string{}
	for _, e := range tr.Root.Entries("vlans") {
		names = append(names, e.Key)
	}
	if strings.Join(names, ",") != "empty,san,users" {
		t.Errorf("entries not re-sorted after rename: %v", names)
	}
	for _, c := range []struct{ from, to, want string }{
		{"vlans san", "users", "already exists"},
		{"vlans nope", "x", "not found"},
		{"system host-name", "x", "not a list entry"},
		{"vlans san", "9bad", ""},
	} {
		steps, err := Resolve(schema.Root(), mustLex(t, c.from), ModeNav)
		if err != nil {
			steps = resolve(t, ModeDelete, c.from)
		}
		err = tr.Rename(steps, c.to)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("rename %s to %s: %v", c.from, c.to, err)
		}
	}
}
