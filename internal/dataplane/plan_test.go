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
	if s.Links["ae1"].Up || s.Links["eth0"].MTU != 9000 || s.Links["eth0"].Master != "ae1" {
		t.Errorf("ae1/eth0: %s %s", s.Links["ae1"], s.Links["eth0"])
	}
	if s.Links["eth1"].Up {
		t.Error("port of an unconfigured bundle is up")
	}
	if len(s.Links) != 3 {
		t.Errorf("member 2 ports leaked into member 1: %v", names(s))
	}
	if len(notes) != 2 {
		t.Errorf("notes: %v", notes)
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
