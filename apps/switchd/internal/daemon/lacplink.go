package daemon

import (
	"encoding/json"

	"github.com/thxrben/cerium-switchd/lib/platform/svc"
)

// republishLACPPorts follows cer-lacpd's port states and republishes them
// for cer-lldpd (bundle membership in LLDPDUs). MC-LAG and RSTP talk to
// cer-lacpd directly (reference 1.9).
func republishLACPPorts(s *service) {
	s.follow("cer-lacpd", svc.TopicLACPPorts, func(st map[string]json.RawMessage) {
		if raw, ok := st[""]; ok {
			s.ep.Publish(svc.TopicLACPPorts, "", raw)
		}
	})
}
