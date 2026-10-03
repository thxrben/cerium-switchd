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
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
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
	{Program: "cer-lldpd", Name: "lldp", Help: "LLDP", Always: true, OOM: -500, Caps: netCaps},
	{Program: "cer-ntpd", Name: "ntp", Help: "NTP client", Always: true, OOM: -500, Caps: []string{"CAP_SYS_TIME", "CAP_NET_RAW"}},
	{Program: "cer-syslogd", Name: "syslog", Help: "remote syslog", Always: true, Nice: 10, IOIdle: true, Caps: netCaps},
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
After=switchd.service
StartLimitIntervalSec=0

[Service]
Type=notify
NotifyAccess=main
ExecStart={{.Exec}}
Restart=always
RestartSec={{.RestartMs}}ms
WatchdogSec=10s
TimeoutStopSec=5s
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
CapabilityBoundingSet={{.Caps}}
NoNewPrivileges=yes
ProtectHome=yes
PrivateTmp=yes
`))

// Unit renders a daemon's unit for the program at path with args.
func Unit(d Daemon, path string, args []string) string {
	delay := d.RestartDelay
	if delay == 0 {
		delay = 200 * time.Millisecond
	}
	var b bytes.Buffer
	unitTemplate.Execute(&b, struct {
		D         Daemon
		Exec      string
		RestartMs int64
		Caps      string
	}{d, strings.Join(append([]string{path}, args...), " "), delay.Milliseconds(), strings.Join(d.Caps, " ")})
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
	// Member is this member's id (in notices).
	Member int
	// Notify reaches every CLI session of the stack.
	Notify func(text string)
	// Wanted reports which daemons that do not run always are needed now
	// (by program).
	Wanted func() map[string]bool
	// Daemons overrides the package table (tests).
	Daemons []Daemon

	mu      sync.Mutex
	tracked map[string]*tracked
	units   map[string]string // unit -> content written
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
}

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
	}
}

func (s *Supervisor) note(text string) {
	if s.Member > 0 {
		text = fmt.Sprintf("member %d: %s", s.Member, text)
	}
	if s.Notify != nil {
		s.Notify(text)
	}
}

// Step converges the units once.
func (s *Supervisor) Step(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tracked == nil {
		s.tracked, s.units = map[string]*tracked{}, map[string]string{}
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
				s.Log.Error("program not installed", "program", d.Program, "path", s.path(d))
				s.note(fmt.Sprintf("%s is not installed (%s); its function is missing", d.Program, s.path(d)))
			}
			continue
		}
		t.missingAt = time.Time{}
		content := Unit(d, s.path(d), s.Args)
		if s.units[d.Unit()] != content {
			ch, err := s.Backend.WriteUnit(d.Unit(), content)
			if err != nil {
				s.Log.Error("unit not written", "unit", d.Unit(), "err", err)
				continue
			}
			s.units[d.Unit()] = content
			changed[d.Unit()] = ch
		}
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
		want := d.Always || wanted[d.Program]
		s.observe(d, t, u, now)
		if d.External {
			continue
		}
		switch {
		case want && changed[d.Unit()] && u.Running():
			// A new unit (new program version, arguments): restart.
			t.expectEnd = now.Add(10 * time.Second)
			s.Log.Info("daemon restarted for its new unit", "program", d.Program)
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
					s.note(fmt.Sprintf("%s cannot be started: %v", d.Program, err))
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
		s.Log.Error(d.Program+" failed", "reason", t.failure, "restarts_last_hour", len(t.restarts))
		s.note(fmt.Sprintf("%s failed (%s) and is restarted", d.Program, t.failure))
	}
	if t.down && u.Running() && u.Sub == "running" {
		t.down = false
		s.Log.Info(d.Program+" runs again", "restarts_last_hour", len(t.restarts))
		s.note(fmt.Sprintf("%s runs again (%s in the last hour)", d.Program, plural(len(t.restarts), "restart")))
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
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
	return s.Backend.Restart(d.Unit())
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
		if t != nil {
			st.Restarts, st.LastFailure, st.FailedAt = len(t.restarts), t.failure, t.failedAt
		}
		out = append(out, st)
	}
	return out
}
