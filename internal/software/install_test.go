package software

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallAndReturn(t *testing.T) {
	dir := t.TempDir()
	in := &Installer{Program: filepath.Join(dir, "switchd"), StateFile: filepath.Join(dir, "software.json")}
	os.WriteFile(in.Program, []byte("v1"), 0o755)
	if err := in.Install([]byte("v2"), "v2", "v1", true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(in.Program); string(b) != "v2" {
		t.Fatalf("program: %q", b)
	}
	if b, _ := os.ReadFile(in.Program + ".prev"); string(b) != "v1" {
		t.Fatalf("previous: %q", b)
	}
	// v2 fails to come up: three starts are allowed, the fourth returns.
	for i := 1; i <= MaxAttempts; i++ {
		if restart, err := in.Start("v2"); restart || err != nil {
			t.Fatalf("start %d: %v %v", i, restart, err)
		}
	}
	restart, err := in.Start("v2")
	if !restart || err != nil {
		t.Fatalf("no return after %d starts: %v", MaxAttempts, err)
	}
	if b, _ := os.ReadFile(in.Program); string(b) != "v1" {
		t.Fatalf("program after return: %q", b)
	}
	st := in.Load()
	if st.Note == "" || st.Pending == nil || st.Pending.Version != "v1" || !st.Pending.ExitMaintenance || st.Previous != "v2" {
		t.Fatalf("state after return: %+v", st)
	}
	// v1 comes up: done, maintenance mode is left.
	in.Start("v1")
	p, err := in.Healthy("v1")
	if err != nil || p == nil || !p.ExitMaintenance || in.Load().Pending != nil {
		t.Fatalf("healthy: %+v %v", p, err)
	}
	// Rollback swaps again.
	to, err := in.Rollback("v1", false)
	if err != nil || to != "v2" {
		t.Fatalf("rollback: %s %v", to, err)
	}
	if b, _ := os.ReadFile(in.Program); string(b) != "v2" {
		t.Fatalf("program after rollback: %q", b)
	}
	// A start with another version clears a stale pending update.
	if restart, _ := in.Start("v9"); restart || in.Load().Pending != nil {
		t.Error("stale pending update kept")
	}
}
