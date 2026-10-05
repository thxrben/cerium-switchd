// Package supervise starts and watches the cer- daemons for switchd
// (reference 1.9): it writes a systemd unit per daemon (program,
// arguments, scheduling, privileges, watchdog), starts the daemons that
// should run, stops the others, and checks every second that they run.
// systemd executes them, so they keep running while switchd restarts.
// Failures and recoveries are reported (to the CLI sessions and the log).
package supervise

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/thxrben/cerium-switchd/internal/alarms"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// Daemon describes one cer- daemon (the table of reference 1.9).
type Daemon struct {
	Program string // cer-lldpd
	// Name is the daemon's name in "restart <name>".
	Name string
	Help string
	// Always: runs whenever switchd runs; otherwise only while Wanted
	// says so (a protocol that is configured).
	Always bool
	Nice   int
	// RealTime: SCHED_FIFO priority (0: normal scheduling with Nice).
	RealTime int
	IOIdle   bool
	OOM      int
	Caps     []string
	// RestartDelay after an unexpected end (default 200 ms).
	RestartDelay time.Duration
	// External: the unit is written and started elsewhere (switchd-update);
	// it is only watched and reported.
	External bool
	// StopStage orders the shutdown (lower first; the same stage stops in
	// parallel); StopTimeout is how long a daemon may take to finish its
	// work (close sessions, tell peers, flush) before it is killed
	// (default 3 s).
	StopStage   int
	StopTimeout time.Duration
}

// stopTimeout returns the daemon's stop budget.
func (d Daemon) stopTimeout() time.Duration {
	if d.StopTimeout > 0 {
		return d.StopTimeout
	}
	return 3 * time.Second
}

// Unit returns the systemd unit name.
func (d Daemon) Unit() string { return d.Program + ".service" }

// Scheduling describes the CPU scheduling for show system processes.
func (d Daemon) Scheduling() string {
	if d.RealTime > 0 {
		return fmt.Sprintf("real-time %d", d.RealTime)
	}
	s := fmt.Sprintf("nice %d", d.Nice)
	if d.IOIdle {
		s += ", idle I/O"
	}
	return s
}

var netCaps = []string{"CAP_NET_ADMIN", "CAP_NET_RAW"}

// Daemons are the programs switchd manages. A daemon is added here when
// its program exists (PLAN.md Phase 9a stages).
var Daemons = []Daemon{
	{Program: "switchd-update", Name: "update", Help: "update daemon", Always: true, External: true},
	// Stop stage 0: the routing protocols end their adjacencies and
	// sessions (the neighbours notice at once).
	{Program: "cer-ospfd", Name: "ospf", Help: "OSPF and OSPFv3", Nice: -5, OOM: -500, Caps: netCaps, StopStage: 0,
		StopTimeout: 5 * time.Second},
	{Program: "cer-bgpd", Name: "bgp", Help: "BGP", Nice: -5, OOM: -500, Caps: append(slices.Clone(netCaps), "CAP_NET_BIND_SERVICE"),
		StopStage: 0, StopTimeout: 10 * time.Second},
	// Stop stage 1 (after the routing protocols, stage 0: cer-ospfd 5 s,
	// cer-bgpd 10 s, which close their sessions): BFD tells its neighbours
	// AdminDown.
	{Program: "cer-bfdd", Name: "bfd", Help: "BFD", RealTime: 50, OOM: -900, Caps: netCaps, RestartDelay: 100 * time.Millisecond,
		StopStage: 1, StopTimeout: 2 * time.Second},
	// Stage 2: MC-LAG takes its legs out of their bundles (it needs
	// cer-lacpd, stage 3) and ends the MAC synchronisation; the others
	// close their sockets (LLDP sends shutdown LLDPDUs).
	{Program: "cer-mclagd", Name: "mclag", Help: "MC-LAG", Always: true, Nice: -10, OOM: -900, Caps: netCaps, RestartDelay: 100 * time.Millisecond,
		StopStage: 2, StopTimeout: 5 * time.Second},
	{Program: "cer-rstpd", Name: "rstp", Help: "RSTP", Always: true, Nice: -10, OOM: -900, Caps: netCaps, RestartDelay: 100 * time.Millisecond,
		StopStage: 2},
	{Program: "cer-ribd", Name: "routing", Help: "routing table", Always: true, Nice: -5, OOM: -500, Caps: netCaps, StopStage: 2},
	{Program: "cer-lldpd", Name: "lldp", Help: "LLDP", Always: true, OOM: -500, Caps: netCaps, StopStage: 2},
	{Program: "cer-dhcpcd", Name: "dhcp", Help: "DHCP client", Always: true, OOM: -500, Caps: netCaps, StopStage: 2},
	{Program: "cer-ntpd", Name: "ntp", Help: "NTP client", Always: true, OOM: -500, Caps: []string{"CAP_SYS_TIME", "CAP_NET_RAW"}, StopStage: 2},
	// Stage 3: LACP.
	{Program: "cer-lacpd", Name: "lacp", Help: "LACP", Always: true, Nice: -10, OOM: -900, Caps: netCaps, RestartDelay: 100 * time.Millisecond,
		StopStage: 3},
	// Stage 4: syslog last, so the shutdown's own messages still go out.
	{Program: "cer-syslogd", Name: "syslog", Help: "remote syslog", Always: true, Nice: 10, IOIdle: true, Caps: netCaps,
		StopStage: 4, StopTimeout: 5 * time.Second},
}

// Find returns a daemon by restart name or program.
func Find(name string) (Daemon, bool) {
	for _, d := range Daemons {
		if d.Name == name || d.Program == name {
			return d, true
		}
	}
	return Daemon{}, false
}

var unitTemplate = template.Must(template.New("unit").Parse(`# Written by switchd (reference 1.9); changes are overwritten.
[Unit]
Description=cerOS {{.D.Help}} ({{.D.Program}})
Documentation=file:///usr/share/doc/switchd/config-reference.md
# Stopped after switchd at shutdown: switchd stops the daemons itself, in
# order (reference 1.9).
Before=switchd.service
StartLimitIntervalSec=0

[Service]
Type=notify
NotifyAccess=main
ExecStart={{.Exec}}
Restart=always
RestartSec={{.RestartMs}}ms
WatchdogSec=10s
# The daemon finishes within its budget (-stop-timeout); then it is killed.
TimeoutStopSec={{.StopSec}}s
SendSIGKILL=yes
{{- if .D.RealTime}}
CPUSchedulingPolicy=fifo
CPUSchedulingPriority={{.D.RealTime}}
{{- else}}
Nice={{.D.Nice}}
{{- end}}
{{- if .D.IOIdle}}
IOSchedulingClass=idle
{{- end}}
OOMScoreAdjust={{.D.OOM}}
{{- if .Lim.GoLimit}}
# Memory slots (reference 5.1): Go's limit, the hard limit, and memory
# kept for the daemon under pressure.
Environment=GOMEMLIMIT={{.Lim.GoLimit}}
MemoryMax={{.Lim.Max}}
{{- end}}
{{- if .Lim.Min}}
MemoryMin={{.Lim.Min}}
{{- end}}
# No core dumps (reference 1.9): the stack trace is in the log.
LimitCORE=0
CapabilityBoundingSet={{.Caps}}
NoNewPrivileges=yes
ProtectHome=yes
PrivateTmp=yes
`))

// Limits are a daemon's memory limits from the memory slots (zero: none).
type Limits struct {
	GoLimit, Max, Min uint64
}

// Unit renders a daemon's unit for the program at path with args.
func Unit(d Daemon, path string, args []string, lim Limits) string {
	delay := d.RestartDelay
	if delay == 0 {
		delay = 200 * time.Millisecond
	}
	var b bytes.Buffer
	args = append(slices.Clone(args), "-stop-timeout", d.stopTimeout().String())
	unitTemplate.Execute(&b, struct {
		D         Daemon
		Lim       Limits
		Exec      string
		RestartMs int64
		Caps      string
		StopSec   int
	}{d, lim, strings.Join(append([]string{path}, args...), " "), delay.Milliseconds(), strings.Join(d.Caps, " "),
		int((d.stopTimeout() + 2*time.Second + time.Second - 1) / time.Second)})
	return b.String()
}

// UnitState is what systemd reports about a unit.
type UnitState struct {
	Loaded bool   // a unit file exists
	Active string // active, activating, deactivating, inactive, failed
	Sub    string // running, auto-restart, dead, ...
	// Result of the last run: success, exit-code, signal, core-dump,
	// watchdog, timeout, start-limit-hit, ...
	Result     string
	PID        int
	NRestarts  int
	ExitCode   int // 1 exited, 2 killed, 3 dumped (ExecMainCode)
	ExitStatus int // exit status or signal number
	Since      time.Time
	Memory     uint64
	CPU        time.Duration
}

// Running: the daemon's process runs.
func (u UnitState) Running() bool { return u.Active == "active" || u.Active == "reloading" }

// Backend executes units (systemd; a fake in tests).
type Backend interface {
	// Installed reports whether a program exists.
	Installed(path string) bool
	// WriteUnit writes a unit file and reports whether it changed.
	WriteUnit(unit, content string) (changed bool, err error)
	Reload() error
	Start(unit string) error
	Stop(unit string) error
	Restart(unit string) error
	Show(units []string) (map[string]UnitState, error)
	// StopWait stops a unit and returns when it has ended (or after
	// timeout); Kill ends it at once.
	StopWait(unit string, timeout time.Duration) error
	Kill(unit string) error
}

// Status is one program for show system processes.
type Status struct {
	Program     string
	Name        string
	State       string // running, restarting, failed, stopped, not installed
	PID         int
	Since       time.Time
	Restarts    int // in the last hour
	LastFailure string
	FailedAt    time.Time
	Memory      uint64
	CPU         time.Duration
	Scheduling  string
}

// Supervisor manages the daemons of one member.
type Supervisor struct {
	Backend Backend
	Log     *slog.Logger
	// Dir is where the programs are (next to switchd).
	Dir string
	// Args are the arguments of every daemon (member, directories).
	Args []string
	// Wanted reports which daemons that do not run always are needed now
	// (by program).
	Wanted func() map[string]bool
	// Beat is called after every step (the watchdog of switchd's loops;
	// nil: none).
	Beat func()
	// Daemons overrides the package table (tests).
	Daemons []Daemon
	// PlannedDir marks a restart the supervisor makes on purpose (a file
	// per program, svc.PlannedRestartDir; "": none): the daemon may then
	// restart gracefully (OSPF announces a grace period, reference 5.13),
	// since the kernel keeps forwarding meanwhile.
	PlannedDir string
	// StoppedFile keeps the daemons stopped by request (request daemon
	// stop) until the reboot: it is on a tmpfs (/run). "": in memory only.
	StoppedFile string
	// Alarms gets the daemons' faults (show system alarms; nil: none).
	// The set's owner logs and announces them (with the member).
	Alarms *alarms.Set
	// Limits are a daemon's memory limits (nil: none). They must not
	// change while switchd runs: a changed unit restarts the daemon.
	Limits func(program string) Limits

	mu       sync.Mutex
	tracked  map[string]*tracked
	units    map[string]string // unit -> content written
	stopping bool              // Shutdown runs: nothing is started any more
	stopped  map[string]bool   // by program: stopped by request until the reboot
}

type tracked struct {
	last      UnitState
	seen      bool
	restarts  []time.Time
	failure   string
	failedAt  time.Time
	down      bool      // reported as failed, recovery not yet reported
	expectEnd time.Time // a planned restart or stop until then
	missingAt time.Time // "not installed" reported at
	startAt   time.Time // last start attempt
	checkedAt time.Time // last state query
}

// idleCheck is how often a unit that is to stay stopped is looked at.
const idleCheck = 30 * time.Second

func (s *Supervisor) daemons() []Daemon {
	if s.Daemons != nil {
		return s.Daemons
	}
	return Daemons
}

func (s *Supervisor) path(d Daemon) string { return s.Dir + "/" + d.Program }

// Run checks every second until ctx ends.
func (s *Supervisor) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	s.Step(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.Step(now)
		}
		if s.Beat != nil {
			s.Beat()
		}
	}
}

// Step converges the units once.
func (s *Supervisor) Step(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return
	}
	if s.tracked == nil {
		s.tracked, s.units = map[string]*tracked{}, map[string]string{}
		s.stopped = s.loadStopped()
	}
	var wanted map[string]bool
	if s.Wanted != nil {
		wanted = s.Wanted()
	}
	var units []string
	changed := map[string]bool{}
	for _, d := range s.daemons() {
		t := s.tracked[d.Program]
		if t == nil {
			t = &tracked{}
			s.tracked[d.Program] = t
		}
		want := d.Always || wanted[d.Program]
		if d.External {
			units = append(units, d.Unit())
			continue
		}
		if !s.Backend.Installed(s.path(d)) {
			if want && now.Sub(t.missingAt) >= time.Minute {
				t.missingAt = now
				s.raise("missing "+d.Program, alarms.Major, fmt.Sprintf("%s is not installed (%s): its function is missing", d.Program, s.path(d)))
				s.Log.Error("program not installed", "program", d.Program, "path", s.path(d))
			}
			continue
		}
		t.missingAt = time.Time{}
		s.clear("missing " + d.Program)
		if s.stopped[d.Program] {
			s.raise("stopped "+d.Program, alarms.Minor, fmt.Sprintf("%s is stopped until the reboot (request daemon stop)", d.Program))
		} else {
			s.clear("stopped " + d.Program)
		}
		var lim Limits
		if s.Limits != nil {
			lim = s.Limits(d.Program)
		}
		content := Unit(d, s.path(d), s.Args, lim)
		if s.units[d.Unit()] != content {
			ch, err := s.Backend.WriteUnit(d.Unit(), content)
			if err != nil {
				s.Log.Error("unit not written", "unit", d.Unit(), "err", err)
				continue
			}
			s.units[d.Unit()] = content
			changed[d.Unit()] = ch
		}
		// A unit that is to stay stopped and is stopped is checked every
		// 30 s only: systemd unloads an inactive unit, and each query loads
		// it from disk again (drop-in directories searched).
		idle := !(want && !s.stopped[d.Program]) && t.seen && !changed[d.Unit()] &&
			(t.last.Active == "inactive" || t.last.Active == "failed")
		if idle && now.Sub(t.checkedAt) < idleCheck {
			continue
		}
		t.checkedAt = now
		units = append(units, d.Unit())
	}
	if slices.Contains(slices.Collect(maps.Values(changed)), true) {
		if err := s.Backend.Reload(); err != nil {
			s.Log.Error("systemctl daemon-reload", "err", err)
		}
	}
	if len(units) == 0 {
		return
	}
	states, err := s.Backend.Show(units)
	if err != nil {
		s.Log.Warn("unit states", "err", err)
		return
	}
	for _, d := range s.daemons() {
		u, ok := states[d.Unit()]
		if !ok {
			continue
		}
		t := s.tracked[d.Program]
		want := (d.Always || wanted[d.Program]) && !s.stopped[d.Program]
		s.observe(d, t, u, now)
		if d.External {
			continue
		}
		switch {
		case want && changed[d.Unit()] && u.Running():
			// A new unit (new program version, arguments): restart.
			t.expectEnd = now.Add(10 * time.Second)
			s.Log.Info("daemon restarted for its new unit", "program", d.Program)
			s.markPlanned(d.Program)
			if err := s.Backend.Restart(d.Unit()); err != nil {
				s.Log.Error("restart", "program", d.Program, "err", err)
			}
		case want && (u.Active == "inactive" || u.Active == "failed") && now.Sub(t.startAt) >= 2*time.Second:
			// Not started yet, or systemd gave up (a start that failed).
			t.startAt = now
			if t.seen && u.Active == "failed" {
				s.Log.Warn("daemon failed; starting it again", "program", d.Program)
			}
			if err := s.Backend.Start(d.Unit()); err != nil {
				s.Log.Error("start", "program", d.Program, "err", err)
				if !t.down {
					t.down, t.failure, t.failedAt = true, "cannot be started: "+err.Error(), now
					s.raise("failed "+d.Program, alarms.Major, fmt.Sprintf("%s cannot be started: %v", d.Program, err))
				}
			}
		case !want && u.Active != "inactive" && u.Active != "failed" && u.Active != "deactivating":
			t.expectEnd = now.Add(10 * time.Second)
			s.Log.Info("daemon no longer needed; stopped", "program", d.Program)
			if err := s.Backend.Stop(d.Unit()); err != nil {
				s.Log.Error("stop", "program", d.Program, "err", err)
			}
		}
	}
}

// observe compares a unit's state with the last one and reports failures
// and recoveries.
func (s *Supervisor) observe(d Daemon, t *tracked, u UnitState, now time.Time) {
	prev, seen := t.last, t.seen
	t.last, t.seen = u, true
	hour := now.Add(-time.Hour)
	t.restarts = slices.DeleteFunc(t.restarts, func(x time.Time) bool { return x.Before(hour) })
	if !seen {
		return
	}
	planned := now.Before(t.expectEnd)
	ended := u.NRestarts > prev.NRestarts ||
		(prev.Running() && !u.Running() && u.Active != "deactivating") ||
		(prev.Running() && u.Running() && u.PID != prev.PID && u.PID != 0 && prev.PID != 0)
	if ended && !planned && u.Result != "success" {
		t.restarts = append(t.restarts, now)
		t.failure, t.failedAt, t.down = describe(u), now, true
		s.raise("failed "+d.Program, alarms.Major, fmt.Sprintf("%s failed (%s); it is restarted", d.Program, t.failure))
		s.Log.Error(d.Program+" failed", "reason", t.failure, "restarts_last_hour", len(t.restarts))
	}
	if t.down && u.Running() && u.Sub == "running" {
		t.down = false
		s.clear("failed " + d.Program)
		s.Log.Info(d.Program+" runs again", "restarts_last_hour", len(t.restarts))
	}
}

func (s *Supervisor) raise(id, class, text string) {
	if s.Alarms != nil {
		s.Alarms.Raise("switchd/"+id, class, text)
	}
}

func (s *Supervisor) clear(id string) {
	if s.Alarms != nil {
		s.Alarms.Clear("switchd/" + id)
	}
}

// describe explains why a daemon ended.
func describe(u UnitState) string {
	switch u.Result {
	case "watchdog":
		return "hung: no sign of life for 10 s"
	case "signal":
		return "killed by signal " + SignalName(u.ExitStatus)
	case "core-dump":
		return "crashed with signal " + SignalName(u.ExitStatus)
	case "exit-code":
		return fmt.Sprintf("exited with status %d", u.ExitStatus)
	case "timeout":
		return "did not start or stop in time"
	case "", "success":
		if u.ExitCode == 2 || u.ExitCode == 3 {
			return "killed by signal " + SignalName(u.ExitStatus)
		}
		if u.ExitStatus != 0 {
			return fmt.Sprintf("exited with status %d", u.ExitStatus)
		}
		return "ended"
	}
	return u.Result
}

// RestartDaemon restarts a daemon on request (restart <name>): no
// failure is reported for it.
func (s *Supervisor) RestartDaemon(name string) error {
	d, ok := s.find(name)
	if !ok {
		return fmt.Errorf("unknown daemon %q", name)
	}
	if !s.Backend.Installed(s.path(d)) {
		return fmt.Errorf("%s is not installed", d.Program)
	}
	s.mu.Lock()
	if t := s.tracked[d.Program]; t != nil {
		t.expectEnd = time.Now().Add(10 * time.Second)
	}
	s.mu.Unlock()
	s.markPlanned(d.Program)
	return s.Backend.Restart(d.Unit())
}

// markPlanned tells a daemon that its coming stop is a restart (it reads
// and removes the mark when it stops).
func (s *Supervisor) markPlanned(program string) {
	if s.PlannedDir == "" {
		return
	}
	if err := hwio.MkdirAll(s.PlannedDir, 0o755); err == nil {
		_ = hwio.WriteFile(filepath.Join(s.PlannedDir, program), nil, 0o644)
	}
}

// StopDaemon stops a daemon on request (request daemon stop) until the
// reboot or StartDaemon: the supervisor does not start it again.
func (s *Supervisor) StopDaemon(name string) (Daemon, error) {
	d, ok := s.find(name)
	if !ok || d.External {
		return d, fmt.Errorf("unknown daemon %q", name)
	}
	s.mu.Lock()
	if s.tracked == nil {
		s.tracked, s.units = map[string]*tracked{}, map[string]string{}
		s.stopped = s.loadStopped()
	}
	s.stopped[d.Program] = true
	if t := s.tracked[d.Program]; t != nil {
		t.expectEnd = time.Now().Add(time.Minute)
	}
	err := s.saveStopped()
	s.mu.Unlock()
	if err != nil {
		s.Log.Warn("stopped daemons not saved (a switchd restart starts them again)", "err", err)
	}
	return d, s.Backend.Stop(d.Unit())
}

// StartDaemon ends a StopDaemon; the next step starts the daemon if it
// is needed.
func (s *Supervisor) StartDaemon(name string) (Daemon, error) {
	d, ok := s.find(name)
	if !ok || d.External {
		return d, fmt.Errorf("unknown daemon %q", name)
	}
	s.mu.Lock()
	was := s.stopped[d.Program]
	delete(s.stopped, d.Program)
	err := s.saveStopped()
	if t := s.tracked[d.Program]; t != nil {
		t.startAt = time.Time{}
	}
	s.mu.Unlock()
	if !was {
		return d, fmt.Errorf("%s was not stopped", d.Program)
	}
	return d, err
}

// Stopped reports whether a program is stopped by request.
func (s *Supervisor) Stopped(program string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped == nil {
		s.stopped = s.loadStopped()
	}
	return s.stopped[program]
}

func (s *Supervisor) loadStopped() map[string]bool {
	out := map[string]bool{}
	if s.StoppedFile == "" {
		return out
	}
	raw, err := hwio.ReadFile(s.StoppedFile)
	if err != nil {
		return out
	}
	for _, l := range strings.Fields(string(raw)) {
		out[l] = true
	}
	return out
}

func (s *Supervisor) saveStopped() error {
	if s.StoppedFile == "" {
		return nil
	}
	names := slices.Sorted(maps.Keys(s.stopped))
	return hwio.WriteFileAtomic(s.StoppedFile, []byte(strings.Join(names, "\n")+"\n"), 0o644)
}

func (s *Supervisor) find(name string) (Daemon, bool) {
	for _, d := range s.daemons() {
		if d.Name == name || d.Program == name {
			return d, true
		}
	}
	return Daemon{}, false
}

// Status reports every daemon.
func (s *Supervisor) Status() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Status
	for _, d := range s.daemons() {
		st := Status{Program: d.Program, Name: d.Name, Scheduling: d.Scheduling(), State: "stopped"}
		t := s.tracked[d.Program]
		switch {
		case !d.External && !s.Backend.Installed(s.path(d)):
			st.State = "not installed"
		case t != nil && t.seen:
			u := t.last
			st.PID, st.Since, st.Memory, st.CPU = u.PID, u.Since, u.Memory, u.CPU
			switch {
			case u.Running() && u.Sub == "running":
				st.State = "running"
			case u.Active == "activating" || u.Sub == "auto-restart":
				st.State = "restarting"
			case u.Active == "failed":
				st.State = "failed"
			}
		}
		if s.stopped[d.Program] && st.State != "running" {
			st.State = "stopped (request daemon stop)"
		}
		if t != nil {
			st.Restarts, st.LastFailure, st.FailedAt = len(t.restarts), t.failure, t.failedAt
		}
		out = append(out, st)
	}
	return out
}

// Shutdown stops every daemon (switchd stops, or the system shuts down):
// stage by stage, the daemons of a stage in parallel; each may take its
// stop budget to finish its work, one that takes longer (it hangs) is
// killed. Nothing is started any more afterwards. External units
// (switchd-update) keep running. ctx bounds the whole shutdown.
func (s *Supervisor) Shutdown(ctx context.Context) {
	s.mu.Lock()
	s.stopping = true
	for _, t := range s.tracked {
		t.expectEnd = time.Now().Add(time.Hour) // not failures
	}
	s.mu.Unlock()
	stages := map[int][]Daemon{}
	for _, d := range s.daemons() {
		if !d.External && s.Backend.Installed(s.path(d)) {
			stages[d.StopStage] = append(stages[d.StopStage], d)
		}
	}
	for _, st := range slices.Sorted(maps.Keys(stages)) {
		var wg sync.WaitGroup
		for _, d := range stages[st] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.stopOne(ctx, d)
			}()
		}
		wg.Wait()
	}
}

// stopOne stops a daemon within its budget (and the unit's kill timeout),
// killing it when it does not end.
func (s *Supervisor) stopOne(ctx context.Context, d Daemon) {
	start := time.Now()
	budget := d.stopTimeout() + 3*time.Second // the unit kills at budget + 2 s
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < budget {
		budget = max(time.Until(dl), time.Second) // late, but still a moment to close
	}
	err := s.Backend.StopWait(d.Unit(), budget)
	if err == nil {
		s.Log.Info(d.Program+" stopped", "took", time.Since(start).Round(time.Millisecond))
		return
	}
	s.Log.Warn(d.Program+" did not stop in time; killed", "after", time.Since(start).Round(time.Millisecond), "err", err)
	if err := s.Backend.Kill(d.Unit()); err != nil {
		s.Log.Error(d.Program+" not killed", "err", err)
	}
}
