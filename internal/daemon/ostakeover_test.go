package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mclag/packaging"
)

func TestEnsureUnit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "switchd.service")
	reloads := 0
	reload := func() error { reloads++; return nil }
	ensureUnit(path, packaging.Unit, "/opt/ceros/switchd", reload, slog.Default())
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "ExecStart=/opt/ceros/switchd $SWITCHD_ARGS") ||
		!strings.Contains(string(raw), "ExecStopPost=-/usr/sbin/ip link del cme") || reloads != 1 {
		t.Fatalf("unit (reloads %d, err %v):\n%s", reloads, err, raw)
	}
	ensureUnit(path, packaging.Unit, "/opt/ceros/switchd", reload, slog.Default())
	if reloads != 1 {
		t.Errorf("unchanged unit reloaded again")
	}
}

func TestTakeOverOSNetwork(t *testing.T) {
	enabled := map[string]string{"networking.service": "enabled\n", "systemd-networkd.service": "disabled\n",
		"NetworkManager.service": "not-found\n", "dhcpcd.service": "masked\n"}
	var calls []string
	systemctl := func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "is-enabled" {
			return enabled[args[1]], nil
		}
		return "", nil
	}
	proc := t.TempDir()
	for pid, comm := range map[string]string{"4242": "sshd", "999999": "dhclient"} {
		os.MkdirAll(filepath.Join(proc, pid), 0o755)
		os.WriteFile(filepath.Join(proc, pid, "comm"), []byte(comm+"\n"), 0o644)
	}
	if ps := findProcs(proc, dhcpClients); len(ps) != 1 || ps[0].comm != "dhclient" || ps[0].pid != 999999 {
		t.Errorf("procs: %+v", ps)
	}
	takeOverOSNetwork(systemctl, t.TempDir(), slog.Default())
	got := strings.Join(calls, "; ")
	for _, want := range []string{"mask networking.service", "mask systemd-networkd.service"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, not := range []string{"mask NetworkManager.service", "mask dhcpcd.service"} {
		if strings.Contains(got, not) {
			t.Errorf("unexpected %q in %s", not, got)
		}
	}
}
