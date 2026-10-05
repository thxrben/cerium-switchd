package updated

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
)

// Main runs the update daemon (the program switchd-update) until it fails.
func Main() int {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	sys, err := NewSystem()
	if err != nil {
		log.Error("switchd-update", "err", err)
		return 1
	}
	switchdDir := filepath.Join(ConfigRoot, "switchd")
	sys.Check = RunCheck
	sys.ActiveConfig = func() ([]byte, error) { return ConfigFromStore(switchdDir) }
	loadTimeouts(sys.ActiveConfig) // system timeouts (reference 5.1)
	// ceros.healthtimeout=<seconds> on the kernel command line shortens the
	// health timeout (image tests).
	var timeout time.Duration
	if cmdline, err := hwio.ReadFile("/proc/cmdline"); err == nil {
		for _, f := range strings.Fields(string(cmdline)) {
			if v, ok := strings.CutPrefix(f, "ceros.healthtimeout="); ok {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					timeout = time.Duration(n) * time.Second
				}
			}
		}
	}
	d := &Daemon{
		P:           sys,
		Timeout:     timeout,
		ConfigDir:   filepath.Join(ConfigRoot, "update"),
		SwitchdDir:  switchdDir,
		BackupDir:   filepath.Join(ConfigRoot, "backup"),
		Socket:      DefaultSocket,
		Log:         log,
		RebootDelay: 2 * time.Second,
	}
	if err := d.Run(context.Background()); err != nil {
		log.Error("switchd-update", "err", err)
		return 1
	}
	return 0
}
