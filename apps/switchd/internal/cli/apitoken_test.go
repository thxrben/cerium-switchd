package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
)

// The token is printed once; only its SHA-256 is in the candidate.
func TestAPITokenCreate(t *testing.T) {
	ts := newTester(t, newEngine(t), "root", commit.SuperUser)
	out := ts.ok("request system api-token create ci user admin")
	var token string
	for l := range strings.Lines(out) {
		if f := strings.Fields(l); len(f) == 1 && len(f[0]) == 43 {
			token = f[0]
		}
	}
	if token == "" {
		t.Fatalf("no token in:\n%s", out)
	}
	sum := sha256.Sum256([]byte(token))
	ts.ok("configure")
	cand := ts.ok("show system services web-management | display set")
	if !strings.Contains(cand, "api-token ci hash "+hex.EncodeToString(sum[:])) || strings.Contains(cand, token) {
		t.Fatalf("candidate:\n%s", cand)
	}
	ts.ok("exit")
	op := newTester(t, newEngine(t), "olga", commit.Operator)
	if out := op.sh.Execute(t.Context(), "request system api-token create x user olga", op.term).Output; !strings.Contains(out, "permission denied") {
		t.Fatalf("an operator made a token: %s", out)
	}
}
