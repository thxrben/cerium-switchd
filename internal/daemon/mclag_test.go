package daemon

import (
	"slices"
	"testing"
	"time"

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

func TestSplitHorizon(t *testing.T) {
	now := time.Unix(1000, 0)
	b := []string{"ae1"}
	cases := []struct {
		name            string
		in              splitInput
		split, drain, d []string
	}{
		{"peer unknown", splitInput{Bundles: b}, b, nil, nil},
		{"peer leg up", splitInput{Bundles: b, PeerKnown: true, PeerLegs: map[string]bool{"ae1": true}, Reachable: true}, b, nil, b},
		{"peer leg up, primary", splitInput{Bundles: b, PeerKnown: true, PeerLegs: map[string]bool{"ae1": true}, Primary: true, Reachable: true}, b, nil, nil},
		{"peer leg down", splitInput{Bundles: b, PeerKnown: true, PeerLegs: map[string]bool{"ae1": false}}, nil, nil, nil},
		{"peer leg not reported", splitInput{Bundles: b, PeerKnown: true, PeerLegs: map[string]bool{}}, b, nil, nil},
		// The peer announced its leg before forwarding: filtered at once,
		// even if an older message still says down.
		{"peer leg joining", splitInput{Bundles: b, PeerKnown: true, PeerLegs: map[string]bool{"ae1": false},
			Joining: map[string]time.Time{"ae1": now.Add(-time.Second)}}, b, nil, nil},
		{"join announcement expired", splitInput{Bundles: b, PeerKnown: true, PeerLegs: map[string]bool{"ae1": false},
			Joining: map[string]time.Time{"ae1": now.Add(-mclagJoinGrace)}}, nil, nil, nil},
		// Maintenance drain: the peer's unicast passes, its flooded frames
		// (BPDUs among them) do not go back to the partner.
		{"peer leg draining", splitInput{Bundles: b, PeerKnown: true, PeerLegs: map[string]bool{"ae1": false},
			Draining: map[string]bool{"ae1": true}}, nil, b, nil},
	}
	for _, c := range cases {
		split, drain, df := c.in.compute(now)
		if !slices.Equal(split, c.split) || !slices.Equal(drain, c.drain) || !slices.Equal(df, c.d) {
			t.Errorf("%s: split %v drain %v df %v, want %v %v %v", c.name, split, drain, df, c.split, c.drain, c.d)
		}
	}
}
