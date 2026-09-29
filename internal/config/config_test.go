package config

import (
	"strings"
	"testing"

	"mclag/internal/schema"
)

func mustLex(t *testing.T, s string) []Token {
	t.Helper()
	toks, err := Lex(s, LexCommand)
	if err != nil {
		t.Fatalf("Lex(%q): %v", s, err)
	}
	return toks
}

func set(t *testing.T, tr *Tree, line string) {
	t.Helper()
	steps, err := Resolve(schema.Root(), mustLex(t, line)[1:], ModeSet)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", line, err)
	}
	if err := tr.Set(steps); err != nil {
		t.Fatalf("Set(%q): %v", line, err)
	}
}

func del(t *testing.T, tr *Tree, line string) error {
	t.Helper()
	steps, err := Resolve(schema.Root(), mustLex(t, line)[1:], ModeDelete)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", line, err)
	}
	return tr.Delete(steps)
}

const sample = `set system host-name core
set system name-server 1.1.1.1
set system name-server 9.9.9.9
set system syslog host 10.0.0.5 transport tls
set system login user alice class super-user
set system login user alice authentication ssh-key "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx alice@laptop"
set stack member 1 host-name sw-a
set stack member 2 host-name sw-b
set interfaces 1/eth2 mtu 9216
set interfaces 1/eth10 description "uplink to core"
set interfaces 1/eth10 disable
set interfaces ae1 aggregated-ether-options lacp active
set interfaces ae1 aggregated-ether-options mclag
set interfaces ae1 unit 0 family ethernet-switching interface-mode trunk
set interfaces ae1 unit 0 family ethernet-switching vlan members 10
set interfaces ae1 unit 0 family ethernet-switching vlan members storage
set vlans storage vlan-id 20
set vlans storage mtu 9000
set vlans users vlan-id 10
set vlans users vxlan vni 10010
set vlans empty
set protocols rstp
`

func sampleTree(t *testing.T) *Tree {
	t.Helper()
	tr, err := ParseSet(sample)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestSetAndOrdering(t *testing.T) {
	tr := sampleTree(t)
	got := FormatSet(tr)
	// Natural sort: eth2 before eth10, schema order: system before stack.
	i2 := strings.Index(got, "1/eth2")
	i10 := strings.Index(got, "1/eth10")
	if i2 < 0 || i10 < 0 || i2 > i10 {
		t.Errorf("natural ordering broken:\n%s", got)
	}
	if strings.Index(got, "set system") > strings.Index(got, "set stack") {
		t.Errorf("schema ordering broken")
	}
	if tr.Root.Leaf("interfaces", "1/eth2", "mtu") != "9216" {
		t.Errorf("mtu not set")
	}
	if !tr.Root.Has("protocols", "rstp") {
		t.Errorf("presence container lost")
	}
	if got := tr.Root.List("interfaces", "ae1", "unit", "0", "family", "ethernet-switching", "vlan", "members"); strings.Join(got, ",") != "10,storage" {
		t.Errorf("members = %v", got)
	}
}

func TestLeafReplaceAndGroups(t *testing.T) {
	tr := sampleTree(t)
	set(t, tr, "set interfaces 1/eth2 mtu 1500")
	if v := tr.Root.Leaf("interfaces", "1/eth2", "mtu"); v != "1500" {
		t.Fatalf("mtu = %s", v)
	}
	set(t, tr, "set interfaces ae1 aggregated-ether-options lacp passive")
	lacp := tr.Root.Get("interfaces", "ae1", "aggregated-ether-options", "lacp")
	if lacp.Child("active") != nil || lacp.Child("passive") == nil {
		t.Fatalf("group exclusion failed: %s", FormatNode(lacp))
	}
	set(t, tr, "set stack member 1 management interface eno1")
	set(t, tr, "set stack member 1 management vlan 10")
	if tr.Root.Has("stack", "member", "1", "management", "interface") {
		t.Fatalf("interface should have been replaced by vlan")
	}
}

func TestLeafListBrackets(t *testing.T) {
	tr := New()
	set(t, tr, "set system name-server [ 1.1.1.1 8.8.8.8 1.1.1.1 ]")
	if got := tr.Root.List("system", "name-server"); len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if err := del(t, tr, "delete system name-server 1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if got := tr.Root.List("system", "name-server"); len(got) != 1 || got[0] != "8.8.8.8" {
		t.Fatalf("got %v", got)
	}
	if err := del(t, tr, "delete system name-server 7.7.7.7"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := del(t, tr, "delete system name-server 8.8.8.8"); err != nil {
		t.Fatal(err)
	}
	if tr.Root.Has("system") {
		t.Fatalf("empty non-presence containers must be pruned:\n%s", FormatCurly(tr.Root))
	}
}

func TestDelete(t *testing.T) {
	tr := sampleTree(t)
	if err := del(t, tr, "delete interfaces 1/eth10 disable"); err != nil {
		t.Fatal(err)
	}
	if tr.Root.Has("interfaces", "1/eth10", "disable") {
		t.Fatal("flag not deleted")
	}
	if !tr.Root.Has("interfaces", "1/eth10") {
		t.Fatal("list entries must not be pruned")
	}
	if err := del(t, tr, "delete vlans"); err != nil {
		t.Fatal(err)
	}
	if len(tr.Root.Entries("vlans")) != 0 {
		t.Fatal("vlans not deleted")
	}
	if err := del(t, tr, "delete vlans"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := del(t, tr, "delete protocols rstp"); err != nil {
		t.Fatal(err)
	}
	if tr.Root.Has("protocols") {
		t.Fatal("protocols should be pruned")
	}
}

func TestResolveErrors(t *testing.T) {
	cases := []struct {
		line string
		mode ResolveMode
		tok  int
		msg  string
	}{
		{"set foo", ModeSet, 0, "syntax error"},
		{"s", ModeSet, 0, "ambiguous"},
		{"interfaces 1/eth0 mtu", ModeSet, 3, "missing argument"},
		{"interfaces 1/eth0 mtu 99999", ModeSet, 3, "out of range"},
		{"interfaces 1/eth0 mtu 1500 extra", ModeSet, 4, "syntax error"},
		{"interfaces bogus mtu 1500", ModeSet, 1, "invalid interface"},
		{"interfaces", ModeSet, 1, "missing"},
		{"interfaces 1/eth0 disable now", ModeSet, 3, "syntax error"},
		{"system name-server [ 1.1.1.1 bad ]", ModeSet, 4, "invalid IP"},
		{"system name-server [ 1.1.1.1", ModeSet, 4, "missing ']'"},
		{"system name-server [ ]", ModeSet, 2, "empty"},
		{"system host-name x", ModeNav, 2, "syntax error"},
		{"vlans v1 | x", ModeSet, 2, "unexpected"},
	}
	for _, c := range cases {
		toks, _ := Lex(c.line, LexCommand)
		if strings.HasPrefix(c.line, "set ") {
			toks = toks[1:]
		}
		_, err := Resolve(schema.Root(), toks, c.mode)
		pe, ok := err.(*PathError)
		if !ok {
			t.Errorf("%q: want PathError, got %v", c.line, err)
			continue
		}
		if pe.Tok != c.tok || !strings.Contains(pe.Msg, c.msg) {
			t.Errorf("%q: got tok=%d msg=%q, want tok=%d msg~%q", c.line, pe.Tok, pe.Msg, c.tok, c.msg)
		}
	}
}

func TestSetIncomplete(t *testing.T) {
	tr := New()
	steps, err := Resolve(schema.Root(), mustLex(t, "system"), ModeSet)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Set(steps); err == nil {
		t.Fatal("setting a bare non-presence container must fail")
	}
}

func TestFormatsRoundTrip(t *testing.T) {
	tr := sampleTree(t)

	// set -> tree -> set
	again, err := ParseSet(FormatSet(tr))
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(tr, again) {
		t.Fatalf("set round trip differs:\n%s", Diff(tr, again))
	}

	// curly -> tree
	curly := FormatCurly(tr.Root)
	fromCurly := New()
	if err := ParseCurly(fromCurly, curly, nil); err != nil {
		t.Fatalf("ParseCurly: %v\n%s", err, curly)
	}
	if !Equal(tr, fromCurly) {
		t.Fatalf("curly round trip differs:\n%s\n%s", Diff(tr, fromCurly), curly)
	}
	if !strings.Contains(curly, "interfaces {\n    1/eth2 {\n        mtu 9216;") {
		t.Errorf("wrapped list rendering unexpected:\n%s", curly)
	}
	if !strings.Contains(curly, "rstp;") || !strings.Contains(curly, "    empty;") {
		t.Errorf("empty presence/entries not rendered:\n%s", curly)
	}

	// JSON
	js, err := tr.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := FromJSON(js)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(tr, fromJSON) {
		t.Fatalf("json round trip differs:\n%s", Diff(tr, fromJSON))
	}
}

func TestParseCurlyErrors(t *testing.T) {
	cases := []string{
		"system { host-name x; ",
		"system { host-name x }",
		"}",
		"interfaces { 1/eth0 { mtu 99999999; } }",
		"system { host-name { } }",
		"; ",
		"bogus;",
		"/* unterminated",
		`system { host-name "open; }`,
	}
	for _, c := range cases {
		if err := ParseCurly(New(), c, nil); err == nil {
			t.Errorf("ParseCurly(%q) should fail", c)
		}
	}
	// Comments are accepted.
	if err := ParseCurly(New(), "# comment\nsystem { /* inline */ host-name x; }", nil); err != nil {
		t.Fatal(err)
	}
}

func TestParseCurlyAtEditPath(t *testing.T) {
	tr := New()
	base, err := Resolve(schema.Root(), mustLex(t, "interfaces 1/eth0"), ModeNav)
	if err != nil {
		t.Fatal(err)
	}
	if err := ParseCurly(tr, "mtu 9000; unit 0 { family { ethernet-switching { vlan { members [ 10 20 ]; } } } }", base); err != nil {
		t.Fatal(err)
	}
	if tr.Root.Leaf("interfaces", "1/eth0", "mtu") != "9000" {
		t.Fatalf("got:\n%s", FormatSet(tr))
	}
}

func TestDiff(t *testing.T) {
	a := sampleTree(t)
	b := a.Clone()
	set(t, b, "set interfaces 1/eth2 mtu 1500")
	set(t, b, "set interfaces 1/eth3 mtu 9000")
	if err := del(t, b, "delete vlans users"); err != nil {
		t.Fatal(err)
	}
	set(t, b, "set system name-server 8.8.8.8")
	d := Diff(a, b)
	want := []string{
		"[edit system]\n-   name-server [ 1.1.1.1 9.9.9.9 ];\n+   name-server [ 1.1.1.1 9.9.9.9 8.8.8.8 ];",
		"[edit interfaces]\n+   1/eth3 {\n+       mtu 9000;\n+   }",
		"[edit interfaces 1/eth2]\n-   mtu 9216;\n+   mtu 1500;",
		"[edit vlans]\n-   users {\n-       vlan-id 10;",
	}
	for _, w := range want {
		if !strings.Contains(d, w) {
			t.Errorf("diff missing %q\n--- got:\n%s", w, d)
		}
	}
	if Diff(a, a.Clone()) != "" {
		t.Error("identical trees must not differ")
	}
	// The clone must be independent.
	if a.Root.Leaf("interfaces", "1/eth2", "mtu") != "9216" {
		t.Error("clone shares state with original")
	}
}

func TestNaturalLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"ae2", "ae10", true},
		{"ae10", "ae2", false},
		{"1/eth9", "1/eth10", true},
		{"2/eth0", "10/eth0", true},
		{"a", "b", true},
		{"a", "a", false},
		{"a", "a1", true},
		{"01", "1", false},
		{"1", "01", true},
	}
	for _, c := range cases {
		if got := NaturalLess(c.a, c.b); got != c.want {
			t.Errorf("NaturalLess(%q,%q)=%v", c.a, c.b, got)
		}
	}
}

func TestLexQuote(t *testing.T) {
	vals := []string{"plain", "with space", `quote"inside`, `back\slash`, "semi;colon", "", "[x]", "a|b", "#hash", "/*c"}
	for _, v := range vals {
		toks, err := Lex(Quote(v), LexConfig)
		if err != nil || len(toks) != 1 || toks[0].Text != v {
			t.Errorf("Quote/Lex(%q) -> %q: %+v %v", v, Quote(v), toks, err)
		}
		toks, err = Lex(Quote(v), LexCommand)
		if err != nil || len(toks) != 1 || toks[0].Text != v {
			t.Errorf("Quote/Lex command(%q) -> %+v %v", v, toks, err)
		}
	}
}

// Fuzz targets: nothing may panic for any input.

func FuzzLexResolve(f *testing.F) {
	for _, l := range strings.Split(sample, "\n") {
		f.Add(l)
	}
	f.Add(`set system name-server [ 1.1.1.1 "x`)
	f.Add("set [ ] | | [")
	f.Fuzz(func(t *testing.T, line string) {
		toks, _ := Lex(line, LexCommand)
		for _, mode := range []ResolveMode{ModeSet, ModeDelete, ModeNav} {
			steps, err := Resolve(schema.Root(), toks, mode)
			if err != nil {
				if pe, ok := err.(*PathError); ok && (pe.Tok < 0 || pe.Tok > len(toks)) {
					t.Fatalf("token index %d out of range", pe.Tok)
				}
				continue
			}
			tr := New()
			if mode == ModeSet {
				if err := tr.Set(steps); err == nil {
					// Whatever was set must survive a round trip.
					again, err := ParseSet(FormatSet(tr))
					if err != nil {
						t.Fatalf("reparse failed: %v\n%s", err, FormatSet(tr))
					}
					if !Equal(tr, again) {
						t.Fatalf("round trip differs for %q", line)
					}
				}
			} else {
				_ = tr.Delete(steps)
			}
		}
	})
}

func FuzzParseCurly(f *testing.F) {
	tr, err := ParseSet(sample)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(FormatCurly(tr.Root))
	f.Add("interfaces { 1/eth0; }")
	f.Add("a { b { c; } }}")
	f.Fuzz(func(t *testing.T, text string) {
		tr := New()
		if err := ParseCurly(tr, text, nil); err != nil {
			return
		}
		again := New()
		if err := ParseCurly(again, FormatCurly(tr.Root), nil); err != nil {
			t.Fatalf("reparse of rendered config failed: %v", err)
		}
		if !Equal(tr, again) {
			t.Fatalf("curly round trip differs")
		}
	})
}

func TestMergeDefaults(t *testing.T) {
	tr, err := ParseSet(`set interfaces 1/eth0 mtu 9216
set interfaces 1/eth0 ether-options no-flow-control
set interfaces 1/eth0 unit 0 family ethernet-switching vlan members a
set interfaces 1/eth1 description template
set interfaces 1/eth1 mtu 1514
set interfaces 1/eth1 ether-options flow-control
set interfaces 1/eth1 unit 0 family ethernet-switching interface-mode trunk
set interfaces 1/eth1 unit 0 family ethernet-switching vlan members [ b c ]
`)
	if err != nil {
		t.Fatal(err)
	}
	dst := tr.Root.Entry("interfaces", "1/eth0")
	dst.MergeDefaults(tr.Root.Entry("interfaces", "1/eth1"))
	if dst.Leaf("mtu") != "9216" {
		t.Error("explicit leaf must win")
	}
	if dst.Leaf("description") != "template" {
		t.Error("missing leaf must be filled in")
	}
	if dst.Has("ether-options", "flow-control") {
		t.Error("mutually exclusive sibling must not be merged")
	}
	es := dst.Get("unit", "0", "family", "ethernet-switching")
	if es.Leaf("interface-mode") != "trunk" || strings.Join(es.List("vlan", "members"), ",") != "a" {
		t.Errorf("nested merge wrong:\n%s", FormatNode(dst))
	}
}
