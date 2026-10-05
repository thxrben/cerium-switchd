package cli

import (
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
)

func TestShowMACsec(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.ReadOnly)
	ts.sh.env.Ops = &allOps{}
	out := ts.ok("show security macsec connections")
	for _, want := range []string{"Interface 1/1/0 (msens19), connectivity association stack", "State:                 secured",
		"Transmit:              SCI 0200000001010001, association 1", "Encryption:            software"} {
		if !strings.Contains(out, want) {
			t.Errorf("connections lack %q:\n%s", want, out)
		}
	}
	if out := ts.ok("show security macsec statistics interface 1/1/0"); !strings.Contains(out, "Packets encrypted:       5") {
		t.Errorf("statistics:\n%s", out)
	}
	if out := ts.ok("show security macsec connections interface 1/2/0"); !strings.Contains(out, "No MACsec interfaces") {
		t.Errorf("filter:\n%s", out)
	}
}
