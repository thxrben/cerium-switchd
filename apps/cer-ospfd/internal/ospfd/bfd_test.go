package ospfd

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/lib/ospf"
	"github.com/thxrben/cerium-switchd/lib/platform/api/bfdapi"
	"github.com/thxrben/cerium-switchd/lib/platform/api/ribapi"
	"github.com/thxrben/cerium-switchd/lib/rib"
)

// fakeBFD is a member's cer-bfdd: the clients' last session lists.
type fakeBFD struct {
	mu   sync.Mutex
	sets map[string][]bfdapi.SessionSpec
}

func (f *fakeBFD) Set(_ context.Context, s bfdapi.Set) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sets == nil {
		f.sets = map[string][]bfdapi.SessionSpec{}
	}
	f.sets[s.Client] = s.Sessions
	return nil
}

func (f *fakeBFD) get(client string) ([]bfdapi.SessionSpec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ss, ok := f.sets[client]
	return ss, ok
}

// waitFor polls cond for up to 15 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
	}
}

// nbrState is the state of the neighbour with router id rid ("" none).
func nbrState(d *Daemon, v ospf.Version, rid ospf.ID) string {
	st, err := d.Status(StatusRequest{Version: v})
	if err != nil || len(st) == 0 {
		return ""
	}
	for _, n := range st[0].Neighbors {
		if n.ID == rid {
			return n.State
		}
	}
	return ""
}

// sessions: the peers of a client's sessions (zone included).
func sessions(f *fakeBFD, client string) map[string]bfdapi.SessionSpec {
	ss, _ := f.get(client)
	out := map[string]bfdapi.SessionSpec{}
	for _, s := range ss {
		out[s.Key.Peer.String()] = s
	}
	return out
}

// BFD on the master's own interface: a session per 2-Way neighbour (OSPFv3
// to the link-local address with the device as zone); a session that was
// never up changes nothing, up -> down takes the neighbour down at once.
func TestBFDLocal(t *testing.T) {
	w := &wire{ports: map[string][]*fakePort{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fb := &fakeBFD{}
	mk := func(rid ospf.ID, ll, p4, p6 string, b BFD, spec *BFDSpec) *Daemon {
		k := fakeKernel{"link": {Index: 7, MTU: 1500, Up: true, LinkLocal: netip.MustParseAddr(ll), SpeedMbps: 10000}}
		d := New(k, fakeNet{w: w}, &fakeRIB{sets: map[rib.Protocol]ribapi.SetRoutes{}}, quiet)
		d.BFD = b
		link := iface("1/0/1.0", "link", p4, p6)
		link.BFD = spec
		d.SetConfig(Config{Instances: []Instance{
			{Version: ospf.V2, RouterID: rid, ReferenceBW: 100e9, Interfaces: []Iface{link}},
			{Version: ospf.V3, RouterID: rid, ReferenceBW: 100e9, Interfaces: []Iface{link}},
		}})
		d.SetMaster(true)
		go d.Run(ctx)
		return d
	}
	d1 := mk(0x01010101, "fe80::a", "10.0.0.1/30", "2001:db8::1/64", fb, &BFDSpec{IntervalMs: 50, Multiplier: 3})
	mk(0x02020202, "fe80::b", "10.0.0.2/30", "2001:db8::2/64", nil, nil)
	peer := ospf.ID(0x02020202)
	waitFor(t, "sessions to both neighbours", func() bool { return len(sessions(fb, bfdClient)) == 2 })
	ss := sessions(fb, bfdClient)
	s4, s6 := ss["10.0.0.2"], ss["fe80::b%link"]
	if s4.IntervalMs != 50 || s4.Multiplier != 3 || s4.Interface != "1/0/1.0" || s6.IntervalMs != 50 {
		t.Fatalf("sessions %+v", ss)
	}
	waitFor(t, "full adjacencies", func() bool {
		return nbrState(d1, ospf.V2, peer) == "Full" && nbrState(d1, ospf.V3, peer) == "Full"
	})
	// Down without having been up (the neighbour does not run BFD).
	d1.BFDChanged(s4.Key.String(), false, false)
	time.Sleep(300 * time.Millisecond)
	if nbrState(d1, ospf.V2, peer) != "Full" {
		t.Fatal("a session that was never up must not take the neighbour down")
	}
	d1.BFDChanged(s4.Key.String(), true, false)
	d1.BFDChanged(s4.Key.String(), false, false)
	waitFor(t, "OSPF neighbour down by BFD", func() bool { return nbrState(d1, ospf.V2, peer) != "Full" })
	if nbrState(d1, ospf.V3, peer) != "Full" {
		t.Fatal("the OSPFv3 neighbour has its own session")
	}
	// No BFD configured any more: the sessions end.
	d1.SetConfig(Config{Instances: []Instance{{Version: ospf.V2, RouterID: 0x01010101, ReferenceBW: 100e9,
		Interfaces: []Iface{iface("1/0/1.0", "link", "10.0.0.1/30")}}}})
	waitFor(t, "sessions removed", func() bool { return len(sessions(fb, bfdClient)) == 0 })
}

// BFD on a routed port of member 2: member 2's cer-bfdd runs the sessions
// for the master, and its state changes reach the master.
func TestBFDRelayed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f1, f2 := &fakeBFD{}, &fakeBFD{}
	d1, d2, _, _ := relaySetup(ctx, &BFDSpec{IntervalMs: 100, Multiplier: 5, AuthType: "keyed-sha-1", AuthKeyID: 2, AuthKey: "k"}, f1, f2)
	peer := ospf.ID(0x02020202)
	waitFor(t, "relayed sessions on member 2", func() bool { return len(sessions(f2, bfdRelayClient)) == 2 })
	ss := sessions(f2, bfdRelayClient)
	s4, s6 := ss["10.0.0.2"], ss["fe80::2%link"]
	if s4.Multiplier != 5 || s4.AuthType != "keyed-sha-1" || s4.AuthKeyID != 2 || s6.Interface != "2/0/1.0" {
		t.Fatalf("relayed sessions %+v", ss)
	}
	if local := sessions(f1, bfdClient); len(local) != 0 {
		t.Fatalf("the master runs %v itself", local)
	}
	waitFor(t, "full adjacencies", func() bool {
		return nbrState(d1, ospf.V2, peer) == "Full" && nbrState(d1, ospf.V3, peer) == "Full"
	})
	d2.BFDChanged(s4.Key.String(), true, false)
	d2.BFDChanged(s4.Key.String(), false, false)
	waitFor(t, "OSPF neighbour down by member 2's BFD", func() bool { return nbrState(d1, ospf.V2, peer) != "Full" })
	if nbrState(d1, ospf.V3, peer) != "Full" {
		t.Fatal("the OSPFv3 neighbour has its own session")
	}
	// The adjacency returns; a report says the session failed again, the
	// "up" and "down" before it were lost.
	waitFor(t, "full adjacency again", func() bool { return nbrState(d1, ospf.V2, peer) == "Full" })
	// A new session counts from 0: its first report, then the failure.
	rs := RelayBFDState{Key: "/v2", Unit: "2/0/1.0", Peer: netip.MustParseAddr("10.0.0.2")}
	d1.RelayedBFDState(rs)
	rs.Downs = 1
	d1.RelayedBFDState(rs)
	waitFor(t, "OSPF neighbour down by a counted failure", func() bool { return nbrState(d1, ospf.V2, peer) != "Full" })
	// Member 2 becomes master: it no longer runs sessions for member 1.
	d2.SetRole(true, 2)
	waitFor(t, "relayed sessions removed", func() bool {
		ss, ok := f2.get(bfdRelayClient)
		return ok && len(ss) == 0
	})
}
