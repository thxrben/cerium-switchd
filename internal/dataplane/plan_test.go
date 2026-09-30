package dataplane

import (
	"fmt"
	"math/rand/v2"
	"mclag/internal/schema"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mclag/internal/model"
)

// effective returns what a link lets through: VLAN membership, PVID,
// untagged egress and tagged ingress per VLAN. A link that is down, not in
// the (up) bridge, or in a bond that is down carries nothing.
func effective(s *State, n string) map[string]bool {
	out := map[string]bool{}
	l := s.Links[n]
	if l == nil || !l.Present || !l.Up {
		return out
	}
	src := l
	if l.Kind == Physical && l.Master != "" && l.Master != BridgeName {
		b := s.Links[l.Master]
		if b == nil || !b.Up {
			return out
		}
		src = b
	}
	br := s.Links[BridgeName]
	if src.Master != BridgeName || br == nil || !br.Up {
		return out
	}
	for vid, f := range src.VLANs {
		out[fmt.Sprintf("member%d", vid)] = true
		if f.PVID {
			out[fmt.Sprintf("pvid%d", vid)] = true
		}
		if f.Untagged {
			out[fmt.Sprintf("untagged%d", vid)] = true
		}
		if !src.DropTagged {
			out[fmt.Sprintf("tagged-in%d", vid)] = true
		}
	}
	return out
}

func subset(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// testNames maps "<m>/0/<n>" to "eth<n>" (every name resolves; whether the
// port is present is up to the kernel state, like hardware that is not
// plugged in).
func testNames(name string) (string, bool) {
	p, ok := schema.ParsePhysical(name)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("eth%d", p.Port), true
}

var testPorts = []string{"eth0", "eth1", "eth2", "eth3", "eth4", "eth5", "eth7"} // eth7 is not plugged in

func baseKernel(r *rand.Rand) *State {
	s := &State{Links: map[string]*Link{}}
	present := append(slices.Clone(testPorts[:6]), "eth6") // eth7 absent; eth6 present, never configured
	for _, p := range present {
		s.Links[p] = &Link{Name: p, Kind: Physical, Up: r.IntN(2) == 0, MTU: 1500, MaxMTU: 9500, Present: true}
	}
	s.Links["virbr0"] = &Link{Name: "virbr0", Kind: Other, Up: true, MTU: 1500, Present: true}
	return s
}

func pick[T any](r *rand.Rand, xs ...T) T { return xs[r.IntN(len(xs))] }

func randSwitching(r *rand.Rand, i *model.Interface) {
	switch r.IntN(3) {
	case 0: // not switched
	case 1:
		i.Switching, i.Mode = true, "access"
		i.AccessVLAN = pick(r, 0, 10, 20, 30)
		if i.AccessVLAN != 0 {
			i.VLANs = []int{i.AccessVLAN}
		}
	case 2:
		i.Switching, i.Mode = true, "trunk"
		for _, v := range []int{10, 20, 30, 40} {
			if r.IntN(2) == 0 {
				i.VLANs = append(i.VLANs, v)
			}
		}
		if len(i.VLANs) > 0 && r.IntN(2) == 0 {
			i.NativeVLAN = pick(r, i.VLANs...)
		}
	}
}

func randConfig(r *rand.Rand) *model.Config {
	cfg := &model.Config{Interfaces: map[string]*model.Interface{}}
	aes := map[string]*model.Interface{}
	for _, p := range testPorts {
		role := r.IntN(6)
		if role == 0 {
			continue // not configured
		}
		i := &model.Interface{Name: "1/0/" + strings.TrimPrefix(p, "eth"), Member: 1, MTU: pick(r, 1514, 9014),
			Disabled: r.IntN(6) == 0, Description: pick(r, "", "a", "b"), MACLimit: pick(r, 0, 100, 200),
			StormControl: model.StormControl{Broadcast: pick(r, 0, 50, 500), Multicast: pick(r, 0, 100)}}
		if r.IntN(3) == 0 {
			fc := r.IntN(2) == 0
			i.FlowControl = &fc
		}
		switch role {
		case 1: // plain
		case 2, 3:
			randSwitching(r, i)
			if !i.Switching {
				i.Switching, i.Mode, i.AccessVLAN, i.VLANs = true, "access", 10, []int{10}
			}
		case 4, 5:
			ae := pick(r, "ae1", "ae2")
			i.Parent = ae
			if aes[ae] == nil {
				a := &model.Interface{Name: ae, AE: true, MTU: pick(r, 1514, 9014), MemberIDs: []int{1},
					HashPolicy: pick(r, "", "layer2", "layer3+4"), MinLinks: pick(r, 1, 2), Disabled: r.IntN(8) == 0}
				randSwitching(r, a)
				aes[ae] = a
			}
			aes[ae].MemberPorts = append(aes[ae].MemberPorts, i.Name)
		}
		cfg.Interfaces[i.Name] = i
	}
	for _, a := range aes {
		cfg.Interfaces[a.Name] = a
	}
	return cfg
}

func names(s *State) map[string]bool {
	m := map[string]bool{}
	for n := range s.Links {
		m[n] = true
	}
	return m
}

func mentions(ops []Op, link string) []string {
	var out []string
	for _, o := range ops {
		if o.Link == link {
			out = append(out, o.String())
		}
	}
	return out
}

// checkConverged verifies that the kernel matches desired for every
// present link.
func checkConverged(t *testing.T, k *Fake, desired *State) {
	t.Helper()
	for n, d := range desired.Links {
		a := k.S.Links[n]
		if a == nil || !a.Present {
			if d.Kind == Bond {
				t.Fatalf("bond %s missing", n)
			}
			continue
		}
		if a.Up != d.Up || a.MTU != d.MTU || a.Master != d.Master || a.Alias != d.Alias ||
			a.DropTagged != d.DropTagged || a.MaxLearned != d.MaxLearned ||
			a.StormBroadcast != d.StormBroadcast || a.StormMulticast != d.StormMulticast ||
			(d.Master == BridgeName && !reflect.DeepEqual(vlanSet(a.VLANs), vlanSet(d.VLANs))) ||
			(d.FlowControl != nil && (a.FlowControl == nil || *a.FlowControl != *d.FlowControl)) ||
			(d.Kind == Bond && *a.Bond != *d.Bond) {
			t.Fatalf("%s not converged:\nkernel  %s\ndesired %s", n, a, d)
		}
	}
}

func TestPlanProperties(t *testing.T) {
	iterations := 3000
	if testing.Short() {
		iterations = 300
	}
	for seed := 0; seed < iterations; seed++ {
		r := rand.New(rand.NewPCG(uint64(seed), 7))
		k := NewFake(baseKernel(r))

		// Converge to configuration A.
		a, _ := Compute(randConfig(r), 1, testNames)
		cur, _ := k.Read()
		if err := Execute(k, Plan(cur, a, nil)); err != nil {
			t.Fatalf("seed %d: apply A: %v", seed, err)
		}
		checkConverged(t, k, a)
		cur, _ = k.Read()
		if again := Plan(cur, a, names(a)); len(again) > 0 {
			t.Fatalf("seed %d: plan not idempotent:\n%s", seed, FormatPlan(again))
		}

		// Change to configuration B, checking every intermediate state.
		b, _ := Compute(randConfig(r), 1, testNames)
		before, _ := k.Read()
		ops := Plan(before, b, names(a))
		final := NewFake(before)
		if err := Execute(final, ops); err != nil {
			t.Fatalf("seed %d: apply B: %v\nplan:\n%s", seed, err, FormatPlan(ops))
		}
		all := map[string]bool{}
		for n := range before.Links {
			all[n] = true
		}
		for n := range final.S.Links {
			all[n] = true
		}
		oldEff, newEff := map[string]map[string]bool{}, map[string]map[string]bool{}
		for n := range all {
			oldEff[n], newEff[n] = effective(before, n), effective(final.S, n)
		}
		for i, op := range ops {
			if err := k.Apply(op); err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
			for n := range all {
				e := effective(k.S, n)
				if !subset(e, oldEff[n]) && !subset(e, newEff[n]) {
					t.Fatalf("seed %d: after step %d (%s) %s mixes old and new permissions\nold %v\nnew %v\nnow %v\nplan:\n%s",
						seed, i+1, op, n, oldEff[n], newEff[n], e, FormatPlan(ops))
				}
			}
		}
		checkConverged(t, k, b)
		cur, _ = k.Read()
		if again := Plan(cur, b, names(b)); len(again) > 0 {
			t.Fatalf("seed %d: plan B not idempotent:\n%s", seed, FormatPlan(again))
		}

		// Unchanged links are not touched; foreign devices never.
		for n, la := range a.Links {
			if lb := b.Links[n]; lb != nil && reflect.DeepEqual(la, lb) {
				if m := mentions(ops, n); len(m) > 0 {
					t.Fatalf("seed %d: unchanged %s touched: %v", seed, n, m)
				}
			}
		}
		for _, n := range []string{"eth6", "virbr0"} {
			if m := mentions(ops, n); len(m) > 0 {
				t.Fatalf("seed %d: foreign %s touched: %v", seed, n, m)
			}
		}
		// Released links are down and out of the bridge/bundles.
		for n := range a.Links {
			if b.Links[n] == nil {
				if l := k.S.Links[n]; l != nil && l.Present && (l.Up || l.Master != "") {
					t.Fatalf("seed %d: released %s still %s", seed, n, l)
				}
			}
		}
	}
}

func TestPlanExamples(t *testing.T) {
	access := func(vid int) *model.Interface {
		return &model.Interface{Name: "1/0/0", Member: 1, MTU: 1514, Switching: true, Mode: "access", AccessVLAN: vid, VLANs: []int{vid}}
	}
	cfg := func(is ...*model.Interface) *model.Config {
		c := &model.Config{Interfaces: map[string]*model.Interface{}}
		for _, i := range is {
			c.Interfaces[i.Name] = i
		}
		return c
	}
	k := NewFake(&State{Links: map[string]*Link{"eth0": {Name: "eth0", Kind: Physical, MTU: 1500, Present: true}}})
	a, _ := Compute(cfg(access(10)), 1, testNames)
	cur, _ := k.Read()
	ops := Plan(cur, a, nil)
	want := []string{
		"drop-tagged eth0 true",
		"create-bridge swbr0 {AgeingSeconds:300}",
		"up swbr0",
		"master eth0 \"swbr0\"",
		"vlan-set eth0 10 pvid=true untagged=true",
		"up eth0",
	}
	if got := strings.Split(strings.TrimSpace(FormatPlan(ops)), "\n"); !slices.Equal(got, want) {
		t.Fatalf("initial plan:\n%s", FormatPlan(ops))
	}
	Execute(k, ops)

	// Access VLAN 10 -> 20: remove first, then add (never both).
	b, _ := Compute(cfg(access(20)), 1, testNames)
	cur, _ = k.Read()
	ops = Plan(cur, b, names(a))
	want = []string{"vlan-del eth0 10", "vlan-set eth0 20 pvid=true untagged=true"}
	if got := strings.Split(strings.TrimSpace(FormatPlan(ops)), "\n"); !slices.Equal(got, want) {
		t.Fatalf("vlan change plan:\n%s", FormatPlan(ops))
	}

	// Only the description changes: one alias op, no flap.
	c := access(10)
	c.Description = "server"
	d, _ := Compute(cfg(c), 1, testNames)
	cur, _ = k.Read()
	Execute(k, Plan(cur, a, names(a)))
	cur, _ = k.Read()
	if ops := Plan(cur, d, names(a)); len(ops) != 1 || ops[0].Kind != OpSetAlias {
		t.Fatalf("description change:\n%s", FormatPlan(ops))
	}
}

func TestComputeNotes(t *testing.T) {
	cfg := &model.Config{Interfaces: map[string]*model.Interface{
		"ae1":   {Name: "ae1", AE: true, MTU: 9014, MemberIDs: []int{1}, LACP: &model.LACP{Active: true}},
		"1/0/0": {Name: "1/0/0", Member: 1, MTU: 1514, Parent: "ae1"},
		"1/0/1": {Name: "1/0/1", Member: 1, MTU: 1514, Parent: "ae9"},
		"2/0/0": {Name: "2/0/0", Member: 2, MTU: 1514},
	}}
	s, notes := Compute(cfg, 1, testNames)
	// An LACP bundle is a team device; its ports are enabled by LACP.
	if !s.Links["ae1"].Up || !s.Links["ae1"].Bond.Team() || s.Links["eth0"].MTU != 9000 || s.Links["eth0"].Master != "ae1" {
		t.Errorf("ae1/eth0: %s %s", s.Links["ae1"], s.Links["eth0"])
	}
	if s.Links["eth1"].Up {
		t.Error("port of an unconfigured bundle is up")
	}
	if len(s.Links) != 3 {
		t.Errorf("member 2 ports leaked into member 1: %v", names(s))
	}
	if len(notes) != 1 {
		t.Errorf("notes: %v", notes)
	}
}

// Enabling LACP on a static bundle replaces the bond by a team device; the
// ports go down, the bond is deleted, the team is created and the ports
// join it (a planned change, the partner has to renegotiate anyway).
func TestPlanStaticToLACP(t *testing.T) {
	ae := func(lacp bool) *model.Config {
		c := &model.Config{Interfaces: map[string]*model.Interface{
			"ae1":   {Name: "ae1", AE: true, MTU: 1514, MemberIDs: []int{1}, Switching: true, Mode: "access", AccessVLAN: 10, VLANs: []int{10}},
			"1/0/0": {Name: "1/0/0", Member: 1, MTU: 1514, Parent: "ae1"},
		}}
		if lacp {
			c.Interfaces["ae1"].LACP = &model.LACP{Active: true, Fast: true}
		}
		return c
	}
	k := NewFake(&State{Links: map[string]*Link{"eth0": {Name: "eth0", Kind: Physical, MTU: 1500, Present: true, Up: true}}})
	static, _ := Compute(ae(false), 1, testNames)
	ops := Plan(mustRead(t, k), static, nil)
	if err := Execute(k, ops); err != nil {
		t.Fatalf("static bundle (a port that is up must go down to join):\n%s%v", FormatPlan(ops), err)
	}
	lacp, _ := Compute(ae(true), 1, testNames)
	ops = Plan(mustRead(t, k), lacp, names(static))
	if err := Execute(k, ops); err != nil {
		t.Fatalf("static -> LACP:\n%s%v", FormatPlan(ops), err)
	}
	after := mustRead(t, k)
	if !after.Links["ae1"].Bond.Team() || after.Links["eth0"].Master != "ae1" || !after.Links["eth0"].Up || after.Links["ae1"].Master != BridgeName {
		t.Errorf("after static -> LACP:\n%s\nae1 %v eth0 %v", FormatPlan(ops), after.Links["ae1"], after.Links["eth0"])
	}
	if ops := Plan(mustRead(t, k), lacp, names(lacp)); len(ops) != 0 {
		t.Errorf("not idempotent:\n%s", FormatPlan(ops))
	}
}

// A witness has no data plane; a switch that becomes one releases its
// ports and the bridge (reference 5.2, role witness).
func TestComputeWitness(t *testing.T) {
	sw := &model.Config{Interfaces: map[string]*model.Interface{
		"1/0/0": {Name: "1/0/0", Member: 1, MTU: 1514, Switching: true, Mode: "access", AccessVLAN: 10, VLANs: []int{10}},
	}}
	a, _ := Compute(sw, 1, testNames)
	k := NewFake(&State{Links: map[string]*Link{"eth0": {Name: "eth0", Kind: Physical, MTU: 1500, Present: true}}})
	Execute(k, Plan(mustRead(t, k), a, nil))
	w := &model.Config{Members: map[int]*model.Member{1: {ID: 1, Witness: true}}}
	s, _ := Compute(w, 1, testNames)
	if s.Bridge != nil || len(s.Links) != 0 {
		t.Fatalf("witness desired state: %+v", s)
	}
	Execute(k, Plan(mustRead(t, k), s, names(a)))
	after := mustRead(t, k)
	// The ports are released (the empty bridge device may stay).
	if after.Links["eth0"] == nil || after.Links["eth0"].Master != "" || after.Links["eth0"].Up {
		t.Errorf("after becoming a witness: eth0 %v", after.Links["eth0"])
	}
}

func mustRead(t *testing.T, k Kernel) *State {
	t.Helper()
	s, err := k.Read()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Learning is off on a peer-link; a port whose learning mac-limit switched
// off is left alone, other ports get it back.
func TestPlanLearning(t *testing.T) {
	k := NewFake(&State{Bridge: &BridgeOpts{AgeingSeconds: 300}, Links: map[string]*Link{
		"eth0": {Name: "eth0", Kind: Physical, MTU: 1500, Present: true, Up: true, Master: BridgeName, VLANs: map[uint16]VlanFlags{}, NoLearning: true, MaxLearned: 3},
		"eth1": {Name: "eth1", Kind: Physical, MTU: 1500, Present: true, Up: true, Master: BridgeName, VLANs: map[uint16]VlanFlags{}, NoLearning: true},
	}})
	d := mustRead(t, k).Clone()
	d.L3 = &L3{}
	d.Links["eth0"].NoLearning = false // mac-limit decides
	d.Links["eth1"].NoLearning = false
	ops := Plan(mustRead(t, k), d, names(d))
	if len(ops) != 1 || ops[0].Kind != OpSetLearning || ops[0].Link != "eth1" || !ops[0].Bool {
		t.Fatalf("plan:\n%s", FormatPlan(ops))
	}
	d.Links["eth1"].NoLearning = true // becomes a peer-link port
	Execute(k, ops)
	ops = Plan(mustRead(t, k), d, names(d))
	if len(ops) != 1 || ops[0].Kind != OpSetLearning || ops[0].Bool {
		t.Fatalf("peer-link plan:\n%s", FormatPlan(ops))
	}
}

// Stack tunnels (reference 5.2): one per other switch member, carrying the
// VLANs both ends have, isolated; the tunnel to the MC-LAG peer does not
// learn. The planner creates them down, enslaves, isolates, then brings
// them up; a changed endpoint recreates the device.
func TestComputeTunnels(t *testing.T) {
	cfg := &model.Config{
		Members: map[int]*model.Member{1: {ID: 1}, 2: {ID: 2}, 3: {ID: 3}, 4: {ID: 4, Witness: true}},
		Interfaces: map[string]*model.Interface{
			"1/0/0": {Name: "1/0/0", Member: 1, MTU: 9014, Switching: true, Mode: "trunk", VLANs: []int{10, 20, 30}},
			"2/0/0": {Name: "2/0/0", Member: 2, MTU: 1514, Switching: true, Mode: "access", AccessVLAN: 20, VLANs: []int{20}},
			"3/0/0": {Name: "3/0/0", Member: 3, MTU: 1514, Switching: true, Mode: "trunk", VLANs: []int{30, 40}},
			"ae1": {Name: "ae1", AE: true, MTU: 1514, MCLAG: true, MemberIDs: []int{1, 2}, Switching: true, Mode: "access",
				AccessVLAN: 10, VLANs: []int{10}, LACP: &model.LACP{Active: true}},
		},
		Domains: map[int]*model.Domain{1: {ID: 1, Members: []int{1, 2}}},
	}
	s, _ := Compute(cfg, 1, testNames)
	t2, t3 := s.Links["swvc2"], s.Links["swvc3"]
	if t2 == nil || t3 == nil || s.Links["swvc4"] != nil || s.Links["swvc1"] != nil {
		t.Fatalf("tunnels: %v", names(s))
	}
	if !reflect.DeepEqual(t2.VLANs, map[uint16]VlanFlags{10: {}, 20: {}}) || !reflect.DeepEqual(t3.VLANs, map[uint16]VlanFlags{30: {}}) {
		t.Errorf("tunnel VLANs: 2 %v, 3 %v", t2.VLANs, t3.VLANs)
	}
	if !t2.NoLearning || t3.NoLearning || !t2.Isolated || !t3.Isolated || t2.MTU != 9000 {
		t.Errorf("tunnel flags: %+v %+v", t2, t3)
	}
	if t2.Tunnel.VNI != 34 || t2.Tunnel.Local.String() != "169.254.64.1" || t2.Tunnel.Remote.String() != "169.254.64.2" {
		t.Errorf("tunnel 2: %+v", *t2.Tunnel)
	}

	k := NewFake(&State{Links: map[string]*Link{"eth0": {Name: "eth0", Kind: Physical, MTU: 1500, Present: true}}})
	ops := Plan(mustRead(t, k), s, nil)
	if err := Execute(k, ops); err != nil {
		t.Fatalf("%s%v", FormatPlan(ops), err)
	}
	plan := FormatPlan(ops)
	iso, up := strings.Index(plan, "isolated swvc2 true"), strings.Index(plan, "up swvc2")
	if iso < 0 || up < iso || strings.Index(plan, "create-tunnel swvc2") > iso {
		t.Errorf("tunnel order:\n%s", plan)
	}
	if ops := Plan(mustRead(t, k), s, names(s)); len(ops) != 0 {
		t.Errorf("not idempotent:\n%s", FormatPlan(ops))
	}
	// Member 3 leaves: its tunnel goes.
	delete(cfg.Members, 3)
	delete(cfg.Interfaces, "3/0/0")
	s2, _ := Compute(cfg, 1, testNames)
	ops = Plan(mustRead(t, k), s2, names(s))
	if err := Execute(k, ops); err != nil || mustRead(t, k).Links["swvc3"] != nil {
		t.Errorf("removing a tunnel:\n%s%v", FormatPlan(ops), err)
	}
	// A standalone switch has none.
	s3, _ := Compute(&model.Config{Interfaces: cfg.Interfaces}, 1, testNames)
	for n := range s3.Links {
		if TunnelMember(n) > 0 {
			t.Errorf("standalone tunnel %s", n)
		}
	}
}
