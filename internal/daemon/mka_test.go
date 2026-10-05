package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/alarms"
	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/supervise"
)

type fakeUnits struct {
	installed bool
	actions   []string
	running   map[string]bool
	files     map[string]string
}

func (f *fakeUnits) Installed(string) bool { return f.installed }
func (f *fakeUnits) WriteUnit(u, c string) (bool, error) {
	ch := f.files[u] != c
	f.files[u] = c
	return ch, nil
}
func (f *fakeUnits) Reload() error { f.actions = append(f.actions, "reload"); return nil }
func (f *fakeUnits) Start(u string) error {
	f.actions = append(f.actions, "start "+u)
	f.running[u] = true
	return nil
}
func (f *fakeUnits) Stop(u string) error {
	f.actions = append(f.actions, "stop "+u)
	delete(f.running, u)
	return nil
}
func (f *fakeUnits) Restart(u string) error {
	f.actions = append(f.actions, "restart "+u)
	f.running[u] = true
	return nil
}
func (f *fakeUnits) Show(units []string) (map[string]supervise.UnitState, error) {
	out := map[string]supervise.UnitState{}
	for _, u := range units {
		if f.running[u] {
			out[u] = supervise.UnitState{Active: "active", Sub: "running"}
		} else {
			out[u] = supervise.UnitState{Active: "failed", Sub: "failed"}
		}
	}
	return out, nil
}
func (f *fakeUnits) StopWait(string, time.Duration) error { return nil }
func (f *fakeUnits) Kill(string) error                    { return nil }

func mkaCfg(t *testing.T, cak string, ports ...string) *model.Config {
	t.Helper()
	text := "set virtual-chassis member 1\n" +
		"set security macsec connectivity-association ca1 pre-shared-key ckn 0a0b\n" +
		"set security macsec connectivity-association ca1 pre-shared-key cak " + cak + "\n" +
		"set security macsec connectivity-association ca1 mka key-server-priority 7\n"
	for _, p := range ports {
		text += "set interfaces " + p + " unit 0 family ethernet-switching\n" +
			"set security macsec interfaces " + p + " connectivity-association ca1\n"
	}
	tree, err := config.ParseSet(text)
	if err != nil {
		t.Fatal(err)
	}
	cfg, issues := model.Build(tree, nil)
	if issues.HasErrors() {
		t.Fatal(issues)
	}
	return cfg
}

func TestMKAManager(t *testing.T) {
	f := &fakeUnits{installed: true, running: map[string]bool{}, files: map[string]string{}}
	m := newMKAManager(f, slog.New(slog.DiscardHandler), &alarms.Set{})
	m.dir = t.TempDir()
	linux := func(n string) (string, bool) { return "eth" + n[len(n)-1:], true }
	cak1 := strings.Repeat("11", 16)
	m.sync(mkaCfg(t, cak1, "1/0/1", "1/0/2"), 1, linux)
	if !slices.Equal(f.actions, []string{"reload", "start cer-mka@eth1.service", "start cer-mka@eth2.service"}) {
		t.Fatalf("actions %v", f.actions)
	}
	if !strings.Contains(f.files["cer-mka@.service"], "-D macsec_linux -i %i -c "+m.dir+"/%i.conf") {
		t.Fatalf("unit:\n%s", f.files["cer-mka@.service"])
	}
	raw, err := os.ReadFile(filepath.Join(m.dir, "eth1.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"macsec_policy=1", "mka_priority=7", "mka_ckn=0a0b", "mka_cak=" + cak1, "macsec_replay_window=0"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("configuration lacks %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "macsec_csindex") {
		t.Error("wpa_supplicant 2.10 rejects macsec_csindex: written for gcm-aes-128")
	}
	if fi, _ := os.Stat(filepath.Join(m.dir, "eth1.conf")); fi.Mode().Perm() != 0o600 {
		t.Errorf("the CAK is readable by others: %v", fi.Mode())
	}
	// Unchanged: nothing.
	f.actions = nil
	m.sync(mkaCfg(t, cak1, "1/0/1", "1/0/2"), 1, linux)
	if len(f.actions) != 0 {
		t.Fatalf("unchanged: %v", f.actions)
	}
	// A new CAK: both renegotiate; a port removed: stopped, file gone.
	m.sync(mkaCfg(t, strings.Repeat("22", 16), "1/0/1"), 1, linux)
	if !slices.Equal(f.actions, []string{"stop cer-mka@eth2.service", "restart cer-mka@eth1.service"}) {
		t.Fatalf("change: %v", f.actions)
	}
	if _, err := os.Stat(filepath.Join(m.dir, "eth2.conf")); err == nil {
		t.Fatal("removed port's configuration (with the CAK) kept")
	}
	// switchd restarts: the running configuration is found on disk.
	m2 := newMKAManager(f, slog.New(slog.DiscardHandler), &alarms.Set{})
	m2.dir = m.dir
	f.actions = nil
	m2.sync(mkaCfg(t, strings.Repeat("22", 16), "1/0/1"), 1, linux)
	if len(f.actions) != 0 {
		t.Fatalf("after a switchd restart: %v", f.actions)
	}
	// MKA does not run: alarm.
	delete(f.running, "cer-mka@eth1.service")
	m2.check(nil)
	if l := m2.alarms.List(); len(l) != 1 || !strings.Contains(l[0].Text, "eth1 is not running") {
		t.Fatalf("alarms %+v", l)
	}
	// wpa_supplicant missing: a Major alarm, nothing started.
	f3 := &fakeUnits{running: map[string]bool{}, files: map[string]string{}}
	m3 := newMKAManager(f3, slog.New(slog.DiscardHandler), &alarms.Set{})
	m3.dir = t.TempDir()
	m3.sync(mkaCfg(t, cak1, "1/0/1"), 1, linux)
	if len(f3.actions) != 0 || len(m3.alarms.List()) != 1 {
		t.Fatalf("missing program: %v %v", f3.actions, m3.alarms.List())
	}
}

// Offload (reference 5.15): only on a NIC that can, with a wpa_supplicant
// that knows the option, and not with offload disable; a link that does
// not come up offloaded falls back to software, and tries again later.
func TestMKAOffload(t *testing.T) {
	f := &fakeUnits{installed: true, running: map[string]bool{}, files: map[string]string{}}
	al := &alarms.Set{}
	m := newMKAManager(f, slog.New(slog.DiscardHandler), al)
	m.dir = t.TempDir()
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	m.capable = func(port string) bool { return port != "eth3" } // eth3's NIC cannot
	knows := true
	m.programOffload = func() bool { return knows }
	linux := func(n string) (string, bool) { return "eth" + n[len(n)-1:], true }
	cfg := mkaCfg(t, strings.Repeat("11", 16), "1/0/1", "1/0/2", "1/0/3")
	cfg.Interfaces["1/0/2"].NoOffload = true
	m.sync(cfg, 1, linux)
	conf := func(port string) string {
		b, _ := os.ReadFile(filepath.Join(m.dir, port+".conf"))
		return string(b)
	}
	if !strings.Contains(conf("eth1"), "macsec_offload=2") || strings.Contains(conf("eth2"), "macsec_offload") || strings.Contains(conf("eth3"), "macsec_offload") {
		t.Fatalf("offload in eth1 only:\n%s\n%s\n%s", conf("eth1"), conf("eth2"), conf("eth3"))
	}
	// Not secured after 30 s: software, a Minor alarm, MKA restarted.
	f.actions = nil
	now = now.Add(31 * time.Second)
	m.check(func(string) bool { return false })
	if strings.Contains(conf("eth1"), "macsec_offload") || !slices.Contains(f.actions, "restart cer-mka@eth1.service") {
		t.Fatalf("no fallback: %v\n%s", f.actions, conf("eth1"))
	}
	if l := al.List(); !slices.ContainsFunc(l, func(a alarms.Alarm) bool { return a.Class == alarms.Minor && strings.Contains(a.Text, "1/0/1 encrypts in software") }) {
		t.Fatalf("alarms %+v", l)
	}
	// Ten minutes later the offload is tried again.
	now = now.Add(11 * time.Minute)
	m.sync(cfg, 1, linux)
	if !strings.Contains(conf("eth1"), "macsec_offload=2") {
		t.Fatal("offload not tried again")
	}
	// Secured offloaded: the alarm ends.
	m.check(func(name string) bool { return name == "1/0/1" })
	if slices.ContainsFunc(al.List(), func(a alarms.Alarm) bool { return strings.Contains(a.ID, "offload") }) {
		t.Fatalf("alarm left: %+v", al.List())
	}
	// A wpa_supplicant without the option: never offloaded.
	knows = false
	m2 := newMKAManager(f, slog.New(slog.DiscardHandler), &alarms.Set{})
	m2.dir, m2.capable, m2.programOffload = t.TempDir(), m.capable, func() bool { return knows }
	m2.sync(cfg, 1, linux)
	b, _ := os.ReadFile(filepath.Join(m2.dir, "eth1.conf"))
	if strings.Contains(string(b), "macsec_offload") {
		t.Fatal("offload with a wpa_supplicant that does not know it")
	}
}
