package daemon

import (
	"testing"

	"github.com/thxrben/cerium-switchd/internal/model"
)

func TestBundleFacts(t *testing.T) {
	cfg := &model.Config{Interfaces: map[string]*model.Interface{
		"ae10": {Name: "ae10", AE: true, MTU: 9216, LACP: &model.LACP{Active: true, Fast: true},
			Switching: true, Mode: "trunk", VLANs: []int{20, 10}, NativeVLAN: 10},
		"ae11": {Name: "ae11", AE: true, Switching: true, Mode: "access", AccessVLAN: 5},
	}}
	for name, want := range map[string]string{
		"ae10": "trunk vlans [10 20] native 10, mtu 9216, lacp active,fast",
		"ae11": "access vlan 5, mtu 0, lacp static",
		"ae12": "",
	} {
		if got := bundleFacts(cfg, name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if cfg.Interfaces["ae10"].VLANs[0] != 20 {
		t.Error("bundleFacts sorted the model's VLAN list in place")
	}
}
