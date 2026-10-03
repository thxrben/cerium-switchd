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

// Processes is what the processes commands need from the member.
type Processes interface {
	// Processes lists switchd and the daemons of this member.
	Processes() ([]Process, error)
	// RestartDaemon restarts a daemon by its restart name.
	RestartDaemon(name, user string) error
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

// restartCommand is "restart <daemon>" (reference 1.9).
func restartCommand() *command {
	cmd := &command{name: "restart", help: "Restart a daemon (hitless where its protocol allows it)", class: commit.SuperUser}
	for _, d := range supervise.Daemons {
		name := d.Name
		cmd.sub = append(cmd.sub, &command{name: name, help: "Restart " + d.Program + " (" + d.Help + ")", class: commit.SuperUser,
			run: func(sh *Shell, c *call) error {
				if err := noArgs(c); err != nil {
					return err
				}
				p, err := sh.processes()
				if err != nil {
					return err
				}
				if err := p.RestartDaemon(name, sh.env.User); err != nil {
					return err
				}
				fmt.Fprintf(c.out, "%s restarted\n", d.Program)
				return nil
			}})
	}
	return cmd
}
