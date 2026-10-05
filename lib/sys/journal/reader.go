package journal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
)

// Entry is a journal record as a log reader needs it.
type Entry struct {
	Time     time.Time
	Severity int    // 0-7
	Facility string // configuration name (kernel, daemon, authorization, change-log, ...)
	App      string // program (SYSLOG_IDENTIFIER, or the command name; kernel)
	PID      int
	Message  string
	// Member is set by the switch's programs (CEROS_MEMBER).
	Member int
}

// ParseEntry converts one line of journalctl -o json.
func ParseEntry(line []byte) (Entry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return Entry{}, err
	}
	str := func(k string) string {
		v, ok := raw[k]
		if !ok {
			return ""
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
		// Binary data: an array of bytes; several values: an array of
		// strings (the first counts).
		var b []byte
		var nums []int
		if json.Unmarshal(v, &nums) == nil {
			for _, n := range nums {
				b = append(b, byte(n))
			}
			return string(b)
		}
		var ss []string
		if json.Unmarshal(v, &ss) == nil && len(ss) > 0 {
			return ss[0]
		}
		return ""
	}
	e := Entry{Message: str("MESSAGE"), Severity: 6, Facility: "daemon"}
	if us, err := strconv.ParseInt(str("__REALTIME_TIMESTAMP"), 10, 64); err == nil {
		e.Time = time.UnixMicro(us)
	}
	if p, err := strconv.Atoi(str("PRIORITY")); err == nil && p >= 0 && p <= 7 {
		e.Severity = p
	}
	switch {
	case str("CEROS_FACILITY") != "":
		e.Facility = str("CEROS_FACILITY")
	case str("_TRANSPORT") == "kernel":
		e.Facility = "kernel"
	case str("SYSLOG_FACILITY") != "":
		if n, err := strconv.Atoi(str("SYSLOG_FACILITY")); err == nil {
			e.Facility = FacilityName(n)
		}
	}
	e.App = str("SYSLOG_IDENTIFIER")
	if e.App == "" {
		e.App = str("_COMM")
	}
	if str("_TRANSPORT") == "kernel" {
		e.App = "kernel"
	}
	e.PID, _ = strconv.Atoi(str("_PID"))
	if p := str("SYSLOG_PID"); p != "" {
		e.PID, _ = strconv.Atoi(p)
	}
	e.Member, _ = strconv.Atoi(str("CEROS_MEMBER"))
	return e, nil
}

// Follow reads the journal from where the last call stopped (the cursor
// in cursorFile; without one, the last backlog entries of this boot) and
// then follows it until ctx ends. It restarts journalctl when it ends.
func Follow(ctx context.Context, cursorFile string, backlog int, f func(Entry)) error {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return errors.New("journalctl is not installed")
	}
	for ctx.Err() == nil {
		args := []string{"--follow", "--output=json", "--boot", "--cursor-file=" + cursorFile}
		if _, err := hwio.Stat(cursorFile); err != nil {
			args = append(args, fmt.Sprintf("--lines=%d", backlog))
		}
		cmd := exec.CommandContext(ctx, "journalctl", args...)
		out, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			if e, err := ParseEntry(sc.Bytes()); err == nil {
				f(e)
			}
		}
		cmd.Wait()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return nil
}
