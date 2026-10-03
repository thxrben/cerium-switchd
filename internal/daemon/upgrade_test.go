package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/config"
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

var labNames = fakeNames{"ens18": "1/0/0", "ens19": "1/1/0", "ens21": "1/2/0", "ens22": "1/3/0", "ens23": "1/4/0"}

func upgrade(t *testing.T, old string) *config.Tree {
	t.Helper()
	raw := newUpgrader(labNames, 1, nil).Upgrade(json.RawMessage(old))
	tr, err := config.FromJSON(raw)
	if err != nil {
		t.Fatalf("upgraded configuration does not parse: %v\n%s", err, raw)
	}
	return tr
}

func TestUpgradeNames(t *testing.T) {
	tr := upgrade(t, `{
  "interfaces": {
    "1/ens19": {"unit": {"0": {"family": {"ethernet-switching": {"vlan": {"members": ["v10"]}}}}}},
    "1/ens21": {"ether-options": {"802.3ad": "ae1"}, "@inactive": true},
    "2/eth0": {"disable": true},
    "1/gone0": {"disable": true},
    "ae1": {"mtu": "9014"}
  },
  "interface-range": {"r": {"member": ["1/ens2*", "1/0/*", "1/nomatch*"], "member-range": {"1/ens22": {"to": "1/ens23"}}}},
  "vlans": {"v10": {"vlan-id": "10"}},
  "forwarding-options": {"analyzer": {"a": {"input": {"ingress": {"interface": ["1/ens19", "1/gone1"]}}, "output": {"interface": "1/ens23"}}}}
}`)
	out := config.FormatSet(tr)
	for _, want := range []string{"set interfaces 1/1/0 unit 0", "deactivate interfaces 1/2/0", "set interfaces 1/2/0 ether-options 802.3ad ae1",
		"member-range 1/3/0 to 1/4/0", "r member 1/2/0\n", "r member 1/3/0\n", "r member 1/4/0\n", "r member 1/0/*\n", "ingress interface 1/1/0", "output interface 1/4/0"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Ports that cannot be converted are dropped (logged), never left behind
	// in a form that makes the configuration unreadable.
	for _, gone := range []string{"eth0", "gone", "nomatch"} {
		if strings.Contains(out, gone) {
			t.Errorf("%s left in:\n%s", gone, out)
		}
	}
	// New-style input is unchanged.
	if tr := upgrade(t, `{"interfaces":{"1/1/0":{"disable":true}}}`); !tr.Root.Has("interfaces", "1/1/0", "disable") {
		t.Error("new-style name changed")
	}
}

func TestUpgradeStack(t *testing.T) {
	tr := upgrade(t, `{"stack": {"bfd": {"multiplier": "5"},
	  "member": {"1": {"priority": "200", "host-name": "a"}, "2": {"@inactive:priority": true, "priority": "10"}}},
	  "protocols": {"rstp": {"interface": {"ae1": {"priority": "32"}}}}}`)
	r := tr.Root
	if r.Leaf("virtual-chassis", "member", "1", "mastership-priority") != "200" || r.Leaf("virtual-chassis", "bfd", "multiplier") != "5" ||
		!r.Get("virtual-chassis", "member", "2", "mastership-priority").Inactive || r.Leaf("protocols", "rstp", "interface", "ae1", "priority") != "32" {
		t.Errorf("converted:\n%s", config.FormatSet(tr))
	}
}

// Management blocks become routed interfaces in routing instance
// mgmt_ceros (reference 5.9).
func TestUpgradeManagement(t *testing.T) {
	// A dedicated port (bare Linux name, the oldest form).
	tr := upgrade(t, `{"stack": {"member": {"1": {"management": {"interface": "ens18",
	  "address": ["10.5.176.95/16", "fd00::5/64"], "gateway": ["10.5.0.1", "fd00::1"]}}}}}`)
	out := config.FormatSet(tr)
	for _, want := range []string{
		"set interfaces 1/0/0 unit 0 family inet address 10.5.176.95/16",
		"set interfaces 1/0/0 unit 0 family inet6 address fd00::5/64",
		"set routing-instances mgmt_ceros interface 1/0/0.0",
		"set routing-instances mgmt_ceros routing-options static route 0.0.0.0/0 next-hop 10.5.0.1",
		"set routing-instances mgmt_ceros routing-options static route ::/0 next-hop fd00::1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "set system management-instance mgmt_ceros") {
		t.Errorf("a dedicated management port does not stay the management instance:\n%s", out)
	}
	if strings.Contains(out, "management {") || tr.Root.Has("virtual-chassis", "member", "1", "management") {
		t.Errorf("management block left:\n%s", out)
	}

	// A management VLAN on two members, by name, with an existing address
	// list elsewhere (the old leaf-list form).
	tr = upgrade(t, `{"vlans": {"mgmt": {"vlan-id": "99"}},
	  "interfaces": {"1/ens23": {"unit": {"0": {"family": {"inet": {"address": ["10.9.0.1/30"]}}}}}},
	  "stack": {"member": {
	    "1": {"management": {"vlan": "mgmt", "address": ["10.5.176.95/16"], "gateway": ["10.5.0.1"]}},
	    "2": {"management": {"vlan": "99", "address": ["10.5.176.96/16"], "gateway": ["10.5.0.1"]}}}}}`)
	out = config.FormatSet(tr)
	for _, want := range []string{
		"set vlans mgmt l3-interface irb.99",
		"set interfaces irb unit 99 family inet address 10.5.176.95/16 member 1",
		"set interfaces irb unit 99 family inet address 10.5.176.96/16 member 2",
		"set routing-instances mgmt_ceros interface irb.99",
		"set routing-instances mgmt_ceros routing-options static route 0.0.0.0/0 next-hop 10.5.0.1",
		"set interfaces 1/4/0 unit 0 family inet address 10.9.0.1/30",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "management-instance") {
		t.Errorf("per-member management addresses kept as the management instance:\n%s", out)
	}
	if n := strings.Count(out, "mgmt_ceros interface irb.99"); n != 1 {
		t.Errorf("irb.99 listed %d times:\n%s", n, out)
	}
}

// The peer-link of earlier versions and its bundle are removed; the MC-LAG
// bundles stay (without their domain, which the ports imply now).
func TestUpgradePeerLink(t *testing.T) {
	tr := upgrade(t, `{
  "interfaces": {
    "1/2/0": {"ether-options": {"802.3ad": "ae10"}},
    "2/2/0": {"ether-options": {"802.3ad": "ae10", "flow-control": true}},
    "1/3/0": {"ether-options": {"802.3ad": "ae1"}},
    "ae10": {"mtu": "9216", "aggregated-ether-options": {"lacp": {"active": true}}},
    "ae1": {"aggregated-ether-options": {"lacp": {"active": true}, "mclag": {}}}
  },
  "mclag": {"domain": {"1": {"members": ["1", "2"], "peer-link": "ae10", "peer-link-bfd": {"multiplier": "3"}, "heartbeat": {"multiplier": "3"}}}}
}`)
	out := config.FormatSet(tr)
	for _, gone := range []string{"ae10", "peer-link", "heartbeat"} {
		if strings.Contains(out, gone) {
			t.Errorf("%s left in:\n%s", gone, out)
		}
	}
	for _, want := range []string{"set interfaces 2/2/0 ether-options flow-control", "set interfaces 1/3/0 ether-options 802.3ad ae1",
		"set interfaces ae1 aggregated-ether-options lacp active"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "set interfaces 1/2/0") {
		t.Errorf("empty former peer-link port left:\n%s", out)
	}
}

func TestUpgradeRenamesManagementInstance(t *testing.T) {
	tr := upgrade(t, `{"system":{"management-instance":true},"routing-instances":{"mgmt_junos":{"interface":["irb.99"]}},"vlans":{"m":{"vlan-id":"99","l3-interface":"irb.99"}},"interfaces":{"irb":{"unit":{"99":{"family":{"inet":{"address":{"10.0.0.2/24":{}}}}}}}}}`)
	out := config.FormatSet(tr)
	if !strings.Contains(out, "set routing-instances mgmt_ceros interface irb.99") || strings.Contains(out, "mgmt_junos") || !strings.Contains(out, "set system management-instance mgmt_ceros") {
		t.Errorf("management instance not renamed:\n%s", out)
	}
}

// A configuration from a newer version: what this version does not know
// is left out, the rest applies (reference 3.6, mixed versions).
func TestUpgradeIgnoresUnknownStatements(t *testing.T) {
	raw := `{"system":{"host-name":"core","future-thing":{"x":"1"},"syslog":{"host":{"10.0.0.1":{"transport":"quantum"}}}},
	  "protocols":{"lldp":{"advertisement-interval":"30","new-knob":true}},
	  "vlans":{"v10":{"vlan-id":"10","shiny":"yes"}},
	  "switch-options":{"vxlan":{"flood-mode":"x"}},
	  "interfaces":{"1/0/0":{"mtu":"99999999"}}}`
	got := UnknownStatements([]byte(raw))
	want := []string{"interfaces 1/0/0 mtu 99999999", "protocols lldp new-knob", "switch-options vxlan flood-mode", "system future-thing", "system syslog host 10.0.0.1 transport quantum", "vlans v10 shiny"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("unknown: %q", got)
	}
	tr := upgrade(t, raw)
	out := config.FormatSet(tr)
	for _, want := range []string{"set system host-name core", "set protocols lldp advertisement-interval 30", "set vlans v10 vlan-id 10"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// MC-LAG domains of earlier versions: delay-restore stays (stack-wide),
// the rest of the domains and the bundles' mclag flag go.
func TestUpgradeMCLAGDomains(t *testing.T) {
	tr := upgrade(t, `{
  "interfaces": {
    "1/3/0": {"ether-options": {"802.3ad": "ae1"}},
    "2/3/0": {"ether-options": {"802.3ad": "ae1"}},
    "ae1": {"aggregated-ether-options": {"lacp": {"active": true}, "mclag": {}}},
    "ae2": {"aggregated-ether-options": {"mclag": {}}}
  },
  "interface-range": {"r": {"member-range": {"1/4/0": {"to": "1/4/3"}}, "aggregated-ether-options": {"mclag": {}}}},
  "mclag": {"domain": {"2": {"members": ["1", "2"], "delay-restore": "60", "system-mac": "02:00:00:00:00:01", "anycast-vtep": "10.0.0.1"}}}
}`)
	out := config.FormatSet(tr)
	for _, gone := range []string{"domain", "aggregated-ether-options mclag", "system-mac", "anycast-vtep"} {
		if strings.Contains(out, gone) {
			t.Errorf("%s left in:\n%s", gone, out)
		}
	}
	for _, want := range []string{"set mclag delay-restore 60", "set interfaces ae1 aggregated-ether-options lacp active"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
