package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/supervise"
)

// Process is one program for "show system processes" (reference 1.9).
type Process struct {
	Program     string
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

// Hang is a system call that does not return (a device that stopped
// answering).
type Hang struct {
	Program  string
	Resource string
	Call     string
	Since    time.Time
}

// Hangs is implemented by members that report calls that do not return.
type Hangs interface {
	Hangs() []Hang
}

// Processes is what the processes commands need from the member.
type Processes interface {
	// Processes lists switchd and the daemons of this member.
	Processes() ([]Process, error)
	// RestartDaemon restarts a daemon by its restart name.
	RestartDaemon(name, user string) error
	// StopDaemon stops it until the reboot; it returns what it does.
	StopDaemon(name, user string) (string, error)
	// StartDaemon ends a StopDaemon.
	StartDaemon(name, user string) error
}

func (sh *Shell) processes() (Processes, error) {
	p, ok := sh.env.Ops.(Processes)
	if sh.env.Ops == nil || !ok {
		return nil, errors.New("process information is not available")
	}
	return p, nil
}

func (sh *Shell) showProcesses(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	p, err := sh.processes()
	if err != nil {
		return err
	}
	ps, err := p.Processes()
	if err != nil {
		return err
	}
	now := time.Now()
	fmt.Fprintf(c.out, "%-15s %-13s %-8s %-10s %-9s %-8s %-9s %s\n", "Program", "State", "PID", "Uptime", "Restarts", "Memory", "CPU", "Scheduling")
	var failures []string
	for _, x := range ps {
		pid, up, mem, cpu := "-", "-", "-", "-"
		if x.PID > 0 {
			pid = fmt.Sprint(x.PID)
		}
		if !x.Since.IsZero() && x.State == "running" {
			up = fmtDuration(now.Sub(x.Since))
		}
		if x.Memory > 0 {
			mem = fmtBytes(x.Memory)
		}
		if x.CPU > 0 {
			cpu = x.CPU.Round(time.Second).String()
		}
		fmt.Fprintf(c.out, "%-15s %-13s %-8s %-10s %-9d %-8s %-9s %s\n", x.Program, x.State, pid, up, x.Restarts, mem, cpu, x.Scheduling)
		if x.LastFailure != "" {
			failures = append(failures, fmt.Sprintf("  %s: %s (%s ago)", x.Program, x.LastFailure, fmtDuration(now.Sub(x.FailedAt))))
		}
	}
	c.out.WriteString("Restarts: in the last hour.\n")
	if len(failures) > 0 {
		c.out.WriteString("Last failures:\n" + strings.Join(failures, "\n") + "\n")
	}
	if h, ok := p.(Hangs); ok {
		if hs := h.Hangs(); len(hs) > 0 {
			c.out.WriteString("ALARM: calls that do not return (the device does not answer):\n")
			for _, x := range hs {
				fmt.Fprintf(c.out, "  %s: %s: %s, for %s\n", x.Program, x.Resource, x.Call, fmtDuration(now.Sub(x.Since)))
			}
		}
	}
	return nil
}

func fmtBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%dB", b)
}

// daemonCommands are "request daemon restart|stop|start <daemon>" and the
// short form "restart <daemon>" (reference 1.9).
func daemonCommand() *command {
	mk := func(verb, help string, run func(sh *Shell, c *call, p Processes, d supervise.Daemon) error) *command {
		cmd := &command{name: verb, help: help, class: commit.SuperUser}
		for _, d := range supervise.Daemons {
			if d.External && verb != "restart" {
				continue // the update daemon is never stopped
			}
			cmd.sub = append(cmd.sub, &command{name: d.Name, help: verb + " " + d.Program + " (" + d.Help + ")", class: commit.SuperUser,
				perMember: true, run: func(sh *Shell, c *call) error {
					if err := noArgs(c); err != nil {
						return err
					}
					p, err := sh.processes()
					if err != nil {
						return err
					}
					return run(sh, c, p, d)
				}})
		}
		return cmd
	}
	return &command{name: "daemon", help: "Restart, stop or start a daemon", class: commit.SuperUser, sub: []*command{
		mk("restart", "Restart a daemon (hitless where its protocol allows it)", restartDaemon),
		mk("stop", "Stop a daemon until the reboot (its function is missing meanwhile)", func(sh *Shell, c *call, p Processes, d supervise.Daemon) error {
			help, err := p.StopDaemon(d.Name, sh.env.User)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.out, "%s stopped until the reboot or 'request daemon start %s': %s is not available meanwhile\n", d.Program, d.Name, help)
			return nil
		}),
		mk("start", "Start a daemon stopped by 'request daemon stop'", func(sh *Shell, c *call, p Processes, d supervise.Daemon) error {
			if err := p.StartDaemon(d.Name, sh.env.User); err != nil {
				return err
			}
			fmt.Fprintf(c.out, "%s may run again (it starts if it is needed)\n", d.Program)
			return nil
		}),
	}}
}

func restartDaemon(sh *Shell, c *call, p Processes, d supervise.Daemon) error {
	if err := p.RestartDaemon(d.Name, sh.env.User); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%s restarted\n", d.Program)
	return nil
}

// restartCommand is the short form "restart <daemon>".
func restartCommand() *command {
	cmd := daemonCommand().sub[0]
	return &command{name: "restart", help: cmd.help, class: commit.SuperUser, sub: cmd.sub, hidden: true}
}
