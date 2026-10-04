package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/commit"
)

// allOps implements every optional interface of Ops (RIB, BGP, OSPF and
// the base fake).
type allOps struct {
	bgpOps
	ospfOps
}

func (allOps) BFDSessions() ([]BFDSession, error) { return bfdOps{}.BFDSessions() }

func (allOps) Optics(iface string) ([]OpticsPort, error) { return opticsOps{}.Optics(iface) }

func (allOps) Alarms() ([]Alarm, error) {
	return []Alarm{{Member: 1, Class: "Major", Text: "x", Since: time.Now()}}, nil
}

// Every operational command runs, bare and with "?", against fakes:
// no panic, no internal error, no broken format verb, and nothing blocks.
func TestEveryCommand(t *testing.T) {
	skip := map[string]bool{"configure": true, "exit": true, "quit": true, "start shell": true}
	var lines []string
	var walk func(prefix []string, cs []*command)
	walk = func(prefix []string, cs []*command) {
		for _, c := range cs {
			p := append(append([]string{}, prefix...), c.name)
			line := strings.Join(p, " ")
			if c.run != nil && !skip[line] {
				lines = append(lines, line)
			}
			walk(p, c.sub)
		}
	}
	walk(nil, operational)
	if len(lines) < 100 {
		t.Fatalf("only %d commands found", len(lines))
	}
	for _, line := range lines {
		for _, l := range []string{line, line + " ?"} {
			ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
			ts.sh.env.Ops = &allOps{}
			done := make(chan string, 1)
			go func() { done <- ts.sh.Execute(context.Background(), l, ts.term).Output }()
			select {
			case out := <-done:
				if strings.Contains(out, "internal error") || strings.Contains(out, "%!") {
					t.Errorf("%q:\n%s", l, out)
				}
				if testing.Verbose() && !strings.HasSuffix(l, "?") && strings.Contains(out, "error") {
					t.Logf("%q: %s", l, strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
				}
			case <-time.After(5 * time.Second):
				t.Errorf("%q blocks", l)
			}
		}
	}
}
