// Package sdnotify tells systemd about a service's state (sd_notify):
// ready after start-up, alive (watchdog), status text. Without
// NOTIFY_SOCKET (not started by systemd) every call does nothing.
package sdnotify

import (
	"net"
	"os"
	"strconv"
	"time"
)

// Notify sends a state string ("READY=1", "WATCHDOG=1", "STATUS=...").
func Notify(state string) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	if path[0] == '@' {
		path = "\x00" + path[1:] // abstract socket
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte(state))
	return err
}

// Ready reports the end of start-up.
func Ready() error { return Notify("READY=1") }

// Status sets the status text shown by systemctl status.
func Status(text string) error { return Notify("STATUS=" + text) }

// WatchdogInterval is how often to send Alive (half the unit's
// WatchdogSec); 0: no watchdog.
func WatchdogInterval() time.Duration {
	us, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || us <= 0 {
		return 0
	}
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0
	}
	return time.Duration(us) * time.Microsecond / 2
}

// Alive tells the watchdog that the service works.
func Alive() error { return Notify("WATCHDOG=1") }
