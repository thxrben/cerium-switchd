package daemon

import (
	"slices"
	"testing"

	"github.com/thxrben/cerium-switchd/lib/conf/model"
)

func TestSTPConfig(t *testing.T) {
	yes := true
	cfg := &model.Config{
		Members: map[int]*model.Member{1: {}, 2: {}},
		RSTP: &model.RSTP{BridgePriority: 4096, HelloTime: 2, MaxAge: 20, ForwardDelay: 15, Ports: map[string]*model.RSTPPort{
			"1/0/1": {Name: "1/0/1", Edge: true, Priority: 64, PointToPnt: &yes},
			"1/0/9": {Name: "1/0/9", Disabled: true},
		}},
		Interfaces: map[string]*model.Interface{
			"1/0/1": {Name: "1/0/1", Member: 1, Switching: true},
			"1/0/9": {Name: "1/0/9", Member: 1, Switching: true},
			"2/0/1": {Name: "2/0/1", Member: 2, Switching: true},
			"1/0/5": {Name: "1/0/5", Member: 1, Parent: "ae1"},
			"2/0/5": {Name: "2/0/5", Member: 2, Parent: "ae1"},
			"ae1":   {Name: "ae1", AE: true, MemberIDs: []int{1, 2}, Switching: true, LACP: &model.LACP{Active: true}},
			"1/0/7": {Name: "1/0/7", Member: 1}, // routed: not an RSTP port
		},
	}
	linux := func(n string) (string, bool) {
		m := map[string]string{"1/0/1": "eth1", "1/0/5": "eth5", "1/0/9": "eth9"}
		l, ok := m[n]
		return l, ok
	}
	c := stpConfig(cfg, 1, linux, "stack-1")
	if !c.On || c.Bridge.BridgePriority != 4096 || c.StackID != "stack-1" {
		t.Fatalf("bridge %+v", c)
	}
	var names []string
	for n := range c.Ports {
		names = append(names, n)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"1/0/1", "2/0/1", "ae1"}) {
		t.Fatalf("ports %v", names)
	}
	p := c.Ports["1/0/1"]
	if p.Device != "eth1" || p.Config == nil || !p.Config.Edge || p.Config.Priority != 64 || !*p.Config.PointToPnt {
		t.Errorf("1/0/1 %+v", p)
	}
	if p := c.Ports["2/0/1"]; p.Device != "" || !slices.Equal(p.Members, []int{2}) {
		t.Errorf("2/0/1 (another member's) %+v", p)
	}
	if p := c.Ports["ae1"]; p.Device != "ae1" || !p.AE || !p.LACP || !slices.Equal(p.Legs, []string{"eth5"}) || !slices.Equal(p.Members, []int{1, 2}) {
		t.Errorf("ae1 %+v", p)
	}
	if c := stpConfig(&model.Config{}, 1, linux, "x"); c.On || len(c.Ports) != 0 {
		t.Errorf("without rstp %+v", c)
	}
}
