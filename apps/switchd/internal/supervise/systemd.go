package supervise

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
	"github.com/thxrben/cerium-switchd/lib/sys/sysexec"
	"golang.org/x/sys/unix"
)

// Systemd is the backend on a real system: unit files in UnitDir,
// systemctl for the rest.
type Systemd struct {
	UnitDir string // /etc/systemd/system
	// Systemctl runs systemctl (nil: the program).
	Systemctl func(args ...string) (string, error)
	// BusPath is systemd's private D-Bus socket for reading unit states
	// ("": systemctl show only).
	BusPath string
	Log     *slog.Logger

	busOnce   sync.Once
	bus       *unitBus
	busFailed bool // reported once until it works again
}

func (s *Systemd) systemctl(args ...string) (string, error) {
	return s.systemctlWait(0, args...)
}

// systemctlWait runs systemctl with a deadline (0: sysexec.Default).
func (s *Systemd) systemctlWait(d time.Duration, args ...string) (string, error) {
	if s.Systemctl != nil {
		return s.Systemctl(args...)
	}
	out, err := sysexec.Command("systemctl", args...).WithTimeout(d).CombinedOutput(context.Background())
	if err != nil {
		return string(out), fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Installed reports whether an executable program is at path.
func (s *Systemd) Installed(path string) bool {
	fi, err := hwio.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// WriteUnit writes the unit file atomically when its content differs.
func (s *Systemd) WriteUnit(unit, content string) (bool, error) {
	path := filepath.Join(s.UnitDir, unit)
	if have, err := hwio.ReadFile(path); err == nil && string(have) == content {
		return false, nil
	}
	tmp := path + ".switchd-tmp"
	if err := hwio.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false, err
	}
	if err := hwio.Rename(tmp, path); err != nil {
		hwio.Remove(tmp)
		return false, err
	}
	return true, nil
}

func (s *Systemd) Reload() error { _, err := s.systemctl("daemon-reload"); return err }

func (s *Systemd) Start(unit string) error {
	_, err := s.systemctl("start", "--no-block", unit)
	return err
}

func (s *Systemd) Stop(unit string) error {
	_, err := s.systemctl("stop", "--no-block", unit)
	return err
}

func (s *Systemd) Restart(unit string) error {
	_, err := s.systemctl("restart", "--no-block", unit)
	return err
}

// StopWait stops a unit and waits until it has ended (systemd kills it at
// its TimeoutStopSec); timeout bounds the wait here.
func (s *Systemd) StopWait(unit string, timeout time.Duration) error {
	_, err := s.systemctlWait(timeout, "stop", unit)
	if errors.Is(err, hwio.ErrTimeout) {
		return fmt.Errorf("%s still running after %s", unit, timeout)
	}
	return err
}

// Kill ends a unit's processes at once.
func (s *Systemd) Kill(unit string) error {
	_, err := s.systemctl("kill", "--signal=SIGKILL", unit)
	return err
}

var showProps = "Id,LoadState,ActiveState,SubState,Result,MainPID,NRestarts,ExecMainCode,ExecMainStatus,ActiveEnterTimestamp,MemoryCurrent,CPUUsageNSec"

// Show reads the units' states: over systemd's D-Bus socket, or with one
// systemctl call for all when that is not available.
func (s *Systemd) Show(units []string) (map[string]UnitState, error) {
	if s.Systemctl == nil && s.BusPath != "" {
		s.busOnce.Do(func() { s.bus = &unitBus{path: s.BusPath} })
		res, err := s.bus.Show(units)
		if err == nil {
			return res, nil
		}
		if !s.busFailed && s.Log != nil {
			s.Log.Warn("unit states over D-Bus failed; systemctl show is used", "err", err)
		}
		s.busFailed = true
	} else if s.busFailed {
		s.busFailed = false
	}
	out, err := s.systemctl(append([]string{"show", "--timestamp=unix", "-p", showProps}, units...)...)
	if err != nil {
		return nil, err
	}
	return ParseShow(out)
}

// ParseShow parses the output of systemctl show for several units
// (blocks separated by empty lines).
func ParseShow(out string) (map[string]UnitState, error) {
	res := map[string]UnitState{}
	var id string
	var u UnitState
	flush := func() {
		if id != "" {
			res[id] = u
		}
		id, u = "", UnitState{}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		num := func() int { n, _ := strconv.Atoi(v); return n }
		switch k {
		case "Id":
			id = v
		case "LoadState":
			u.Loaded = v == "loaded"
		case "ActiveState":
			u.Active = v
		case "SubState":
			u.Sub = v
		case "Result":
			u.Result = v
		case "MainPID":
			u.PID = num()
		case "NRestarts":
			u.NRestarts = num()
		case "ExecMainCode":
			u.ExitCode = num()
		case "ExecMainStatus":
			u.ExitStatus = num()
		case "ActiveEnterTimestamp":
			if sec, err := strconv.ParseInt(strings.TrimPrefix(v, "@"), 10, 64); err == nil && sec > 0 {
				u.Since = time.Unix(sec, 0)
			}
		case "MemoryCurrent":
			u.Memory, _ = strconv.ParseUint(v, 10, 64) // "[not set]" stays 0
		case "CPUUsageNSec":
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				u.CPU = time.Duration(n)
			}
		}
	}
	flush()
	if len(res) == 0 && strings.TrimSpace(out) != "" {
		return nil, errors.New("systemctl show: no units in the output")
	}
	return res, nil
}

// SignalName returns "SEGV" for 11.
func SignalName(n int) string {
	if name := unix.SignalName(syscall.Signal(n)); name != "" {
		return strings.TrimPrefix(name, "SIG")
	}
	return strconv.Itoa(n)
}
