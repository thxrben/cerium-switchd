package cli

import (
	"errors"
	"fmt"

	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/schema"
)

// BPDUClearer is implemented by members whose bpdu-block ports can be
// re-enabled (reference 5.5).
type BPDUClearer interface {
	// ClearBPDU re-enables a port of this member shut down by bpdu-block
	// ("": all); it returns how many.
	ClearBPDU(iface string) (int, error)
}

// clearErrorCommand is "clear error bpdu interface <if>".
func clearErrorCommand() *command {
	return &command{name: "error", help: "Re-enable ports shut down by a protection", class: commit.Operator, sub: []*command{
		{name: "bpdu", help: "Ports shut down by bpdu-block", class: commit.Operator, run: (*Shell).clearBPDU,
			complete: words(Completion{Text: "interface", Help: "The port or aggregated interface"})},
	}}
}

func (sh *Shell) clearBPDU(c *call) error {
	if len(c.args) != 2 || !prefixOf(c.args[0].Text, "interface") {
		return &posError{pos: c.argPos(0), msg: "expecting 'interface <interface>'"}
	}
	name := c.args[1].Text
	o, ok := sh.env.Ops.(BPDUClearer)
	if sh.env.Ops == nil || !ok {
		return errors.New("bpdu-block is not available")
	}
	// The port's member clears it; a bundle on every member it spans.
	members := []int{}
	if p, ok := schema.ParsePhysical(name); ok {
		members = append(members, p.Member)
	} else if sh.env.Stack != nil {
		members = sh.env.Stack.Members()
	}
	self := 0
	if sh.env.Stack != nil {
		self = sh.env.Stack.Self()
	}
	total := 0
	for _, m := range members {
		if sh.env.Stack == nil || m == self || self == 0 {
			n, err := o.ClearBPDU(name)
			if err != nil {
				return err
			}
			total += n
			continue
		}
		if _, err := sh.env.Stack.Exec(c.ctx, m, "clear error bpdu interface "+name, true); err != nil {
			fmt.Fprintf(c.out, "member %d: %v\n", m, err)
		} else {
			total++
		}
	}
	if len(members) == 0 {
		n, err := o.ClearBPDU(name)
		if err != nil {
			return err
		}
		total = n
	}
	if total == 0 {
		fmt.Fprintf(c.out, "%s was not shut down by bpdu-block\n", name)
		return nil
	}
	fmt.Fprintf(c.out, "%s enabled again\n", name)
	return nil
}
