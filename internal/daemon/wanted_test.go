package daemon

import (
	"testing"

	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/model"
)

func TestWantedDaemons(t *testing.T) {
	build := func(set string) *model.Config {
		tr, err := config.ParseSet(set)
		if err != nil {
			t.Fatal(err)
		}
		cfg, issues := model.Build(tr, nil)
		if issues.HasErrors() {
			t.Fatal(issues)
		}
		return cfg
	}
	base := `
set vlans v10 vlan-id 10
set vlans v10 l3-interface irb.10
set interfaces irb unit 10 family inet address 10.1.0.1/24
set routing-options router-id 10.1.0.1
`
	if w := wantedBy(build(base)); len(w) != 0 {
		t.Errorf("without protocols: %v", w)
	}
	w := wantedBy(build(base + "set protocols ospf area 0 interface irb.10\n"))
	if !w["cer-ospfd"] || w["cer-bfdd"] || w["cer-bgpd"] {
		t.Errorf("ospf: %v", w)
	}
	w = wantedBy(build(base + "set protocols ospf area 0 interface irb.10 bfd-liveness-detection minimum-interval 100\n"))
	if !w["cer-ospfd"] || !w["cer-bfdd"] {
		t.Errorf("ospf with bfd: %v", w)
	}
}
