package access

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"mclag/internal/model"
)

// Consoles starts logins (serial-getty@<tty> with the CLI as login shell
// via the account) on serial consoles (reference 5.1 system ports).
type Consoles struct {
	SysRoot   string // "/sys"
	UnitDir   string // "/etc/systemd/system"
	StateFile string
	Log       *slog.Logger
	// Systemctl runs systemctl (replaceable in tests).
	Systemctl func(args ...string) error
}

// consoleState records the gettys switchd manages (tty -> speed, 0 = masked).
type consoleState struct {
	TTYs map[string]int `json:"ttys"`
}

// Detect lists serial consoles: ttyS ports with a UART, USB serial adapters
// and the kernel console.
func (c *Consoles) Detect() []string {
	seen := map[string]bool{}
	ents, _ := os.ReadDir(filepath.Join(c.SysRoot, "class", "tty"))
	for _, e := range ents {
		n := e.Name()
		switch {
		case strings.HasPrefix(n, "ttyS"):
			raw, err := os.ReadFile(filepath.Join(c.SysRoot, "class", "tty", n, "type"))
			if t, _ := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && t != 0 {
				seen[n] = true
			}
		case strings.HasPrefix(n, "ttyUSB"), strings.HasPrefix(n, "ttyACM"):
			seen[n] = true
		}
	}
	if raw, err := os.ReadFile(filepath.Join(c.SysRoot, "class", "tty", "console", "active")); err == nil {
		for _, n := range strings.Fields(string(raw)) {
			if !strings.HasPrefix(n, "tty") || strings.TrimLeft(n[3:], "0123456789") == "" {
				continue // virtual terminals (tty0, tty1, …) have their own gettys
			}
			seen[n] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Desired returns tty -> speed for the consoles that get a login, and the
// ones that are explicitly disabled.
func (c *Consoles) Desired(cfg *model.Config) (map[string]int, map[string]bool) {
	want, disabled := map[string]int{}, map[string]bool{}
	if cfg.System.AutoConsole {
		for _, t := range c.Detect() {
			want[t] = 115200
		}
	}
	for t, con := range cfg.System.Consoles {
		if con.Disabled {
			delete(want, t)
			disabled[t] = true
			continue
		}
		want[t] = con.Speed
	}
	return want, disabled
}

func (c *Consoles) dropIn(tty string) string {
	return filepath.Join(c.UnitDir, "serial-getty@"+tty+".service.d", "switchd.conf")
}

func (c *Consoles) load() consoleState {
	st := consoleState{TTYs: map[string]int{}}
	if raw, err := os.ReadFile(c.StateFile); err == nil {
		_ = json.Unmarshal(raw, &st)
		if st.TTYs == nil {
			st.TTYs = map[string]int{}
		}
	}
	return st
}

// Sync converges the managed gettys.
func (c *Consoles) Sync(cfg *model.Config) error {
	st := c.load()
	want, disabled := c.Desired(cfg)
	var errs []error
	reload := false
	type change struct {
		tty   string
		start bool
	}
	var changes []change
	for tty, speed := range want {
		if st.TTYs[tty] == speed {
			continue
		}
		unit := fmt.Sprintf("[Service]\nExecStart=\nExecStart=-/sbin/agetty --noreset --noclear %d %%I $TERM\n", speed)
		if err := os.MkdirAll(filepath.Dir(c.dropIn(tty)), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.WriteFile(c.dropIn(tty), []byte(unit), 0o644); err != nil {
			errs = append(errs, err)
			continue
		}
		reload = true
		changes = append(changes, change{tty, true})
	}
	for tty := range disabled {
		if s, ok := st.TTYs[tty]; ok && s == 0 {
			continue
		}
		changes = append(changes, change{tty, false})
	}
	for tty := range st.TTYs {
		if _, ok := want[tty]; !ok && !disabled[tty] {
			changes = append(changes, change{tty, false})
		}
	}
	if reload {
		if err := c.Systemctl("daemon-reload"); err != nil {
			errs = append(errs, err)
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].tty < changes[j].tty })
	for _, ch := range changes {
		unit := "serial-getty@" + ch.tty + ".service"
		switch {
		case ch.start:
			_ = c.Systemctl("unmask", unit)
			if err := c.Systemctl("enable", unit); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := c.Systemctl("restart", unit); err != nil {
				errs = append(errs, err)
				continue
			}
			st.TTYs[ch.tty] = want[ch.tty]
			c.Log.Info("console login started", "tty", ch.tty, "speed", want[ch.tty])
		case disabled[ch.tty]:
			// Explicitly disabled: also the OS's own getty (kernel console).
			if err := c.Systemctl("mask", "--now", unit); err != nil {
				errs = append(errs, err)
				continue
			}
			_ = os.Remove(c.dropIn(ch.tty))
			st.TTYs[ch.tty] = 0
			c.Log.Info("console login disabled", "tty", ch.tty)
		default:
			// No longer wanted (unplugged adapter, auto-detection off): undo
			// what switchd did.
			if st.TTYs[ch.tty] == 0 {
				_ = c.Systemctl("unmask", unit)
			} else {
				_ = c.Systemctl("disable", "--now", unit)
				_ = os.Remove(c.dropIn(ch.tty))
			}
			delete(st.TTYs, ch.tty)
		}
	}
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(c.StateFile, raw, 0o600); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
