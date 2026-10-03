package daemon

import (
	"encoding/json"
	"sync"

	"github.com/thxrben/cerium-switchd/internal/svc"
)

// lacpControl is what MC-LAG needs from LACP (reference 5.6): the legs and
// ready ports of this member, and holding legs out of their bundles.
type lacpControl interface {
	Legs() map[string]bool
	Ready() map[string]int
	SetHold(bundle string, hold bool)
	SetPeerReady(bundle string, n int)
}

// lacpLink is switchd's side of cer-lacpd (reference 1.9): it mirrors the
// daemon's legs, ready ports and port states, and publishes MC-LAG's
// decisions as the control topic the daemon follows (state, not calls: a
// restart of either side cannot lose a hold).
type lacpLink struct {
	svc               *service
	legs, ready, port *mirror

	mu      sync.Mutex
	control map[string]svc.LACPControl
}

func newLACPLink(s *service) *lacpLink {
	l := &lacpLink{svc: s, control: map[string]svc.LACPControl{}}
	l.legs = s.follow("cer-lacpd", svc.TopicLACPLegs, nil)
	l.ready = s.follow("cer-lacpd", svc.TopicLACPReady, nil)
	// Port states for cer-lldpd (bundle membership in LLDPDUs).
	l.port = s.follow("cer-lacpd", svc.TopicLACPPorts, func(st map[string]json.RawMessage) {
		if raw, ok := st[""]; ok {
			s.ep.Publish(svc.TopicLACPPorts, "", raw)
		}
	})
	// Until MC-LAG decides otherwise: nothing held (the daemon starts once
	// it knows).
	s.ep.Replace(svc.TopicLACPControl, map[string]any{})
	return l
}

func (l *lacpLink) Legs() map[string]bool {
	out := map[string]bool{}
	for b, raw := range l.legs.State() {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			out[b] = v
		}
	}
	return out
}

func (l *lacpLink) Ready() map[string]int {
	out := map[string]int{}
	for b, raw := range l.ready.State() {
		var v int
		if json.Unmarshal(raw, &v) == nil {
			out[b] = v
		}
	}
	return out
}

func (l *lacpLink) set(bundle string, f func(*svc.LACPControl)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.control[bundle]
	f(&c)
	if c == (svc.LACPControl{}) {
		delete(l.control, bundle)
		l.svc.ep.Publish(svc.TopicLACPControl, bundle, nil)
		return
	}
	l.control[bundle] = c
	l.svc.ep.Publish(svc.TopicLACPControl, bundle, c)
}

func (l *lacpLink) SetHold(bundle string, hold bool) {
	l.set(bundle, func(c *svc.LACPControl) { c.Hold = hold })
}

func (l *lacpLink) SetPeerReady(bundle string, n int) {
	l.set(bundle, func(c *svc.LACPControl) { c.PeerReady = n })
}
