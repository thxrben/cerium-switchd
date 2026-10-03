package access

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/model"
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
	fakeTTYs(t, sys, map[string]string{"ttyS0": "4", "ttyS1": "0", "ttyUSB0": "", "tty0": "", "tty1": ""}, "tty0 ttyAMA0")
	var calls []string
	comm := map[string]string{"getty@tty1.service": "agetty", "serial-getty@ttyUSB0.service": "agetty"}
	c := &Consoles{SysRoot: sys, UnitDir: filepath.Join(dir, "units"), StateFile: filepath.Join(dir, "c.json"),
		ProfileDir: dir,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Systemctl:  func(a ...string) error { calls = append(calls, strings.Join(a, " ")); return nil },
		MainComm:   func(u string) string { return comm[u] }}
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
	if !strings.Contains(string(drop), "agetty --autologin root --noreset --noclear 9600 %I $TERM") {
		t.Errorf("drop-in: %s", drop)
	}
	for _, want := range []string{"restart serial-getty@ttyUSB0.service", "daemon-reload", "restart getty@tty1.service"} {
		if !slices.Contains(calls, want) {
			t.Errorf("missing %q in %v", want, calls)
		}
	}
	if vt, _ := os.ReadFile(filepath.Join(c.UnitDir, "getty@.service.d", "switchd.conf")); !strings.Contains(string(vt), "--autologin root") {
		t.Errorf("vt drop-in: %s", vt)
	}
	if hook, _ := os.ReadFile(filepath.Join(dir, "switchd-cli.sh")); string(hook) != ProfileHook {
		t.Errorf("profile hook: %q", hook)
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
	// login-required: a console with a logged-in user is not restarted; the
	// VT drop-in is removed.
	cfg.System.AutoConsole = true
	cfg.System.ConsoleLogin = true
	cfg.System.Consoles = nil
	comm["serial-getty@ttyUSB0.service"] = "login"
	calls = nil
	c.Sync(cfg)
	if slices.Contains(calls, "restart serial-getty@ttyUSB0.service") || !slices.Contains(calls, "restart serial-getty@ttyAMA0.service") {
		t.Errorf("login-required: %v", calls)
	}
	if drop, _ := os.ReadFile(c.dropIn("ttyUSB0")); strings.Contains(string(drop), "autologin") {
		t.Errorf("drop-in still autologin: %s", drop)
	}
	if _, err := os.Stat(filepath.Join(c.UnitDir, "getty@.service.d", "switchd.conf")); err == nil {
		t.Error("vt drop-in not removed")
	}
	// Re-enable: unmask and start.
	cfg.System.AutoConsole = false
	cfg.System.ConsoleLogin = false
	cfg.System.Consoles = map[string]*model.Console{"ttyS0": {Device: "ttyS0", Disabled: true}}
	c.Sync(cfg)
	cfg.System.Consoles = map[string]*model.Console{"ttyS0": {Device: "ttyS0", Speed: 115200}}
	calls = nil
	c.Sync(cfg)
	if !slices.Contains(calls, "unmask serial-getty@ttyS0.service") || !slices.Contains(calls, "restart serial-getty@ttyS0.service") {
		t.Errorf("re-enable: %v", calls)
	}
}
