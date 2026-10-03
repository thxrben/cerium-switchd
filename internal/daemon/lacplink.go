package daemon

import (
	"encoding/json"

	"github.com/thxrben/cerium-switchd/internal/svc"
)

// lacpLink is switchd's view of cer-lacpd (reference 1.9): the legs (for
// RSTP) and the port states (republished for cer-lldpd). MC-LAG's holds
// come from cer-mclagd, not from switchd.
type lacpLink struct {
	svc               *service
	legs, ready, port *mirror
}

func newLACPLink(s *service) *lacpLink {
	l := &lacpLink{svc: s}
	l.legs = s.follow("cer-lacpd", svc.TopicLACPLegs, nil)
	l.ready = s.follow("cer-lacpd", svc.TopicLACPReady, nil)
	// Port states for cer-lldpd (bundle membership in LLDPDUs).
	l.port = s.follow("cer-lacpd", svc.TopicLACPPorts, func(st map[string]json.RawMessage) {
		if raw, ok := st[""]; ok {
			s.ep.Publish(svc.TopicLACPPorts, "", raw)
		}
	})
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
