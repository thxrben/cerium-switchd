package cli

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
)

// BFDSession is one BFD session of the stack (show bfd session, reference
// 5.12).
type BFDSession struct {
	Member      int           `json:"member"`
	Address     string        `json:"address"`
	Instance    string        `json:"instance,omitempty"`
	Multihop    bool          `json:"multihop,omitempty"`
	Interface   string        `json:"interface,omitempty"`
	State       string        `json:"state"`
	RemoteState string        `json:"remote_state"`
	Diag        string        `json:"diag,omitempty"`
	Detect      time.Duration `json:"detect"`
	Interval    time.Duration `json:"interval"`
	Multiplier  int           `json:"multiplier"`
	Clients     []string      `json:"clients,omitempty"`
	Up          time.Duration `json:"up,omitempty"` // in state Up
	Transitions int           `json:"transitions"`
	LocalDisc   uint32        `json:"local_disc"`
	RemoteDisc  uint32        `json:"remote_disc"`
	Rx, Tx      uint64
	RxDropped   uint64 `json:"rx_dropped,omitempty"`
}

// BFD is implemented by members that run cer-bfdd.
type BFD interface {
	BFDSessions() ([]BFDSession, error)
}

func bfdCommand() *command {
	return &command{name: "bfd", help: "Show BFD information", class: commit.ReadOnly, sub: []*command{
		{name: "session", help: "Show the BFD sessions", class: commit.ReadOnly, run: (*Shell).showBFD,
			complete: words(Completion{Text: "extensive", Help: "Counters, discriminators, clients"},
				Completion{Text: "address", Help: "One neighbour's sessions"})},
	}}
}

// clientName is a BFD client as shown.
func clientName(c string) string {
	switch {
	case strings.HasPrefix(c, "ospf"):
		return "OSPF"
	case strings.HasPrefix(c, "bgp"):
		return "BGP"
	}
	return c
}

func (sh *Shell) showBFD(c *call) error {
	extensive, addr := false, ""
	for i := 0; i < len(c.args); i++ {
		switch w := c.args[i].Text; {
		case prefixOf(w, "extensive"):
			extensive = true
		case w == "address" && i+1 < len(c.args):
			i++
			addr = c.args[i].Text
		case addr == "" && w != "address":
			addr = w
		default:
			return &posError{pos: c.argPos(i), msg: "syntax error, expecting 'extensive' or an address"}
		}
	}
	b, ok := sh.env.Ops.(BFD)
	if sh.env.Ops == nil || !ok {
		return errors.New("BFD information is not available")
	}
	ss, err := b.BFDSessions()
	if err := partial(c, err); err != nil {
		return err
	}
	ss = slices.DeleteFunc(ss, func(s BFDSession) bool {
		return (addr != "" && s.Address != addr) || !c.shows(s.Interface)
	})
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].Address != ss[j].Address {
			return ss[i].Address < ss[j].Address
		}
		return ss[i].Member < ss[j].Member
	})
	clients := 0
	var rate float64
	for _, s := range ss {
		clients += len(s.Clients)
		if s.Interval > 0 {
			rate += float64(time.Second) / float64(s.Interval)
		}
	}
	secs := func(d time.Duration) string { return fmt.Sprintf("%.3f", d.Seconds()) }
	if !extensive {
		fmt.Fprintf(c.out, "%-26s %-9s %-14s %8s %9s %10s\n", "", "", "", "Detect", "Transmit", "")
		fmt.Fprintf(c.out, "%-26s %-9s %-14s %8s %9s %10s\n", "Address", "State", "Interface", "Time", "Interval", "Multiplier")
		for _, s := range ss {
			fmt.Fprintf(c.out, "%-26s %-9s %-14s %8s %9s %10d\n", s.Address, s.State, dash(s.Interface), secs(s.Detect), secs(s.Interval), s.Multiplier)
		}
	}
	for _, s := range ss {
		if !extensive {
			break
		}
		fmt.Fprintf(c.out, "%-26s %-9s %-14s Detect %s, transmit %s, multiplier %d\n", s.Address, s.State, dash(s.Interface),
			secs(s.Detect), secs(s.Interval), s.Multiplier)
		var cs []string
		for _, cl := range s.Clients {
			if n := clientName(cl); !slices.Contains(cs, n) {
				cs = append(cs, n)
			}
		}
		slices.Sort(cs)
		fmt.Fprintf(c.out, " Client %s, member %d", strings.Join(cs, " "), s.Member)
		if s.Instance != "" {
			fmt.Fprintf(c.out, ", routing instance %s", s.Instance)
		}
		if s.Multihop {
			c.out.WriteString(", multihop")
		}
		c.out.WriteString("\n")
		if s.State == "Up" {
			fmt.Fprintf(c.out, " Session up time %s\n", fmtDuration(s.Up))
		}
		fmt.Fprintf(c.out, " Remote state %s, local diagnostic %s, %d transitions\n", s.RemoteState, dash(s.Diag), s.Transitions)
		fmt.Fprintf(c.out, " Local discriminator %d, remote discriminator %d\n", s.LocalDisc, s.RemoteDisc)
		fmt.Fprintf(c.out, " Packets received %d (dropped %d), sent %d\n\n", s.Rx, s.RxDropped, s.Tx)
	}
	fmt.Fprintf(c.out, "\n%d sessions, %d clients\n", len(ss), clients)
	fmt.Fprintf(c.out, "Cumulative transmit rate %.1f pps, cumulative receive rate %.1f pps\n", rate, rate)
	return nil
}
