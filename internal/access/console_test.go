package access

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mclag/internal/model"
)

func fakeTTYs(t *testing.T, root string, ttys map[string]string, console string) {
	t.Helper()
	for n, typ := range ttys {
		d := filepath.Join(root, "class", "tty", n)
		os.MkdirAll(d, 0o755)
		if typ != "" {
			os.WriteFile(filepath.Join(d, "type"), []byte(typ+"\n"), 0o644)
		}
	}
	os.MkdirAll(filepath.Join(root, "class", "tty", "console"), 0o755)
	os.WriteFile(filepath.Join(root, "class", "tty", "console", "active"), []byte(console+"\n"), 0o644)
}

func TestConsoles(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "sys")
	fakeTTYs(t, sys, map[string]string{"ttyS0": "4", "ttyS1": "0", "ttyUSB0": "", "tty1": ""}, "tty0 ttyAMA0")
	var calls []string
	c := &Consoles{SysRoot: sys, UnitDir: filepath.Join(dir, "units"), StateFile: filepath.Join(dir, "c.json"),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Systemctl: func(a ...string) error { calls = append(calls, strings.Join(a, " ")); return nil }}
	if got := c.Detect(); !slices.Equal(got, []string{"ttyAMA0", "ttyS0", "ttyUSB0"}) {
		t.Fatalf("Detect = %v", got)
	}
	cfg := &model.Config{}
	cfg.System.AutoConsole = true
	cfg.System.Consoles = map[string]*model.Console{"ttyS0": {Device: "ttyS0", Speed: 9600}}
	if err := c.Sync(cfg); err != nil {
		t.Fatal(err)
	}
	drop, _ := os.ReadFile(c.dropIn("ttyS0"))
	if !strings.Contains(string(drop), "agetty --noreset --noclear 9600 %I $TERM") {
		t.Errorf("drop-in: %s", drop)
	}
	if !slices.Contains(calls, "restart serial-getty@ttyUSB0.service") || !slices.Contains(calls, "daemon-reload") {
		t.Errorf("calls: %v", calls)
	}
	// Idempotent.
	calls = nil
	c.Sync(cfg)
	if len(calls) != 0 {
		t.Errorf("second sync: %v", calls)
	}
	// Disable one, turn auto-detection off.
	cfg.System.AutoConsole = false
	cfg.System.Consoles = map[string]*model.Console{"ttyS0": {Device: "ttyS0", Disabled: true}}
	calls = nil
	c.Sync(cfg)
	for _, want := range []string{"mask --now serial-getty@ttyS0.service", "disable --now serial-getty@ttyUSB0.service", "disable --now serial-getty@ttyAMA0.service"} {
		if !slices.Contains(calls, want) {
			t.Errorf("missing %q in %v", want, calls)
		}
	}
	// Re-enable: unmask and start.
	cfg.System.Consoles = map[string]*model.Console{"ttyS0": {Device: "ttyS0", Speed: 115200}}
	calls = nil
	c.Sync(cfg)
	if !slices.Contains(calls, "unmask serial-getty@ttyS0.service") || !slices.Contains(calls, "restart serial-getty@ttyS0.service") {
		t.Errorf("re-enable: %v", calls)
	}
}
