package schema

import "testing"

func walk(n *Node, f func(*Node)) {
	f(n)
	for _, c := range n.Children {
		walk(c, f)
	}
}

func TestSchemaWellFormed(t *testing.T) {
	walk(Root(), func(n *Node) {
		if n == Root() {
			return
		}
		if n.Name == "" {
			t.Errorf("unnamed node below %q", n.Parent().Path())
		}
		if n.Help == "" {
			t.Errorf("%s: missing help", n.Path())
		}
		switch n.Kind {
		case Leaf, LeafList, List:
			if n.Type == nil {
				t.Errorf("%s: %s without type", n.Path(), n.Kind)
			}
		}
		if n.Presence && n.Kind != Container {
			t.Errorf("%s: presence on non-container", n.Path())
		}
		seen := map[string]bool{}
		for _, c := range n.Children {
			if seen[c.Name] {
				t.Errorf("%s: duplicate child %q", n.Path(), c.Name)
			}
			seen[c.Name] = true
			if c.Parent() != n {
				t.Errorf("%s: bad parent link", c.Path())
			}
		}
	})
}

func TestTypes(t *testing.T) {
	cases := []struct {
		typ  *Type
		in   string
		want string
		ok   bool
	}{
		{VlanID, "10", "10", true},
		{VlanID, "0", "", false},
		{VlanID, "4095", "", false},
		{VlanID, "abc", "", false},
		{MTU, "9216", "9216", true},
		{MTU, "16001", "", false},
		{Interface, "1/0/0", "1/0/0", true},
		{Interface, "16/3/1", "16/3/1", true},
		{Interface, "17/0/0", "", false},
		{Interface, "0/0/0", "", false},
		{Interface, "ae0", "ae0", true},
		{Interface, "ae01", "", false},
		{Interface, "eth0", "", false},
		{Interface, "1/averyveryverylongname", "", false},
		{PhysInterface, "ae1", "", false},
		{AEInterface, "ae12", "ae12", true},
		{VlanRef, "10-20", "10-20", true},
		{VlanRef, "20-10", "", false},
		{VlanRef, "users", "users", true},
		{VlanRef, "all", "all", true},
		{VlanRef, "1x", "", false},
		{IPv4, "10.0.0.1", "10.0.0.1", true},
		{IPv4, "::1", "", false},
		{IP, "fe80::1%eth0", "", false},
		{IP, "2001:DB8::1", "2001:db8::1", true},
		{IPPrefix, "10.0.0.1/24", "10.0.0.1/24", true},
		{IPPrefix, "10.0.0.1", "", false},
		{MAC, "02:00:00:AA:bb:cc", "02:00:00:aa:bb:cc", true},
		{MAC, "02:00:00:aa:bb", "", false},
		{Hostname, "sw-a.example.org", "sw-a.example.org", true},
		{Hostname, "-bad", "", false},
		{Host, "10.1.1.1", "10.1.1.1", true},
		{Host, "log.example", "log.example", true},
		{UintStep("<p>", 0, 61440, 4096), "8192", "8192", true},
		{UintStep("<p>", 0, 61440, 4096), "8000", "", false},
		{Enum(E("access", ""), E("trunk", "")), "tr", "trunk", true},
		{Enum(E("access", ""), E("trunk", "")), "x", "", false},
		{Enum(E("9600", ""), E("115200", "")), "11", "", false},
		{Text, "hello world", "hello world", true},
		{Text, "bad\nline", "", false},
		{Text, "esc\x1b[2J", "", false},
		{Text, "c1\u009b", "", false},
		{Text, "bad\xff", "", false},
		{Text, "Grüße", "Grüße", true},
	}
	for _, c := range cases {
		got, err := c.typ.Validate(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%s.Validate(%q) = %q, %v; want %q ok=%v", c.typ.Name, c.in, got, err, c.want, c.ok)
		}
	}
}

func TestLookupPrefix(t *testing.T) {
	r := Root()
	if m := r.Lookup("int"); len(m) != 1 || m[0].Name != "interfaces" {
		t.Fatalf("Lookup(int) = %v", m)
	}
	if m := r.Lookup("interface-"); len(m) != 1 || m[0].Name != "interface-range" {
		t.Fatalf("Lookup(interface-) = %v", m)
	}
	if m := r.Lookup("s"); len(m) < 2 {
		t.Fatalf("Lookup(s) should be ambiguous, got %d", len(m))
	}
	if m := r.Lookup("nothing"); len(m) != 0 {
		t.Fatalf("Lookup(nothing) = %v", m)
	}
}

func TestPortNames(t *testing.T) {
	for s, want := range map[string]string{"1/0/0": "1/0/0", "16/99/999": "16/99/999", "01/002/3": "1/2/3"} {
		if p, ok := ParsePhysical(s); !ok || p.String() != want {
			t.Errorf("ParsePhysical(%q) = %v %v", s, p, ok)
		}
	}
	for _, s := range []string{"0/0/0", "17/0/0", "1/0", "1/eth0", "1/0/-1", "1/100/0", "1/0/1000", "1/+1/0", "1/0/0/0", "ae0"} {
		if _, ok := ParsePhysical(s); ok {
			t.Errorf("ParsePhysical(%q) accepted", s)
		}
	}
	pp, err := ParsePortPattern("*/1/[2-4]")
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[Port]bool{{1, 1, 2}: true, {16, 1, 4}: true, {1, 1, 5}: false, {1, 0, 2}: false} {
		if pp.Match(p) != want {
			t.Errorf("match %v = %v", p, !want)
		}
	}
	for _, s := range []string{"1/eth*", "1/*", "1/[3-1]/0", "1/[0-x]/0", "*/*/*/*", "0/0/0", "1/0/[0-1000]"} {
		if _, err := ParsePortPattern(s); err == nil {
			t.Errorf("ParsePortPattern(%q) accepted", s)
		}
	}
}
