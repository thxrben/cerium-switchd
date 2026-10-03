package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateUnit(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "switchd")
	// An installation of the single program of earlier versions.
	if u := renderUnit(updateUnit(exe), exe); !strings.Contains(u, "ExecStart="+exe+" update-daemon\n") {
		t.Fatalf("without switchd-update:\n%s", u)
	}
	if err := os.WriteFile(filepath.Join(dir, "switchd-update"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if u := renderUnit(updateUnit(exe), exe); !strings.Contains(u, "ExecStart="+exe+"-update\n") {
		t.Fatalf("with switchd-update:\n%s", u)
	}
}
