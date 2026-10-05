package daemon

import (
	"context"
	"fmt"
	"github.com/thxrben/cerium-switchd/apps/switchd/internal/inventory"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/lib/platform/alarms"
	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
	"github.com/thxrben/cerium-switchd/lib/sys/nlx"
)

// loopMax is how long one of switchd's loops may go without progress
// before the watchdog restarts switchd. Every kernel and disk call has a
// deadline far below it, so only a bug stops a loop that long.
const loopMax = 45 * time.Second

// describeResource names a resource for operators.
func describeResource(res string) string {
	switch {
	case res == nlx.Resource:
		return "the kernel's network configuration (rtnl lock; usually a NIC driver that hangs)"
	case res == "/config":
		return "the configuration partition (/config)"
	case res == "/var":
		return "the data partition (/var)"
	case res == "/sys" || res == "/proc":
		return "the kernel (" + res + ")"
	case strings.HasPrefix(res, "/dev/"):
		return "the device " + res
	case strings.HasPrefix(res, "exec "):
		return "the program " + strings.TrimPrefix(res, "exec ")
	}
	return res
}

// watchHangs raises an alarm when a device stops answering and clears it
// when it answers again (the alarm set logs and announces both). Nothing
// reboots: the member keeps forwarding with what is set already.
func watchHangs(al *alarms.Set) {
	hwio.WatchResources(func(c hwio.Call, raised bool) {
		if raised {
			al.Raise("switchd/hang "+c.Resource, alarms.Major, fmt.Sprintf("%s does not answer (%s, since %s); changes that need it fail until it answers",
				describeResource(c.Resource), c.Op, c.Since.Format("15:04:05")))
			return
		}
		al.Clear("switchd/hang " + c.Resource)
	})
}

// Hangs lists switchd's calls that do not return (show system processes).
func (o *ops) Hangs() []cli.Hang {
	var out []cli.Hang
	for _, c := range hwio.Stuck() {
		out = append(out, cli.Hang{Program: "switchd", Resource: describeResource(c.Resource), Call: c.Op, Since: c.Since})
	}
	return out
}

// watchSensors raises an alarm for a sensor beyond its limits (Minor at
// Warning, Major at Critical) and clears it when it is back (show
// chassis environment, reference 3.5).
func watchSensors(ctx context.Context, al *alarms.Set) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	raised := map[string]string{} // sensor -> status alarmed
	for {
		seen := map[string]bool{}
		for _, s := range inventory.ReadSensors("/sys") {
			id := "switchd/env " + s.ID()
			seen[id] = true
			if s.Status == "OK" {
				if raised[id] != "" {
					delete(raised, id)
					al.Clear(id)
				}
				continue
			}
			if raised[id] == s.Status {
				continue
			}
			raised[id] = s.Status
			class := alarms.Minor
			if s.Status == "Critical" {
				class = alarms.Major
			}
			al.Raise(id, class, fmt.Sprintf("%s %s %s: %s", s.Class, s.ID(), strings.ToLower(s.Status), s.Measurement()))
		}
		for id := range raised {
			if !seen[id] { // the sensor is gone
				delete(raised, id)
				al.Clear(id)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
