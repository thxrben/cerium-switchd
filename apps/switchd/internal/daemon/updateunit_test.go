package daemon

import (
	"path/filepath"
	"strings"
	"testing"
)

// The update daemon is its own program next to switchd.
func TestUpdateUnit(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "switchd")
	if u := renderUnit(updateUnit(exe), exe); !strings.Contains(u, "ExecStart="+exe+"-update\n") {
		t.Fatalf("unit:\n%s", u)
	}
}
