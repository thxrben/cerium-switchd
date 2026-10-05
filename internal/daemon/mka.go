package daemon

import (
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/alarms"
	"github.com/thxrben/cerium-switchd/internal/inventory"
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

	// capable reports whether a port's NIC offloads MACsec (tests replace
	// it); programOffload whether wpa_supplicant knows macsec_offload.
	capable        func(linux string) bool
	programOffload func() bool
	now            func() time.Time

	mu      sync.Mutex
	unit    string            // template written
	running map[string]string // port -> configuration in use
	off     map[string]*mkaOffload
	// the last sync's arguments (a fallback applies itself at once)
	last struct {
		cfg    *model.Config
		member int
		linux  func(string) (string, bool)
	}
}

// mkaOffload is a port's hardware offload state (reference 5.15).
type mkaOffload struct {
	name     string // interface name
	offload  bool   // the running MKA offloads
	started  time.Time
	restarts int // the unit's restarts when it started
	// software until then: the offload did not come up.
	softwareUntil time.Time
}

const (
	mkaAlarm = "switchd/mka "
	// mkaOffloadWait: an offloaded link not secured by then falls back to
	// software, for mkaSoftwareFor.
	mkaOffloadWait = 30 * time.Second
	mkaSoftwareFor = 10 * time.Minute
)

func newMKAManager(backend supervise.Backend, log *slog.Logger, al *alarms.Set) *mkaManager {
	m := &mkaManager{backend: backend, log: log, alarms: al, dir: "/run/switchd/mka", program: "/usr/sbin/wpa_supplicant",
		capable: func(linux string) bool { return inventory.ReadCaps("/sys", linux).Features["macsec-hw-offload"] != "" },
		now:     time.Now}
	var once sync.Once
	var knows bool
	m.programOffload = func() bool {
		// The program is read-only on the image: once per start. An option it
		// does not know makes it refuse the whole configuration.
		once.Do(func() {
			b, err := hwio.ReadFile(m.program)
			knows = err == nil && bytes.Contains(b, []byte("macsec_offload"))
			if !knows {
				m.log.Info("mka: wpa_supplicant cannot offload MACsec; every port encrypts in software", "program", m.program)
			}
		})
		return knows
	}
	return m
}

// wantOffload decides whether a port's MKA offloads to the NIC.
func (m *mkaManager) wantOffload(port string, i *model.Interface) bool {
	if i.NoOffload || m.capable == nil || !m.capable(port) || m.programOffload == nil || !m.programOffload() {
		return false
	}
	if o := m.off[port]; o != nil && m.now().Before(o.softwareUntil) {
		return false
	}
	return true
}

// mkaUnit is the template unit (instance: the port's kernel name).
func (m *mkaManager) mkaUnit() string {
	return fmt.Sprintf(`# Written by switchd: MKA for a port secured with MACsec (reference 5.15).
[Unit]
Description=cerOS MACsec key agreement on %%i
After=switchd.service
StartLimitIntervalSec=0

[Service]
ExecStart=%s -D macsec_linux -i %%i -c %s/%%i.conf
Restart=always
RestartSec=1
Nice=-5
OOMScoreAdjust=-900
LimitCORE=0
`, m.program, m.dir)
}

func mkaUnitName(port string) string { return "cer-mka@" + port + ".service" }

// mkaConfig is a port's wpa_supplicant configuration; offload: in the NIC
// (macsec_offload=2, the MAC; PHY offload has no NIC feature to tell it).
func mkaConfig(ca *model.MACsecCA, dir string, offload bool) string {
	// macsec_csindex selects GCM-AES-256; wpa_supplicant 2.10 (Debian 13)
	// does not know the field, so it is written only for that suite (which
	// the commit check refuses while the image has 2.10).
	csindex := ""
	if ca.Bits256() {
		csindex = "\tmacsec_csindex=1\n"
	}
	if offload {
		csindex += "\tmacsec_offload=2\n"
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
%s	macsec_replay_protect=1
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
	m.last.cfg, m.last.member, m.last.linux = cfg, member, linux
	if m.off == nil {
		m.off = map[string]*mkaOffload{}
	}
	want := map[string]string{} // port -> configuration
	offload := map[string]bool{}
	names := map[string]string{}
	for name, caName := range cfg.MACsec.Ports {
		ca := cfg.MACsec.CAs[caName]
		i := cfg.Interfaces[name]
		if ca == nil || i == nil || i.Member != member || i.Parent != "" { // bundle members: PLAN 10.8
			continue
		}
		if port, ok := linux(name); ok {
			offload[port], names[port] = m.wantOffload(port, i), name
			want[port] = mkaConfig(ca, m.dir, offload[port])
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
		delete(m.off, port)
		m.alarms.Clear(mkaAlarm + port)
		m.alarms.Clear(mkaAlarm + "offload " + port)
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
		o := m.off[port]
		if o == nil {
			o = &mkaOffload{}
			m.off[port] = o
		}
		o.name, o.offload, o.started = names[port], offload[port], m.now()
		if st, err := m.backend.Show([]string{mkaUnitName(port)}); err == nil {
			o.restarts = st[mkaUnitName(port)].NRestarts
		}
		m.log.Info("mka: started", "port", port, "renegotiate", had, "offload", offload[port])
	}
	for port := range m.off {
		if o := m.off[port]; o.name == "" {
			o.name = names[port] // found running after a switchd restart
		}
	}
}

// check raises an alarm for each port whose MKA does not run (systemd
// restarts it; a failing start shows here), and moves an offloaded port
// whose link does not come up to software (secured: the port's MACsec
// device exists, by interface name).
func (m *mkaManager) check(secured func(name string) bool) {
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
	fallback := false
	m.mu.Lock()
	for _, p := range ports {
		u := states[mkaUnitName(p)]
		o := m.off[p]
		if o != nil && o.offload && secured != nil {
			switch {
			case secured(o.name):
				m.alarms.Clear(mkaAlarm + "offload " + p)
			case m.now().Sub(o.started) >= mkaOffloadWait || u.NRestarts >= o.restarts+2:
				o.softwareUntil = m.now().Add(mkaSoftwareFor)
				m.alarms.Raise(mkaAlarm+"offload "+p, alarms.Minor, fmt.Sprintf("MACsec on %s encrypts in software: the link did not come up with the NIC's offload (tried again in %s)", o.name, mkaSoftwareFor))
				m.log.Warn("mka: offloaded link not secured; software encryption", "port", p, "since", o.started)
				fallback = true
			}
		}
		if u.Running() && u.Sub == "running" {
			m.alarms.Clear(mkaAlarm + p)
			continue
		}
		m.alarms.Raise(mkaAlarm+p, alarms.Major, fmt.Sprintf("MKA (wpa_supplicant) on %s is not running (%s/%s): the port carries no traffic", p, u.Active, u.Sub))
	}
	last := m.last
	m.mu.Unlock()
	if fallback && last.cfg != nil {
		m.sync(last.cfg, last.member, last.linux) // the new configuration restarts MKA
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
