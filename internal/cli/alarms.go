package cli

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Alarm is an active alarm (show system alarms).
type Alarm struct {
	Member int       `json:"member"`
	Class  string    `json:"class"`
	Text   string    `json:"text"`
	Since  time.Time `json:"since"`
}

// Alarms is implemented by members that keep alarms.
type Alarms interface {
	Alarms() ([]Alarm, error)
}

// showAlarms is "show system alarms" (reference 3.5).
func (sh *Shell) showAlarms(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	a, ok := sh.env.Ops.(Alarms)
	if sh.env.Ops == nil || !ok {
		return errors.New("alarm information is not available")
	}
	as, err := a.Alarms()
	if err := partial(c, err); err != nil {
		return err
	}
	if c.only != nil {
		var keep []Alarm
		for _, x := range as {
			for _, m := range c.only {
				if x.Member == m {
					keep = append(keep, x)
				}
			}
		}
		as = keep
	}
	sort.SliceStable(as, func(i, j int) bool {
		if !as[i].Since.Equal(as[j].Since) {
			return as[i].Since.Before(as[j].Since)
		}
		return as[i].Member < as[j].Member
	})
	if len(as) == 0 {
		c.out.WriteString("No alarms currently active\n")
		return nil
	}
	word := "alarms"
	if len(as) == 1 {
		word = "alarm"
	}
	fmt.Fprintf(c.out, "%d %s currently active\n", len(as), word)
	fmt.Fprintf(c.out, "%-23s %-6s %-6s %s\n", "Alarm time", "Class", "Member", "Description")
	for _, x := range as {
		fmt.Fprintf(c.out, "%-23s %-6s %-6d %s\n", x.Since.Format("2006-01-02 15:04:05 MST"), x.Class, x.Member, x.Text)
	}
	return nil
}
