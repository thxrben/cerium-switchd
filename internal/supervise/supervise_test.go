package supervise

import (
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeSystemd behaves like systemd for the units it is given.
type fakeSystemd struct {
	installed map[string]bool
	files     map[string]string
	states    map[string]UnitState
	actions   []string
	nextPID   int
}

func newFake() *fakeSystemd {
	return &fakeSystemd{installed: map[string]bool{}, files: map[string]string{}, states: map[string]UnitState{}, nextPID: 100}
}

func (f *fakeSystemd) Installed(path string) bool { return f.installed[path] }

func (f *fakeSystemd) WriteUnit(unit, content string) (bool, error) {
	if f.files[unit] == content {
		return false, nil
	}
	f.files[unit] = content
	f.actions = append(f.actions, "write "+unit)
	return true, nil
}

func (f *fakeSystemd) Reload() error { f.actions = append(f.actions, "reload"); return nil }

func (f *fakeSystemd) run(unit string) {
	f.nextPID++
	u := f.states[unit]
	u.Active, u.Sub, u.PID, u.Result = "active", "running", f.nextPID, "success"
	f.states[unit] = u
}

func (f *fakeSystemd) Start(unit string) error {
	f.actions = append(f.actions, "start "+unit)
	f.run(unit)
	return nil
}

func (f *fakeSystemd) Stop(unit string) error {
	f.actions = append(f.actions, "stop "+unit)
	f.states[unit] = UnitState{Active: "inactive", Sub: "dead", Result: "success", NRestarts: f.states[unit].NRestarts}
	return nil
}

func (f *fakeSystemd) Restart(unit string) error {
	f.actions = append(f.actions, "restart "+unit)
	f.run(unit)
	return nil
}

func (f *fakeSystemd) Show(units []string) (map[string]UnitState, error) {
	out := map[string]UnitState{}
	for _, u := range units {
		st, ok := f.states[u]
		if !ok {
			st = UnitState{Active: "inactive", Sub: "dead"}
		}
		st.Loaded = f.files[u] != ""
		out[u] = st
	}
	return out, nil
}

// crash: the process died and systemd is about to restart it.
func (f *fakeSystemd) crash(unit, result string, status int) {
	u := f.states[unit]
	u.Active, u.Sub, u.Result, u.ExitStatus, u.PID = "activating", "auto-restart", result, status, 0
	u.NRestarts++
	f.states[unit] = u
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var testDaemons = []Daemon{
	{Program: "cer-lacpd", Name: "lacp", Help: "LACP", Always: true, Nice: -10, OOM: -900, Caps: netCaps, RestartDelay: 100 * time.Millisecond},
	{Program: "cer-bfdd", Name: "bfd", Help: "BFD", RealTime: 50, OOM: -900, Caps: netCaps},
	{Program: "cer-syslogd", Name: "syslog", Help: "syslog", Always: true, Nice: 10, IOIdle: true},
}

func setup(t *testing.T) (*Supervisor, *fakeSystemd, *[]string, map[string]bool) {
	f := newFake()
	f.installed["/usr/local/sbin/cer-lacpd"] = true
	f.installed["/usr/local/sbin/cer-bfdd"] = true
	notes := &[]string{}
	wanted := map[string]bool{}
	s := &Supervisor{Backend: f, Log: quiet, Dir: "/usr/local/sbin", Args: []string{"-member", "2"}, Member: 2,
		Notify: func(s string) { *notes = append(*notes, s) }, Wanted: func() map[string]bool { return wanted }, Daemons: testDaemons}
	return s, f, notes, wanted
}

func TestStartsWhatIsNeeded(t *testing.T) {
	s, f, notes, wanted := setup(t)
	now := time.Unix(1000, 0)
	s.Step(now)
	want := []string{"write cer-lacpd.service", "write cer-bfdd.service", "reload", "start cer-lacpd.service"}
	if !slices.Equal(f.actions, want) {
		t.Fatalf("actions %v, want %v", f.actions, want)
	}
	// cer-syslogd is needed but its program is missing: reported once a
	// minute.
	if len(*notes) != 1 || !strings.Contains((*notes)[0], "member 2: cer-syslogd is not installed") {
		t.Fatalf("notes %v", *notes)
	}
	s.Step(now.Add(30 * time.Second))
	s.Step(now.Add(61 * time.Second))
	if len(*notes) != 2 {
		t.Fatalf("not-installed notices: %v", *notes)
	}
	// BFD is configured: started; removed: stopped without a failure.
	f.actions = nil
	wanted["cer-bfdd"] = true
	s.Step(now.Add(62 * time.Second))
	s.Step(now.Add(63 * time.Second))
	wanted["cer-bfdd"] = false
	s.Step(now.Add(64 * time.Second))
	s.Step(now.Add(65 * time.Second))
	if want := []string{"start cer-bfdd.service", "stop cer-bfdd.service"}; !slices.Equal(f.actions, want) {
		t.Fatalf("on demand: %v, want %v", f.actions, want)
	}
	if len(*notes) != 2 {
		t.Fatalf("a planned stop was reported: %v", *notes)
	}
}

func TestReportsFailureAndRecovery(t *testing.T) {
	s, f, notes, _ := setup(t)
	now := time.Unix(1000, 0)
	s.Step(now)
	s.Step(now.Add(time.Second))
	*notes = nil
	f.crash("cer-lacpd.service", "signal", 11)
	s.Step(now.Add(2 * time.Second))
	if len(*notes) != 1 || (*notes)[0] != "member 2: cer-lacpd failed (killed by signal SEGV) and is restarted" {
		t.Fatalf("failure: %v", *notes)
	}
	st := s.Status()[0]
	if st.State != "restarting" || st.Restarts != 1 || st.LastFailure != "killed by signal SEGV" {
		t.Fatalf("status %+v", st)
	}
	f.run("cer-lacpd.service") // systemd restarted it
	s.Step(now.Add(3 * time.Second))
	if len(*notes) != 2 || (*notes)[1] != "member 2: cer-lacpd runs again (1 restart in the last hour)" {
		t.Fatalf("recovery: %v", *notes)
	}
	// A hang ends through the watchdog.
	f.crash("cer-lacpd.service", "watchdog", 6)
	s.Step(now.Add(4 * time.Second))
	f.run("cer-lacpd.service")
	s.Step(now.Add(5 * time.Second))
	if !strings.Contains((*notes)[2], "hung: no sign of life for 10 s") || !strings.Contains((*notes)[3], "2 restarts") {
		t.Fatalf("watchdog: %v", *notes)
	}
	// Restarts older than an hour are not counted.
	s.Step(now.Add(2 * time.Hour))
	if st := s.Status()[0]; st.Restarts != 0 || st.State != "running" {
		t.Fatalf("after an hour %+v", st)
	}
}

func TestPlannedRestartsAreNoFailures(t *testing.T) {
	s, f, notes, _ := setup(t)
	now := time.Now()
	s.Step(now)
	s.Step(now.Add(time.Second))
	*notes = nil
	if err := s.RestartDaemon("lacp"); err != nil {
		t.Fatal(err)
	}
	s.Step(now.Add(2 * time.Second))
	// New arguments (e.g. the member id after joining a stack): the unit
	// is rewritten and the daemon restarted, again without a failure.
	s.Args = []string{"-member", "3"}
	f.actions = nil
	s.Step(now.Add(3 * time.Second))
	if want := []string{"write cer-lacpd.service", "write cer-bfdd.service", "reload", "restart cer-lacpd.service"}; !slices.Equal(f.actions, want) {
		t.Fatalf("actions %v", f.actions)
	}
	s.Step(now.Add(4 * time.Second))
	if len(*notes) != 0 {
		t.Fatalf("planned restarts reported: %v", *notes)
	}
	if err := s.RestartDaemon("nope"); err == nil {
		t.Fatal("unknown daemon restarted")
	}
}

func TestUnit(t *testing.T) {
	u := Unit(testDaemons[0], "/usr/local/sbin/cer-lacpd", []string{"-member", "2"})
	for _, want := range []string{"ExecStart=/usr/local/sbin/cer-lacpd -member 2\n", "Nice=-10\n", "OOMScoreAdjust=-900\n",
		"RestartSec=100ms\n", "WatchdogSec=10s\n", "Type=notify\n", "CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW\n"} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	u = Unit(testDaemons[1], "/x/cer-bfdd", nil)
	if !strings.Contains(u, "CPUSchedulingPolicy=fifo\nCPUSchedulingPriority=50\n") || strings.Contains(u, "Nice=") {
		t.Errorf("real-time unit:\n%s", u)
	}
	if u := Unit(testDaemons[2], "/x/cer-syslogd", nil); !strings.Contains(u, "Nice=10\nIOSchedulingClass=idle\n") {
		t.Errorf("idle unit:\n%s", u)
	}
}

func TestParseShow(t *testing.T) {
	out := `Id=cer-lacpd.service
LoadState=loaded
ActiveState=active
SubState=running
Result=success
MainPID=4242
NRestarts=3
ExecMainCode=0
ExecMainStatus=0
ActiveEnterTimestamp=@1790000000
MemoryCurrent=12582912
CPUUsageNSec=1500000000

Id=cer-bfdd.service
LoadState=loaded
ActiveState=activating
SubState=auto-restart
Result=core-dump
MainPID=0
NRestarts=1
ExecMainCode=3
ExecMainStatus=6
ActiveEnterTimestamp=
MemoryCurrent=[not set]
CPUUsageNSec=[not set]
`
	m, err := ParseShow(out)
	if err != nil {
		t.Fatal(err)
	}
	l := m["cer-lacpd.service"]
	if !l.Running() || l.PID != 4242 || l.NRestarts != 3 || l.Since.Unix() != 1790000000 || l.Memory != 12<<20 || l.CPU != 1500*time.Millisecond {
		t.Fatalf("lacpd %+v", l)
	}
	b := m["cer-bfdd.service"]
	if b.Running() || b.Sub != "auto-restart" || describe(b) != "crashed with signal ABRT" {
		t.Fatalf("bfdd %+v %q", b, describe(b))
	}
}
