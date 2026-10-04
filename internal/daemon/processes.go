package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thxrben/cerium-switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// Processes lists switchd and the daemons of this member (show system
// processes, reference 1.9).
func (o *ops) Processes() ([]cli.Process, error) {
	self := cli.Process{Program: "switchd", State: "running", PID: os.Getpid(), Since: o.started, Scheduling: selfScheduling()}
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		self.CPU = time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	self.Memory = selfRSS()
	out := []cli.Process{self}
	if o.sup == nil {
		return out, nil
	}
	for _, st := range o.sup.Status() {
		out = append(out, cli.Process{Program: st.Program, State: st.State, PID: st.PID, Since: st.Since, Restarts: st.Restarts,
			LastFailure: st.LastFailure, FailedAt: st.FailedAt, Memory: st.Memory, CPU: st.CPU, Scheduling: st.Scheduling})
	}
	return out, nil
}

// RestartDaemon restarts a daemon on request (restart <name>).
func (o *ops) RestartDaemon(name, user string) error {
	if o.sup == nil {
		return errors.New("the daemons are not managed here (switchd was not started by systemd)")
	}
	if err := o.sup.RestartDaemon(name); err != nil {
		return err
	}
	o.log.Info(fmt.Sprintf("restart %s requested by %s", name, user))
	return nil
}

// StopDaemon is request daemon stop: the daemon stays stopped until the
// reboot or request daemon start.
func (o *ops) StopDaemon(name, user string) (string, error) {
	if o.sup == nil {
		return "", errors.New("the daemons are not managed here (switchd was not started by systemd)")
	}
	d, err := o.sup.StopDaemon(name)
	if err != nil {
		return "", err
	}
	o.log.Warn(fmt.Sprintf("%s stopped until the reboot, requested by %s", d.Program, user))
	return d.Help, nil
}

// StartDaemon is request daemon start.
func (o *ops) StartDaemon(name, user string) error {
	if o.sup == nil {
		return errors.New("the daemons are not managed here (switchd was not started by systemd)")
	}
	d, err := o.sup.StartDaemon(name)
	if err != nil {
		return err
	}
	o.log.Info(fmt.Sprintf("%s may run again, requested by %s", d.Program, user))
	return nil
}

// selfRSS reads this process's resident memory.
func selfRSS() uint64 {
	raw, err := hwio.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				n, _ := strconv.ParseUint(f[0], 10, 64)
				return n << 10
			}
		}
	}
	return 0
}

// selfScheduling reads this process's nice value.
func selfScheduling() string {
	raw, err := hwio.ReadFile("/proc/self/stat")
	if err != nil {
		return ""
	}
	// The fields after the command name (which may contain spaces): the
	// state is field 3, nice field 19.
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 17 {
		return ""
	}
	return "nice " + f[16]
}
