package daemon

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"mclag/packaging"
)

// unitPath is where switchd keeps its systemd unit.
const unitPath = "/etc/systemd/system/switchd.service"

// renderUnit is switchd's unit with exe as the program.
func renderUnit(exe string) string {
	if exe == "" {
		return packaging.Unit
	}
	return strings.ReplaceAll(packaging.Unit, "/usr/local/sbin/switchd", exe)
}

// ensureUnit keeps switchd's systemd unit current (reference 1.4): a
// software update may change it, and only the program is replaced by an
// update. The new unit applies the next time systemd starts or stops switchd.
func ensureUnit(path, exe string, reload func() error, log *slog.Logger) {
	want := renderUnit(exe)
	have, err := os.ReadFile(path)
	if err == nil && string(have) == want {
		return
	}
	tmp := path + ".switchd-tmp"
	if err := os.WriteFile(tmp, []byte(want), 0o644); err != nil {
		log.Warn("systemd unit: not updated", "path", path, "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		log.Warn("systemd unit: not updated", "path", path, "err", err)
		return
	}
	if err := reload(); err != nil {
		log.Warn("systemd unit: daemon-reload", "err", err)
	}
	log.Info("systemd unit updated", "path", path)
}

// osNetworkUnits are the operating system's network services that switchd
// replaces (reference 1.4).
var osNetworkUnits = []string{"networking.service", "systemd-networkd.service", "systemd-networkd.socket",
	"NetworkManager.service", "dhcpcd.service"}

// dhcpClients are DHCP client programs of the operating system.
var dhcpClients = []string{"dhclient", "dhcpcd", "udhcpc"}

// takeOverOSNetwork disables the operating system's own network
// configuration (reference 1.4): its services are masked (from the next
// boot on; nothing is stopped now, so no link goes down), and running DHCP
// clients end (switchd removes the addresses they set on its ports anyway,
// but they would add them again and rewrite resolv.conf).
func takeOverOSNetwork(systemctl func(args ...string) (string, error), procRoot string, log *slog.Logger) {
	var masked []string
	for _, u := range osNetworkUnits {
		state, _ := systemctl("is-enabled", u)
		switch strings.TrimSpace(state) {
		case "masked", "not-found", "":
			continue
		}
		if _, err := systemctl("mask", u); err != nil {
			log.Warn("OS network configuration: cannot mask "+u, "err", err)
			continue
		}
		masked = append(masked, u)
	}
	if len(masked) > 0 {
		log.Warn("OS network configuration disabled: services masked (from the next boot; their files stay)",
			"facility", "change-log", "units", strings.Join(masked, ", "))
	}
	for _, p := range findProcs(procRoot, dhcpClients) {
		if err := syscall.Kill(p.pid, syscall.SIGTERM); err == nil {
			log.Warn("OS network configuration: DHCP client ended", "facility", "change-log", "pid", p.pid, "program", p.comm)
		}
	}
}

type proc struct {
	pid  int
	comm string
}

// findProcs lists the processes whose program name is one of names.
func findProcs(procRoot string, names []string) []proc {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var out []proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "comm"))
		if err != nil {
			continue
		}
		comm := string(bytes.TrimSpace(raw))
		for _, n := range names {
			if comm == n {
				out = append(out, proc{pid, comm})
			}
		}
	}
	return out
}

// systemctlOutput runs systemctl and returns its standard output (is-enabled
// reports through its output and exit status).
func systemctlOutput(args ...string) (string, error) {
	out, err := exec.Command("systemctl", args...).Output()
	return string(out), err
}
