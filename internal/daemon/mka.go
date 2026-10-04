package daemon

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/thxrben/cerium-switchd/internal/alarms"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/supervise"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// mkaManager runs MKA (IEEE 802.1X-2010) for the ports secured with MACsec
// (security macsec interfaces, reference 5.15): one wpa_supplicant per port
// (macsec_linux driver), as an instance of the template unit cer-mka@.
// wpa_supplicant makes the port's MACsec device once the link is secured;
// the data plane moves the port's traffic onto it (dataplane.SecDevices).
type mkaManager struct {
	backend supervise.Backend
	log     *slog.Logger
	alarms  *alarms.Set
	// dir holds the per-port configurations (with the CAK: root only, in
	// memory) and the control sockets.
	dir     string
	program string // wpa_supplicant

	mu      sync.Mutex
	unit    string            // template written
	running map[string]string // port -> configuration in use
}

const mkaAlarm = "switchd/mka "

func newMKAManager(backend supervise.Backend, log *slog.Logger, al *alarms.Set) *mkaManager {
	return &mkaManager{backend: backend, log: log, alarms: al, dir: "/run/switchd/mka", program: "/usr/sbin/wpa_supplicant"}
}

// mkaUnit is the template unit (instance: the port's kernel name).
func (m *mkaManager) mkaUnit() string {
	return fmt.Sprintf(`# Written by switchd: MKA for a port secured with MACsec (reference 5.15).
[Unit]
Description=cerOS MACsec key agreement on %%i
After=switchd.service

[Service]
ExecStart=%s -D macsec_linux -i %%i -c %s/%%i.conf
Restart=always
RestartSec=1
StartLimitIntervalSec=0
Nice=-5
OOMScoreAdjust=-900
LimitCORE=0
`, m.program, m.dir)
}

func mkaUnitName(port string) string { return "cer-mka@" + port + ".service" }

// mkaConfig is a port's wpa_supplicant configuration.
func mkaConfig(ca *model.MACsecCA, dir string) string {
	csindex := 0 // GCM-AES-128
	if ca.Bits256() {
		csindex = 1
	}
	return fmt.Sprintf(`# Written by switchd (reference 5.15): connectivity association %s
ctrl_interface=%s
eapol_version=3
ap_scan=0
network={
	key_mgmt=NONE
	eapol_flags=0
	macsec_policy=1
	macsec_integ_only=0
	macsec_port=1
	macsec_csindex=%d
	macsec_replay_protect=1
	macsec_replay_window=%d
	mka_priority=%d
	mka_ckn=%s
	mka_cak=%s
}
`, ca.Name, dir, csindex, ca.ReplayWindow, ca.KeyServerPriority, ca.CKN, ca.CAK)
}

// sync starts, restarts (changed CA) and stops the ports' MKA. linux maps
// an interface name to its port's kernel name.
func (m *mkaManager) sync(cfg *model.Config, member int, linux func(string) (string, bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]string{} // port -> configuration
	for name, caName := range cfg.MACsec.Ports {
		ca := cfg.MACsec.CAs[caName]
		i := cfg.Interfaces[name]
		if ca == nil || i == nil || i.Member != member || i.Parent != "" {
			continue
		}
		if port, ok := linux(name); ok {
			want[port] = mkaConfig(ca, m.dir)
		}
	}
	if m.running == nil {
		m.running = map[string]string{}
		// After a switchd restart: the configurations on disk are what runs.
		if ents, err := os.ReadDir(m.dir); err == nil {
			for _, e := range ents {
				if port, ok := strings.CutSuffix(e.Name(), ".conf"); ok {
					if raw, err := hwio.ReadFile(filepath.Join(m.dir, e.Name())); err == nil {
						m.running[port] = string(raw)
					}
				}
			}
		}
	}
	if len(want) > 0 {
		if !m.backend.Installed(m.program) {
			m.alarms.Raise(mkaAlarm+"missing", alarms.Major, m.program+" is not installed: ports secured with MACsec carry no traffic")
			return
		}
		m.alarms.Clear(mkaAlarm + "missing")
		if err := hwio.MkdirAll(m.dir, 0o700); err != nil {
			m.log.Error("mka: directory", "err", err)
			return
		}
		if m.unit == "" {
			ch, err := m.backend.WriteUnit("cer-mka@.service", m.mkaUnit())
			if err != nil {
				m.log.Error("mka: unit not written", "err", err)
				return
			}
			if ch {
				_ = m.backend.Reload()
			}
			m.unit = "written"
		}
	} else {
		m.alarms.Clear(mkaAlarm + "missing")
	}
	for _, port := range slices.Sorted(maps.Keys(m.running)) {
		if _, ok := want[port]; ok {
			continue
		}
		if err := m.backend.Stop(mkaUnitName(port)); err != nil {
			m.log.Warn("mka: not stopped", "port", port, "err", err)
		}
		_ = hwio.Remove(filepath.Join(m.dir, port+".conf"))
		delete(m.running, port)
		m.alarms.Clear(mkaAlarm + port)
		m.log.Info("mka: stopped (MACsec removed from the port)", "port", port)
	}
	for _, port := range slices.Sorted(maps.Keys(want)) {
		conf := want[port]
		if m.running[port] == conf {
			continue
		}
		if err := hwio.WriteFile(filepath.Join(m.dir, port+".conf"), []byte(conf), 0o600); err != nil {
			m.log.Error("mka: configuration not written", "port", port, "err", err)
			continue
		}
		_, had := m.running[port]
		start := m.backend.Start
		if had {
			start = m.backend.Restart // a changed CA: renegotiate
		}
		if err := start(mkaUnitName(port)); err != nil {
			m.log.Error("mka: not started", "port", port, "err", err)
			continue
		}
		m.running[port] = conf
		m.log.Info("mka: started", "port", port, "renegotiate", had)
	}
}

// check raises an alarm for each port whose MKA does not run (systemd
// restarts it; a failing start shows here).
func (m *mkaManager) check() {
	m.mu.Lock()
	ports := slices.Sorted(maps.Keys(m.running))
	m.mu.Unlock()
	if len(ports) == 0 {
		return
	}
	var units []string
	for _, p := range ports {
		units = append(units, mkaUnitName(p))
	}
	states, err := m.backend.Show(units)
	if err != nil {
		return
	}
	for _, p := range ports {
		u := states[mkaUnitName(p)]
		if u.Running() && u.Sub == "running" {
			m.alarms.Clear(mkaAlarm + p)
			continue
		}
		m.alarms.Raise(mkaAlarm+p, alarms.Major, fmt.Sprintf("MKA (wpa_supplicant) on %s is not running (%s/%s): the port carries no traffic", p, u.Active, u.Sub))
	}
}

// state is the MKA state of a port for show security macsec connections:
// running or why not.
func (m *mkaManager) state(port string) (configured bool, problem string) {
	m.mu.Lock()
	_, configured = m.running[port]
	m.mu.Unlock()
	if !configured {
		return false, ""
	}
	if st, err := m.backend.Show([]string{mkaUnitName(port)}); err == nil {
		if u := st[mkaUnitName(port)]; !(u.Running() && u.Sub == "running") {
			return true, fmt.Sprintf("MKA not running (%s/%s)", u.Active, u.Sub)
		}
	}
	return true, ""
}
