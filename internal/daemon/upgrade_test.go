package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"mclag/internal/config"
)

type fakeNames map[string]string // linux -> name

func (f fakeNames) Name(l string) (string, bool) { n, ok := f[l]; return n, ok }
func (f fakeNames) LinuxNames() []string {
	var out []string
	for l := range f {
		out = append(out, l)
	}
	return out
}

func TestUpgradeNames(t *testing.T) {
	old := `{
  "interfaces": {
    "1/ens19": {"unit": {"0": {"family": {"ethernet-switching": {"vlan": {"members": ["v10"]}}}}}},
    "1/ens21": {"ether-options": {"802.3ad": "ae1"}, "@inactive": true},
    "2/eth0": {"disable": true},
    "1/gone0": {"disable": true},
    "ae1": {"mtu": "9014"}
  },
  "interface-range": {"r": {"member": ["1/ens2*", "1/0/*", "1/nomatch*"], "member-range": {"1/ens22": {"to": "1/ens23"}}}},
  "vlans": {"v10": {"vlan-id": "10"}},
  "stack": {"member": {"1": {"management": {"interface": "ens18", "address": ["10.0.0.1/24"]}},
                       "2": {"management": {"interface": "eth9"}}}},
  "forwarding-options": {"analyzer": {"a": {"input": {"ingress": {"interface": ["1/ens19"]}}, "output": {"interface": "1/ens23"}}}}
}`
	up := upgradeNames(fakeNames{"ens18": "1/0/0", "ens19": "1/1/0", "ens21": "1/2/0", "ens22": "1/3/0", "ens23": "1/4/0"}, 1)
	raw := up(json.RawMessage(old))
	tr, err := config.FromJSON(raw)
	if err == nil {
		t.Fatalf("names of other members and missing ports must stay (and fail parsing): %s", raw)
	}
	for _, want := range []string{`"1/1/0"`, `"1/2/0"`, `"1/3/0"`, `"interface":"1/0/0"`, `"interface":"eth9"`, `"2/eth0"`, `"1/gone0"`, `"to":"1/4/0"`, `"1/0/*"`, `"1/nomatch*"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %s in %s", want, raw)
		}
	}
	// A wildcard becomes the ports it matched.
	var m map[string]any
	json.Unmarshal(raw, &m)
	members := m["interface-range"].(map[string]any)["r"].(map[string]any)["member"].([]any)
	got := map[string]bool{}
	for _, v := range members {
		got[v.(string)] = true
	}
	for _, w := range []string{"1/2/0", "1/3/0", "1/4/0", "1/0/*"} {
		if !got[w] {
			t.Errorf("wildcard expansion lacks %s: %v", w, members)
		}
	}
	if got["1/0/0"] || got["1/1/0"] {
		t.Errorf("1/ens2* must not match ens18/ens19: %v", members)
	}

	// A configuration of only this member's existing ports parses fully.
	clean := `{"interfaces": {"1/ens19": {"description": "x"}, "1/ens21": {"@inactive": true, "disable": true}},
	  "stack": {"member": {"1": {"management": {"interface": "ens18"}}}}}`
	tr, err = config.FromJSON(up(json.RawMessage(clean)))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Root.Leaf("interfaces", "1/1/0", "description") != "x" || !tr.Root.Get("interfaces", "1/2/0").Inactive ||
		tr.Root.Leaf("virtual-chassis", "member", "1", "management", "interface") != "1/0/0" {
		t.Errorf("upgraded:\n%s", config.FormatSet(tr))
	}
	// New-style input is unchanged.
	if s := string(up(json.RawMessage(`{"interfaces":{"1/1/0":{"disable":true}}}`))); !strings.Contains(s, `"1/1/0"`) {
		t.Error(s)
	}
}

// The stack hierarchy of older versions becomes virtual-chassis.
func TestUpgradeStack(t *testing.T) {
	up := upgradeNames(fakeNames{}, 1)
	tr, err := config.FromJSON(up(json.RawMessage(`{"stack": {"bfd": {"multiplier": "5"},
	  "member": {"1": {"priority": "200", "host-name": "a"}, "2": {"@inactive:priority": true, "priority": "10"}}},
	  "protocols": {"rstp": {"interface": {"ae1": {"priority": "32"}}}}}`)))
	if err != nil {
		t.Fatal(err)
	}
	r := tr.Root
	if r.Leaf("virtual-chassis", "member", "1", "mastership-priority") != "200" || r.Leaf("virtual-chassis", "bfd", "multiplier") != "5" ||
		!r.Get("virtual-chassis", "member", "2", "mastership-priority").Inactive || r.Leaf("protocols", "rstp", "interface", "ae1", "priority") != "32" {
		t.Errorf("converted:\n%s", config.FormatSet(tr))
	}
}
