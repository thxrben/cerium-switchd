package cli

import (
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/commit"
)

type bfdOps struct{ fakeOps }

func (bfdOps) BFDSessions() ([]BFDSession, error) {
	return []BFDSession{
		{Member: 2, Address: "10.1.1.2", Interface: "2/0/1.0", State: "Up", RemoteState: "Up", Detect: 900 * time.Millisecond,
			Interval: 300 * time.Millisecond, Multiplier: 3, Clients: []string{"ospf-relay"}, Up: time.Hour, Transitions: 1,
			LocalDisc: 7, RemoteDisc: 9, Rx: 100, Tx: 101},
		{Member: 1, Address: "192.0.2.9", Multihop: true, State: "Down", RemoteState: "Down", Diag: "Control Detection Time Expired",
			Detect: 3 * time.Second, Interval: time.Second, Multiplier: 3, Clients: []string{"bgp"}, Transitions: 2},
	}, nil
}

func TestShowBFD(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ts.sh.env.Ops = &bfdOps{}
	contains(t, ts.ok("show bfd session"), "Address", "Multiplier", "10.1.1.2", "Up", "2/0/1.0", "0.900", "0.300",
		"192.0.2.9", "Down", "2 sessions, 2 clients", "Cumulative transmit rate 4.3 pps")
	out := ts.ok("show bfd session extensive")
	contains(t, out, "Client OSPF, member 2", "Session up time 01:00:00", "Local discriminator 7, remote discriminator 9",
		"Client BGP, member 1, multihop", "local diagnostic Control Detection Time Expired")
	contains(t, ts.ok("show bfd session 192.0.2.9"), "1 sessions, 1 clients")
}
