package cli

import (
	"errors"
	"fmt"
	"sort"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/inventory"
)

// EnvSensor is a member's sensor (show chassis environment).
type EnvSensor struct {
	Member int `json:"member"`
	inventory.Sensor
}

// Environment is implemented by members that read their sensors.
type Environment interface {
	Environment() ([]EnvSensor, error)
}

// showEnvironment is "show chassis environment" (reference 3.5).
func (sh *Shell) showEnvironment(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	e, ok := sh.env.Ops.(Environment)
	if sh.env.Ops == nil || !ok {
		return errors.New("environment information is not available")
	}
	ss, err := e.Environment()
	if err := partial(c, err); err != nil {
		return err
	}
	if c.only != nil {
		var keep []EnvSensor
		for _, s := range ss {
			for _, m := range c.only {
				if s.Member == m {
					keep = append(keep, s)
				}
			}
		}
		ss = keep
	}
	if len(ss) == 0 {
		c.out.WriteString("No sensors (the kernel reports none: a virtual machine, or no hwmon driver for the board)\n")
		return nil
	}
	order := map[string]int{"Temp": 0, "Fans": 1, "Voltage": 2, "Power": 3}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].Member != ss[j].Member {
			return ss[i].Member < ss[j].Member
		}
		return order[ss[i].Class] < order[ss[j].Class]
	})
	fmt.Fprintf(c.out, "%-8s %-36s %-9s %s\n", "Class", "Item", "Status", "Measurement")
	for _, s := range ss {
		item := fmt.Sprintf("Member %d %s", s.Member, s.ID())
		fmt.Fprintf(c.out, "%-8s %-36s %-9s %s\n", s.Class, item, s.Status, s.Measurement())
	}
	return nil
}
