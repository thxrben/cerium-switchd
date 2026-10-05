package dataplane

import (
	"reflect"
	"testing"

	"github.com/thxrben/cerium-switchd/lib/conf/model"
)

func TestComputeMirrors(t *testing.T) {
	cfg := &model.Config{
		Interfaces: map[string]*model.Interface{
			"1/0/1": {Name: "1/0/1", Member: 1, Switching: true, Mode: "access", AccessVLAN: 10},
			"1/0/2": {Name: "1/0/2", Member: 1, Switching: true, Mode: "trunk", VLANs: []int{10, 20}, NativeVLAN: 20},
			"1/0/3": {Name: "1/0/3", Member: 1}, // output (plain)
			"2/0/1": {Name: "2/0/1", Member: 2, Switching: true, Mode: "access", AccessVLAN: 10},
			"ae1":   {Name: "ae1", AE: true, MemberIDs: []int{1, 2}, Switching: true, Mode: "trunk", VLANs: []int{10}},
		},
		Analyzers: map[string]*model.Analyzer{
			"a": {Name: "a", IngressIfs: []string{"1/0/1"}, EgressIfs: []string{"ae1"}, Output: "1/0/3"},
			"v": {Name: "v", IngressVLANs: []int{10, 20}, Output: "1/0/3"},
		},
	}
	got := ComputeMirrors(cfg, 1, testNames)
	want := map[string]*MirrorPort{
		"eth1": {Ingress: []string{"eth3"}, Untagged: []string{"eth3"}},
		"eth2": {Tagged: map[int][]string{10: {"eth3"}}, Untagged: []string{"eth3"}},
		"ae1":  {Egress: []string{"eth3"}, Tagged: map[int][]string{10: {"eth3"}}},
	}
	if !reflect.DeepEqual(got, want) {
		for k, v := range got {
			t.Logf("%s: %+v", k, *v)
		}
		t.Errorf("mirrors differ")
	}
	// On member 2 nothing: the output is on member 1.
	if m := ComputeMirrors(cfg, 2, testNames); len(m) != 0 {
		t.Errorf("member 2: %v", m)
	}
}
