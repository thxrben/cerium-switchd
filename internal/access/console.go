package access

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/internal/model"
)

// Consoles runs the CLI on the local consoles (reference 5.1 system
// ports): serial-getty@<tty> on serial ports and getty@ on the virtual
// terminals of a display. By default they log in root without a password;
// root's bash then starts the CLI through the profile hook.
type Consoles struct {
	SysRoot    string // "/sys"
	UnitDir    string // "/etc/systemd/system"
	ProfileDir string // "/etc/profile.d" ("" = no hook)
	StateFile  string
	Log        *slog.Logger
	// Systemctl runs systemctl (replaceable in tests).
	Systemctl func(args ...string) error
	// MainComm returns the command name of a unit's main process ("" = not
	// running). A getty whose process is no longer agetty has a user logged
	// in and is not restarted.
	MainComm func(unit string) string
}

// ProfileHook starts the CLI for interactive logins on local consoles. The
// CLI exports SWITCHD_SHELL to its shells, so "start shell" gets a plain
// bash. When the CLI ends normally the login ends too (the getty starts
// the CLI again); if it fails, the user stays in this shell.
const ProfileHook = `# Managed by switchd: the local consoles run the switch CLI.
if [ -z "$SWITCHD_SHELL" ] && [ -x ` + Shell + ` ]; then
	case "$-" in *i*)
		case "$(tty 2>/dev/null)" in
		/dev/tty[0-9]*|/dev/ttyS*|/dev/ttyUSB*|/dev/ttyACM*|/dev/ttyAMA*|/dev/hvc*)
			if SWITCHD_SHELL=console ` + Shell + `; then
				exit 0
			fi
			echo "The CLI failed; continuing in a Linux shell. Type 'cli' to start it again."
			;;
		esac
		;;
	esac
fi
`

const vtDropIn = "getty@.service.d"

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
	// The hook first: the gettys restarted below log in through it.
	if c.ProfileDir != "" {
		if _, err := writeIfChanged(filepath.Join(c.ProfileDir, "switchd-cli.sh"), ProfileHook, 0o644); err != nil {
			errs = append(errs, err)
		}
	}
	reload := false
	type change struct {
		tty   string
		start bool
	}
	var changes []change
	for tty, speed := range want {
		unit := serialUnit(speed, cfg.System.ConsoleLogin)
		if old, err := os.ReadFile(c.dropIn(tty)); err == nil && string(old) == unit && st.TTYs[tty] == speed {
			continue
		}
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
			st.TTYs[ch.tty] = want[ch.tty]
			if c.busy(unit) {
				c.Log.Info("console: settings apply when the current session ends", "tty", ch.tty)
				continue
			}
			if err := c.Systemctl("restart", unit); err != nil {
				errs = append(errs, err)
				continue
			}
			c.Log.Info("console CLI started", "tty", ch.tty, "speed", want[ch.tty])
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
	if err := c.syncVTs(cfg); err != nil {
		errs = append(errs, err)
	}
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(c.StateFile, raw, 0o600); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// serialUnit is the drop-in for serial-getty@<tty>.
func serialUnit(speed int, login bool) string {
	auto := "--autologin root "
	if login {
		auto = ""
	}
	return fmt.Sprintf("[Service]\nExecStart=\nExecStart=-/sbin/agetty %s--noreset --noclear %d %%I $TERM\n", auto, speed)
}

// vtUnit is the drop-in for getty@ (all virtual terminals).
const vtUnit = "[Service]\nExecStart=\nExecStart=-/sbin/agetty --autologin root --noreset --noclear - $TERM\n"

// busy reports whether someone is logged in through the getty unit.
func (c *Consoles) busy(unit string) bool {
	if c.MainComm == nil {
		return false
	}
	comm := c.MainComm(unit)
	return comm != "" && comm != "agetty"
}

// syncVTs sets up the virtual terminals (a connected display). Running
// gettys without a logged-in user are restarted to pick up the change.
func (c *Consoles) syncVTs(cfg *model.Config) error {
	if !fileExists(filepath.Join(c.SysRoot, "class", "tty", "tty0")) {
		return nil
	}
	path := filepath.Join(c.UnitDir, vtDropIn, "switchd.conf")
	changed := false
	if cfg.System.ConsoleLogin {
		if err := os.Remove(path); err == nil {
			changed = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		var err error
		if changed, err = writeIfChanged(path, vtUnit, 0o644); err != nil {
			return err
		}
	}
	if !changed {
		return nil
	}
	if err := c.Systemctl("daemon-reload"); err != nil {
		return err
	}
	for n := 1; n <= 12; n++ {
		unit := fmt.Sprintf("getty@tty%d.service", n)
		if c.MainComm != nil && c.MainComm(unit) == "agetty" {
			_ = c.Systemctl("restart", unit)
		}
	}
	c.Log.Info("console: virtual terminals configured", "login_required", cfg.System.ConsoleLogin)
	return nil
}

// SystemdMainComm implements Consoles.MainComm with systemctl and /proc.
func SystemdMainComm(unit string) string {
	out, err := exec.Command("systemctl", "show", "--property=MainPID", "--value", unit).Output()
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(out))
	if pid == "" || pid == "0" {
		return ""
	}
	comm, err := os.ReadFile("/proc/" + pid + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(comm))
}
