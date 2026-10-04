package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// Reload restarts the member's switch software without rebooting
// (request system reload, reference 3.5): drain, stop the daemons in
// order, switch ports down, a new memory plan, and switchd again.
func (o *ops) Reload(user string) error {
	if err := o.updateRunning("the reload"); err != nil {
		return err
	}
	o.log.Warn("system reload requested", "facility", "change-log", "user", user)
	o.notify(fmt.Sprintf("%s: member %d reloads its software (request system reload)", user, o.member))
	if o.dryRun {
		return nil
	}
	go func() {
		time.Sleep(time.Second) // the notice and the reply reach the sessions
		if o.maint != nil {
			o.maint.drainForShutdown("the reload")
		}
		if o.sup != nil {
			ctx, cancel := context.WithTimeout(context.Background(), daemonsStopTime)
			o.sup.Shutdown(ctx)
			cancel()
		}
		o.switchPortsDown()
		// The next start divides the memory anew (system memory).
		hwio.Remove(memoryPlanFile)
		o.log.Warn("system reload: switchd restarts")
		o.restart()
	}()
	return nil
}

// switchPortsDown takes this member's ports down, except stacking and
// management ports (the stack and the management access stay).
func (o *ops) switchPortsDown() {
	cfg := o.model()
	for _, p := range o.names.Ports() {
		if o.vc != nil && o.vc.IsPort(p.Linux) {
			continue
		}
		if cfg != nil {
			if i := cfg.Interfaces[p.Name]; i != nil && i.Management {
				continue
			}
		}
		if err := command("ip", "link", "set", "dev", p.Linux, "down"); err != nil {
			o.log.Warn("system reload: port not taken down", "port", p.Name, "err", err)
		}
	}
}

// Started is when switchd started on member id (a reload restarts it).
func (o *ops) Started(id int) (time.Time, error) {
	if id == o.member || o.vc == nil || o.vc.Control == nil {
		return o.started, nil
	}
	raw, err := o.vc.Control.Call(id, "started", nil, 5*time.Second)
	if err != nil {
		return time.Time{}, err
	}
	var t time.Time
	if err := json.Unmarshal(raw, &t); err != nil {
		return time.Time{}, err
	}
	if t.IsZero() {
		return t, errors.New("no start time")
	}
	return t, nil
}
