package daemon

import (
	"context"
	"time"

	"github.com/thxrben/cerium-switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/internal/mclag"
	"github.com/thxrben/cerium-switchd/internal/svc"
)

// mclagClient is switchd's side of cer-mclagd (reference 1.9): maintenance
// mode as a topic (a restarted daemon learns it again), and the questions
// of maintenance mode and show mclag as calls.
type mclagClient struct{ svc *service }

func (c mclagClient) call(method string, req, resp any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return c.svc.call(ctx, "cer-mclagd", method, req, resp)
}

// SetMaintenance holds (on) or releases this member's legs.
func (c mclagClient) SetMaintenance(on bool, _ time.Time) {
	c.svc.ep.Publish(svc.TopicMaintenance, "", on)
}

// LegsUp lists this member's MC-LAG legs that carry traffic. Without an
// answer it reports that it does not know, so that draining waits.
func (c mclagClient) LegsUp() []string {
	var up []string
	if err := c.call(svc.MethodLegsUp, nil, &up); err != nil {
		return []string{"(unknown: " + err.Error() + ")"}
	}
	return up
}

// DrainBlockers explains why draining would cut traffic; an MC-LAG daemon
// that does not answer blocks too ('force' enters anyway).
func (c mclagClient) DrainBlockers(draining []int) []string {
	var out []string
	if err := c.call(svc.MethodDrainBlockers, draining, &out); err != nil {
		return []string{"MC-LAG state unknown: " + err.Error()}
	}
	return out
}

// Status is show mclag of this member.
func (c mclagClient) Status() ([]cli.MCLAGStatus, error) {
	var st []mclag.Status
	if err := c.call(svc.MethodStatus, nil, &st); err != nil {
		return nil, err
	}
	var out []cli.MCLAGStatus
	for _, s := range st {
		cs := cli.MCLAGStatus{Pair: s.Pair, Member: s.Member, Peer: s.Peer, Primary: s.Primary, PeerReachable: s.PeerReachable,
			PeerKnown: s.PeerKnown, PeerSeen: s.PeerSeen, Reach: s.Reach, Members: s.Members}
		for _, b := range s.Bundles {
			cs.Bundles = append(cs.Bundles, cli.MCLAGBundle{Name: b.Name, LocalUp: b.LocalUp, PeerUp: b.PeerUp, PeerKnown: b.PeerKnown,
				SplitHorizon: b.SplitHorizon, Hold: b.Hold, Facts: b.Facts, PeerFacts: b.PeerFacts, DiffersSince: b.DiffersSince})
		}
		out = append(out, cs)
	}
	return out, nil
}
