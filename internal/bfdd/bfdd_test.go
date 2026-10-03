package bfdd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/bfd"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

// memNet delivers a server's packets to the server that owns the peer
// address.
type memNet struct {
	mu      sync.Mutex
	servers map[netip.Addr]*bfd.Server
}

type memT struct {
	n    *memNet
	self netip.Addr
}

func (t memT) Send(k bfd.Key, b []byte) error {
	t.n.mu.Lock()
	dst := t.n.servers[k.Peer]
	t.n.mu.Unlock()
	if dst != nil {
		go dst.Input(bfd.Input{Instance: k.Instance, From: t.self, To: k.Peer, TTL: 255, Raw: bytes.Clone(b)})
	}
	return nil
}
func (memT) Open(string, bool) error { return nil }
func (memT) Close(string, bool)      {}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func side(t *testing.T, n *memNet, addr string) (*Daemon, *ipc.Endpoint) {
	a := netip.MustParseAddr(addr)
	sv := bfd.NewServer(memT{n, a}, quiet)
	n.mu.Lock()
	n.servers[a] = sv
	n.mu.Unlock()
	go sv.Run()
	t.Cleanup(sv.Stop)
	ep := ipc.NewEndpoint("cer-bfdd", "test", quiet)
	return New(sv, ep, quiet), ep
}

func waitState(t *testing.T, ep *ipc.Endpoint, key string, f func(raw json.RawMessage) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f(ep.Get(TopicSessions, key)) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s", key, ep.Get(TopicSessions, key))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func up(raw json.RawMessage) bool {
	var s State
	return json.Unmarshal(raw, &s) == nil && s.Up
}

func TestSessionsOfClients(t *testing.T) {
	n := &memNet{servers: map[netip.Addr]*bfd.Server{}}
	a, epA := side(t, n, "10.0.0.1")
	b, _ := side(t, n, "10.0.0.2")
	toB := bfd.Key{Peer: netip.MustParseAddr("10.0.0.2")}
	toA := bfd.Key{Peer: netip.MustParseAddr("10.0.0.1")}
	if err := a.Set(Set{Client: "ospf", Sessions: []SessionSpec{{Key: toB, Interface: "irb.10", IntervalMs: 50, Multiplier: 3}}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Set(Set{Client: "ospf", Sessions: []SessionSpec{{Key: toA, IntervalMs: 50, Multiplier: 3}}}); err != nil {
		t.Fatal(err)
	}
	waitState(t, epA, toB.String(), up)

	// A second client shares the session; the first leaving keeps it up.
	if err := a.Set(Set{Client: "bgp", Sessions: []SessionSpec{{Key: toB, IntervalMs: 100, Multiplier: 3}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Set(Set{Client: "ospf"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !up(epA.Get(TopicSessions, toB.String())) {
		t.Fatal("the session went down while BGP still uses it")
	}
	// The neighbour removes its session: down here (not a failure there).
	if err := b.Set(Set{Client: "ospf"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, epA, toB.String(), func(raw json.RawMessage) bool { return raw != nil && !up(raw) })
	// The last client removes it: no state left.
	if err := a.Set(Set{Client: "bgp"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, epA, toB.String(), func(raw json.RawMessage) bool { return raw == nil })

	if err := a.Set(Set{Client: "x", Sessions: []SessionSpec{{Key: toB, IntervalMs: 50, Multiplier: 3, AuthType: "nope"}}}); err == nil {
		t.Fatal("unknown authentication accepted")
	}
}
