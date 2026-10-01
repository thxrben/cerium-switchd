package rstp

import (
	"fmt"
	"testing"
)

// net wires bridges: a BPDU sent on a port arrives on the port it is cabled
// to, through the wire format.
type net struct {
	t       *testing.T
	bridges map[string]*Bridge
	cable   map[string]string // "A/1" -> "B/2"
	cut     map[string]bool
	queue   []delivery
	flushes map[string]int
	states  map[string][2]bool
}

type delivery struct {
	to   string
	data []byte
}

func newNet(t *testing.T) *net {
	return &net{t: t, bridges: map[string]*Bridge{}, cable: map[string]string{}, cut: map[string]bool{},
		flushes: map[string]int{}, states: map[string][2]bool{}}
}

func key(b string, port uint16) string { return fmt.Sprintf("%s/%d", b, port) }

func (n *net) add(name string, prio uint16, last byte) *Bridge {
	var br *Bridge //lint:ignore S1021 the callbacks below refer to br
	br = New(BridgeConfig{ID: MakeBridgeID(prio, [6]byte{2, 0, 0, 0, 0, last}), HelloTime: 2, MaxAge: 20, ForwardDelay: 15}, Callbacks{
		Send: func(port uint16, b *BPDU) {
			from := key(name, port)
			if to, ok := n.cable[from]; ok && !n.cut[from] {
				n.queue = append(n.queue, delivery{to, b.Frame([6]byte{2, 0, 0, 0, 1, last})})
			}
		},
		State: func(port uint16, l, f bool) { n.states[key(name, port)] = [2]bool{l, f} },
		Flush: func(port uint16) { n.flushes[key(name, port)]++ },
	})
	n.bridges[name] = br
	return br
}

func p2p() PortConfig { return PortConfig{Priority: 128, P2P: true, SpeedMbps: 10000, AutoEdge: true} }

// link cables a/pa to b/pb and brings both ends up.
func (n *net) link(a string, pa uint16, b string, pb uint16) {
	n.cable[key(a, pa)], n.cable[key(b, pb)] = key(b, pb), key(a, pa)
	for _, e := range []struct {
		br string
		p  uint16
	}{{a, pa}, {b, pb}} {
		if _, ok := n.bridges[e.br].ports[e.p]; !ok {
			n.bridges[e.br].AddPort(e.p, p2p())
		}
		n.bridges[e.br].SetEnabled(e.p, true)
	}
	n.deliver()
}

func (n *net) down(a string, pa uint16) {
	other := n.cable[key(a, pa)]
	delete(n.cable, key(a, pa))
	delete(n.cable, other)
	n.bridges[a].SetEnabled(pa, false)
	var ob string
	var op uint16
	fmt.Sscanf(other, "%1s/%d", &ob, &op)
	n.bridges[ob].SetEnabled(op, false)
	n.deliver()
}

func (n *net) deliver() {
	for i := 0; len(n.queue) > 0; i++ {
		if i > 100000 {
			n.t.Fatal("BPDU storm")
		}
		d := n.queue[0]
		n.queue = n.queue[1:]
		var br string
		var port uint16
		fmt.Sscanf(d.to, "%1s/%d", &br, &port)
		bpdu, err := ParseFrame(d.data)
		if err != nil || bpdu == nil {
			n.t.Fatalf("bad frame to %s: %v", d.to, err)
		}
		n.bridges[br].Receive(port, bpdu)
	}
}

func (n *net) run(seconds int) {
	for range seconds {
		for _, name := range []string{"A", "B", "C", "D"} {
			if b := n.bridges[name]; b != nil {
				b.Tick()
			}
		}
		n.deliver()
	}
}

func (n *net) port(b string, p uint16) PortStatus {
	for _, s := range n.bridges[b].Ports() {
		if s.Number == p {
			return s
		}
	}
	n.t.Fatalf("no port %s/%d", b, p)
	return PortStatus{}
}

func (n *net) want(b string, p uint16, role Role, fwd bool) {
	n.t.Helper()
	s := n.port(b, p)
	if s.Role != role || s.Forwarding != fwd {
		n.t.Errorf("%s/%d: %v forwarding %v, want %v forwarding %v", b, p, s.Role, s.Forwarding, role, fwd)
	}
	if st := n.states[key(b, p)]; st[1] != s.Forwarding {
		n.t.Errorf("%s/%d: kernel state %v, port says forwarding %v", b, p, st, s.Forwarding)
	}
}

func TestBPDURoundTrip(t *testing.T) {
	in := &BPDU{Version: 2, Type: TypeRST, Flags: flagProposal | encDesig<<roleShift | flagLearning,
		Priority: Vector{Root: MakeBridgeID(4096, [6]byte{1, 2, 3, 4, 5, 6}), Cost: 2000, Bridge: MakeBridgeID(32768, [6]byte{2}), Port: MakePortID(128, 5)},
		Times:    Times{MessageAge: 1, MaxAge: 20, HelloTime: 2, ForwardDelay: 15}}
	out, err := ParseFrame(in.Frame([6]byte{2, 0, 0, 0, 0, 9}))
	if err != nil || out == nil {
		t.Fatal(err)
	}
	if *out != *in {
		t.Errorf("round trip: %+v != %+v", out, in)
	}
	if _, err := Unmarshal([]byte{0, 0, 2, 2, 0}); err == nil {
		t.Error("short RST BPDU accepted")
	}
}

// Two bridges: the lower id is root; the link forwards at once
// (proposal/agreement), without waiting for timers.
func TestTwoBridgesRapid(t *testing.T) {
	n := newNet(t)
	n.add("A", 32768, 1)
	n.add("B", 32768, 2)
	n.link("A", 1, "B", 1)
	n.want("A", 1, RoleDesignated, true)
	n.want("B", 1, RoleRoot, true)
	if v, rp, _ := n.bridges["B"].Root(); v.Root != n.bridges["A"].cfg.ID || rp == 0 {
		t.Errorf("B's root %v via %v", v.Root, rp)
	}
}

// A triangle: exactly one port blocks, on the bridge farthest from the root.
func TestTriangle(t *testing.T) {
	n := newNet(t)
	n.add("A", 4096, 1)
	n.add("B", 32768, 2)
	n.add("C", 32768, 3)
	n.link("A", 1, "B", 1)
	n.link("B", 2, "C", 2)
	n.link("C", 1, "A", 2)
	n.run(1)
	n.want("A", 1, RoleDesignated, true)
	n.want("A", 2, RoleDesignated, true)
	n.want("B", 1, RoleRoot, true)
	n.want("C", 1, RoleRoot, true)
	// B-C: B has the lower id, so B's port is designated, C's alternate.
	n.want("B", 2, RoleDesignated, true)
	n.want("C", 2, RoleAlternate, false)

	// C's root port fails: the alternate takes over at once.
	n.down("C", 1)
	n.want("C", 2, RoleRoot, true)
	n.want("B", 2, RoleDesignated, true)
	if n.flushes["B/2"] == 0 && n.flushes["B/1"] == 0 {
		t.Error("no topology change flush after the failure")
	}
}

// A cable between two ports of the same bridge: one end is a backup port.
func TestSelfLoop(t *testing.T) {
	n := newNet(t)
	n.add("A", 32768, 1)
	n.link("A", 1, "A", 2)
	n.run(1)
	n.want("A", 1, RoleDesignated, true)
	n.want("A", 2, RoleBackup, false)
}

// An edge port forwards at once; a BPDU makes it a normal port.
func TestEdgePort(t *testing.T) {
	n := newNet(t)
	a := n.add("A", 32768, 1)
	a.AddPort(1, PortConfig{Priority: 128, P2P: true, SpeedMbps: 1000, AdminEdge: true})
	a.SetEnabled(1, true)
	n.want("A", 1, RoleDesignated, true)
	if !n.port("A", 1).OperEdge {
		t.Error("not oper-edge")
	}
	// A bridge (better id) appears on it.
	n.add("B", 4096, 2)
	n.bridges["B"].AddPort(1, p2p())
	n.cable["A/1"], n.cable["B/1"] = "B/1", "A/1"
	n.bridges["B"].SetEnabled(1, true)
	n.deliver()
	n.run(1)
	if n.port("A", 1).OperEdge {
		t.Error("still oper-edge after a BPDU")
	}
	n.want("A", 1, RoleRoot, true)
}

// Root guard: a superior BPDU on a guarded port blocks it.
func TestRootGuard(t *testing.T) {
	n := newNet(t)
	a := n.add("A", 32768, 1)
	n.add("B", 4096, 2)
	cfg := p2p()
	cfg.RootGuard = true
	a.AddPort(1, cfg)
	n.link("A", 1, "B", 1)
	n.run(1)
	s := n.port("A", 1)
	if s.Role != RoleAlternate || s.Forwarding || !s.RootInconsistent {
		t.Errorf("guarded port: %+v", s)
	}
	if v, _, _ := a.Root(); v.Root != a.cfg.ID {
		t.Errorf("A accepted %v as root through a guarded port", v.Root)
	}
}

// A neighbour that speaks 802.1D only: the port falls back to STP, and as a
// designated port (no agreement from an 802.1D bridge) it forwards only
// after twice the forward delay.
func TestSTPNeighbour(t *testing.T) {
	n := newNet(t)
	a := n.add("A", 32768, 1)
	a.AddPort(1, p2p())
	a.SetEnabled(1, true)
	stp := &BPDU{Type: TypeConfig, Priority: Vector{Root: MakeBridgeID(61440, [6]byte{9}), Bridge: MakeBridgeID(61440, [6]byte{9}), Port: MakePortID(128, 1)},
		Times: Times{MaxAge: 20, HelloTime: 2, ForwardDelay: 15}}
	var sent []*BPDU
	a.cb.Send = func(port uint16, b *BPDU) { sent = append(sent, b) }
	for i := 0; i < 40; i++ {
		f := stp.Frame([6]byte{9})
		b, _ := ParseFrame(f)
		a.Receive(1, b)
		a.Tick()
		if i == 25 {
			if a.ports[1].SendRSTP {
				t.Fatal("still sending RSTP to an 802.1D neighbour")
			}
			if a.ports[1].Forwarding {
				t.Fatal("forwarding before the forward delay")
			}
		}
	}
	s := n.port("A", 1)
	if s.Role != RoleDesignated || !s.Forwarding || s.RSTP {
		t.Errorf("port towards the STP neighbour: %+v", s)
	}
	for _, b := range sent[len(sent)-3:] {
		if b.Type == TypeRST {
			t.Errorf("RST BPDU sent to the STP neighbour: %+v", b)
		}
	}
}

// Snapshot and restore: a second instance continues where the first one
// stopped, without a port leaving the forwarding state.
func TestRestoreKeepsStates(t *testing.T) {
	n := newNet(t)
	n.add("A", 4096, 1)
	n.add("B", 32768, 2)
	n.add("C", 32768, 3)
	n.link("A", 1, "B", 1)
	n.link("B", 2, "C", 2)
	n.link("C", 1, "A", 2)
	n.run(3)
	snap := n.bridges["C"].Snapshot()
	old := n.bridges["C"]
	// A new owner of bridge C: same id, same ports, state from the snapshot.
	var changes []string
	c2 := New(old.cfg, Callbacks{
		Send: old.cb.Send,
		State: func(port uint16, l, f bool) {
			if prev := n.states[key("C", port)]; prev != [2]bool{l, f} {
				changes = append(changes, fmt.Sprintf("%d: %v -> %v", port, prev, [2]bool{l, f}))
			}
			n.states[key("C", port)] = [2]bool{l, f}
		},
		Flush: old.cb.Flush,
	})
	cfgs := map[uint16]PortConfig{}
	for _, num := range old.Numbers() {
		cfgs[num] = old.ports[num].Config
	}
	c2.Restore(snap, cfgs)
	n.bridges["C"] = c2
	if len(changes) > 0 {
		t.Errorf("state changes on restore: %v", changes)
	}
	n.run(30)
	n.want("C", 1, RoleRoot, true)
	n.want("C", 2, RoleAlternate, false)
	n.want("A", 2, RoleDesignated, true)
}

// A ring of four: one port blocks; when a ring link fails, the ring is a
// chain and everything forwards again within a few seconds.
func TestRingFailure(t *testing.T) {
	n := newNet(t)
	n.add("A", 4096, 1)
	n.add("B", 32768, 2)
	n.add("C", 32768, 3)
	n.add("D", 32768, 4)
	n.link("A", 1, "B", 1)
	n.link("B", 2, "C", 1)
	n.link("C", 2, "D", 2)
	n.link("D", 1, "A", 2)
	n.run(2)
	blocked := 0
	for _, b := range []string{"A", "B", "C", "D"} {
		for _, p := range n.bridges[b].Ports() {
			if !p.Forwarding {
				blocked++
			}
		}
	}
	if blocked != 1 {
		t.Fatalf("%d ports blocked in the ring, want 1", blocked)
	}
	n.want("C", 2, RoleAlternate, false) // C-D: C's port 2 (via B and via D equal cost; D has id 4)
	n.down("A", 1)
	n.run(3)
	for _, b := range []string{"B", "C", "D"} {
		for _, p := range n.bridges[b].Ports() {
			if p.Enabled && !p.Forwarding {
				t.Errorf("%s/%d still %v, not forwarding after the ring failure", b, p.Number, p.Role)
			}
		}
	}
	if n.port("B", 2).Role != RoleRoot {
		t.Errorf("B's root port is %v", n.port("B", 2).Role)
	}
}
