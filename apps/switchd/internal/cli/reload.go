package cli

import (
	"errors"
	"fmt"
	"time"
)

// Reloader is implemented by switches that reload their software
// (request system reload, reference 3.5).
type Reloader interface {
	Reload(user string) error
	// Started is when switchd started on a member.
	Started(member int) (time.Time, error)
}

// reloadWait is how long a member may take to come back after a reload.
var reloadWait = 10 * time.Minute

func (sh *Shell) reload(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	r, ok := sh.env.Ops.(Reloader)
	if sh.env.Ops == nil || !ok {
		return errors.New("reload is not available")
	}
	a, err := c.term.Ask("Reload the switch software (the member does not forward meanwhile) ? [yes,no] (no) ", true)
	if err != nil || !isYes(a) {
		return nil
	}
	if err := r.Reload(sh.env.User); err != nil {
		return err
	}
	c.out.WriteString("Reload requested: the member drains, stops its daemons and starts its software again\n")
	return nil
}

// waitBack waits until member id's switchd started again after now
// (a reload), so the next member reloads only then.
func (sh *Shell) waitBack(c *call, id int) error {
	r, ok := sh.env.Ops.(Reloader)
	if !ok {
		return nil
	}
	since := time.Now()
	fmt.Fprintf(c.out, "waiting for member %d to be back ...\n", id)
	for time.Since(since) < reloadWait {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if t, err := r.Started(id); err == nil && t.After(since) {
			// Back: give it the time to apply the configuration.
			time.Sleep(10 * time.Second)
			return nil
		}
	}
	return fmt.Errorf("member %d is not back after %s", id, reloadWait)
}
