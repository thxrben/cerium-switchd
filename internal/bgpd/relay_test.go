package bgpd

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/ribd"
	"github.com/thxrben/cerium-switchd/pkg/bgp"
)

// fakeStack delivers stack calls between daemons (JSON as on the wire).
type fakeStack struct {
	mu sync.Mutex
	ds map[int]*Daemon
}

func (s *fakeStack) call(ctx context.Context, from int) func(context.Context, int, string, any, any) error {
	return func(ctx context.Context, member int, method string, req, resp any) error {
		s.mu.Lock()
		d := s.ds[member]
		s.mu.Unlock()
		if d == nil {
			return errors.New("not reachable")
		}
		raw, _ := json.Marshal(req)
		var out any
		var err error
		switch method {
		case StackRelayOpen:
			var o RelayOpen
			json.Unmarshal(raw, &o)
			err = d.RelayOpened(from, o)
		case StackRelayDial:
			var q RelayDial
			json.Unmarshal(raw, &q)
			out, err = d.RelayDialed(ctx, from, q)
		case StackRelayData:
			var m RelayData
			json.Unmarshal(raw, &m)
			err = d.RelayedData(m)
		case StackRelayClose:
			var m RelayClose
			json.Unmarshal(raw, &m)
			d.RelayedClose(m)
		case StackBFDState:
			var m RelayBFDState
			json.Unmarshal(raw, &m)
			d.RelayedBFDState(m)
		}
		if err == nil && resp != nil && out != nil {
			b, _ := json.Marshal(out)
			err = json.Unmarshal(b, resp)
		}
		return err
	}
}

// A neighbour on a routed port of member 2: the master (member 1) runs the
// session; member 2 accepts and dials the TCP connection and relays it.
func TestRelayedSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ports := &sync.Map{}
	m1, m2, router := netip.MustParseAddr("127.0.0.91"), netip.MustParseAddr("127.0.0.92"), netip.MustParseAddr("127.0.0.93")

	// The router knows only member 2's port address.
	rn := loNet{local: router, ports: ports}
	rl, _ := rn.Listen("", nil)
	r := bgp.New(rn.Transport(), bgp.Policy{Export: func(*bgp.Neighbor, *bgp.Path) bool { return true }}, quiet)
	go r.Run(ctx)
	r.Configure(bgp.Config{AS: 65002, RouterID: router, ConnectRetry: 200 * time.Millisecond, Neighbors: []bgp.Neighbor{{
		Addr: m2, PeerAS: 65001, LocalAS: 65002, HoldTime: 9, Families: []bgp.Family{bgp.IPv4Unicast}}}})
	go func() {
		for {
			c, err := rl.Accept()
			if err != nil {
				return
			}
			r.Accept(c)
		}
	}()
	r.SetLocal([]bgp.Path{{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Source: "static"}}, nil)

	st := &fakeStack{ds: map[int]*Daemon{}}
	rib1 := &fakeRIB{sets: map[string]ribd.SetRoutes{}}
	d1 := New(loNet{local: m1, ports: ports}, rib1, quiet)
	d2 := New(loNet{local: m2, ports: ports}, &fakeRIB{sets: map[string]ribd.SetRoutes{}}, quiet)
	d1.Member, d1.StackCall = 1, st.call(ctx, 1)
	d2.Member, d2.StackCall = 2, st.call(ctx, 2)
	b1, b2 := &fakeBFD{}, &fakeBFD{}
	d1.BFD, d2.BFD = b1, b2
	st.ds[1], st.ds[2] = d1, d2
	cfg := Config{Instances: []Instance{{AS: 65001, RouterID: m1, Neighbors: []Neighbor{{Owner: 2,
		Neighbor: bgp.Neighbor{Addr: router, PeerAS: 65002, LocalAS: 65001, HoldTime: 9, Families: []bgp.Family{bgp.IPv4Unicast}},
		BFDCfg:   &BFDConfig{IntervalMs: 300, Multiplier: 3}}}}}}
	for _, d := range []*Daemon{d1, d2} {
		go d.Run(ctx)
		d.SetConfig(cfg)
	}
	d2.SetRole(false, 1)
	d1.SetRole(true, 1)

	waitFor(t, "relayed session established", func() bool {
		s := d1.Status(nil)
		return len(s) == 1 && s[0].Neighbors[0].State == "Established"
	})
	waitFor(t, "the router's route at the master", func() bool { return len(rib1.routes(router.String())) == 1 })
	if n := d1.Status(nil)[0].Neighbors[0]; n.Local != m2 {
		t.Fatalf("the session's local address %v, want member 2's port %v", n.Local, m2)
	}
	if len(d2.Status(nil)) != 0 {
		t.Fatal("member 2 runs BGP itself")
	}
	// BFD runs on the owner, not on the master; its failure ends the
	// master's session.
	waitFor(t, "BFD on the owner", func() bool { return len(b2.get()) == 1 })
	if len(b1.get()) != 0 {
		t.Fatalf("the master runs BFD for a neighbour behind member 2: %+v", b1.get())
	}
	key := b2.get()[0].Key.String()
	d2.BFDChanged(key, true, false)
	d2.BFDChanged(key, false, false)
	waitFor(t, "down by the owner's BFD", func() bool {
		n := d1.Status(nil)[0].Neighbors[0]
		return n.State != "Established" && n.LastError == "BFD session down"
	})
	d2.BFDChanged(key, true, false)
	waitFor(t, "back", func() bool { return d1.Status(nil)[0].Neighbors[0].State == "Established" })
	// A soft clear sends everything again: the session stays.
	d1.Clear(ClearRequest{Neighbor: router, Mode: bgp.ClearSoft})
	time.Sleep(300 * time.Millisecond)
	if n := d1.Status(nil)[0].Neighbors[0]; n.State != "Established" {
		t.Fatalf("after a soft clear: %+v", n)
	}
}
