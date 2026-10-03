//go:build linux

package lacp

import (
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"
)

type orderKernel struct{ events *[]string }

func (k orderKernel) SetPort(bundle, port string, on bool) error {
	state := "off"
	if on {
		state = "on"
	}
	*k.events = append(*k.events, "port "+port+" "+state)
	return nil
}

func (orderKernel) PortsEnabled(string) (map[string]bool, error) { return map[string]bool{}, nil }

// An MC-LAG leg must be announced to the peer before its first port
// carries traffic (reference 5.6: the peer filters first), and only then.
func TestBeforeJoinRunsBeforeTheFirstPort(t *testing.T) {
	s := newSim()
	a := s.add("A", Config{System: sys(1), Key: 1, Active: true, Fast: true}, "p1", "p2")
	s.add("B", Config{System: sys(2), Key: 9, Active: true, Fast: true}, "p1", "p2")
	s.connect("A/p1", "B/p1")
	s.connect("A/p2", "B/p2")
	var events []string
	r := &Runtime{Kernel: orderKernel{&events}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		BeforeJoin: func(b string) { events = append(events, "join "+b) }}
	rb := &rtBundle{spec: BundleSpec{Name: "ae1"}, b: a, enabled: map[string]bool{}}
	r.enforce(rb) // nothing distributing yet
	if len(events) != 0 {
		t.Fatalf("before LACP converged: %v", events)
	}
	s.run(4 * time.Second)
	r.enforce(rb)
	if want := []string{"join ae1", "port p1 on", "port p2 on"}; !slices.Equal(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}
	// Already forwarding: no second announcement.
	events = nil
	r.enforce(rb)
	if len(events) != 0 {
		t.Fatalf("steady state: %v", events)
	}
	// The leg leaves (held) and comes back: announced again.
	a.SetHold(true)
	s.run(4 * time.Second)
	r.enforce(rb)
	a.SetHold(false)
	s.run(4 * time.Second)
	events = nil
	r.enforce(rb)
	if len(events) == 0 || events[0] != "join ae1" {
		t.Fatalf("rejoin: %v", events)
	}
}
