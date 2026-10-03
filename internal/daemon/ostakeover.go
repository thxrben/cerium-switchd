package daemon

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/thxrben/cerium-switchd/packaging"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"github.com/thxrben/cerium-switchd/pkg/sysexec"
)

// unitPath is where switchd keeps its systemd unit, updateUnitPath the
// update daemon's.
const (
	unitPath       = "/etc/systemd/system/switchd.service"
	updateUnitPath = "/etc/systemd/system/switchd-update.service"
)

// renderUnit is a unit with exe as the program.
// updateUnit is the update daemon's unit: its own program next to switchd,
// or "switchd update-daemon" where that program is not installed (an
// installation of an earlier version's single program).
func updateUnit(exe string) string {
	if exe == "" {
		return packaging.UpdateUnit
	}
	if _, err := hwio.Stat(filepath.Join(filepath.Dir(exe), "switchd-update")); err != nil {
		return strings.Replace(packaging.UpdateUnit, "/usr/local/sbin/switchd-update", "/usr/local/sbin/switchd update-daemon", 1)
	}
	return packaging.UpdateUnit
}

func renderUnit(unit, exe string) string {
	if exe == "" {
		return unit
	}
	return strings.ReplaceAll(unit, "/usr/local/sbin/switchd", exe)
}

// ensureUnit keeps switchd's systemd unit current (reference 1.4): a
// software update may change it, and only the program is replaced by an
// update. The new unit applies the next time systemd starts or stops switchd.
func ensureUnit(path, unit, exe string, reload func() error, log *slog.Logger) bool {
	want := renderUnit(unit, exe)
	have, err := hwio.ReadFile(path)
	if err == nil && string(have) == want {
		return false
	}
	tmp := path + ".switchd-tmp"
	if err := hwio.WriteFile(tmp, []byte(want), 0o644); err != nil {
		log.Warn("systemd unit: not updated", "path", path, "err", err)
		return false
	}
	if err := hwio.Rename(tmp, path); err != nil {
		hwio.Remove(tmp)
		log.Warn("systemd unit: not updated", "path", path, "err", err)
		return false
	}
	if err := reload(); err != nil {
		log.Warn("systemd unit: daemon-reload", "err", err)
	}
	log.Info("systemd unit updated", "path", path)
	return true
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
	entries, err := hwio.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var out []proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		raw, err := hwio.ReadFile(filepath.Join(procRoot, e.Name(), "comm"))
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
	out, err := sysexec.Output("systemctl", args...)
	return string(out), err
}
