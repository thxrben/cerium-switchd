package daemon

import (
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/alarms"
)

func TestParseSwaps(t *testing.T) {
	in := "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n" +
		"/dev/sda5                               partition\t1799164\t\t0\t\t-2\n" +
		"/var/swap\\040file                       file\t\t1024\t\t0\t\t-3\n"
	if got := parseSwaps(in); !slices.Equal(got, []string{"/dev/sda5", "/var/swap file"}) {
		t.Fatalf("got %q", got)
	}
	if got := parseSwaps("Filename\tType\tSize\tUsed\tPriority\n"); len(got) != 0 {
		t.Fatalf("no swap: got %q", got)
	}
}

func TestSwapGuard(t *testing.T) {
	active := []string{"/dev/sda5", "/dev/zram0"}
	stuck := map[string]bool{"/dev/zram0": true}
	var notes []string // the alarm changes switchd announces
	al := &alarms.Set{}
	al.OnChange(func(a alarms.Alarm, _ bool) { notes = append(notes, a.Text) })
	g := newSwapGuard(slog.New(slog.DiscardHandler), al)
	g.swaps = func() ([]string, error) { return slices.Clone(active), nil }
	g.off = func(d string) error {
		if stuck[d] {
			return errors.New("busy")
		}
		active = slices.DeleteFunc(active, func(x string) bool { return x == d })
		return nil
	}
	g.step()
	if !slices.Equal(active, []string{"/dev/zram0"}) {
		t.Fatalf("active %q", active)
	}
	if l := al.List(); len(l) != 1 || l[0].Class != alarms.Major || len(notes) != 1 {
		t.Fatalf("alarm %+v notes %q", l, notes)
	}
	g.step() // still stuck: no new notice
	if len(notes) != 1 {
		t.Fatalf("notes %q", notes)
	}
	stuck = nil
	g.step()
	if len(active) != 0 || len(al.List()) != 0 || len(notes) != 2 {
		t.Fatalf("active %q alarms %+v notes %q", active, al.List(), notes)
	}
	active = []string{"/dev/sdb1"} // swap turned on again
	g.step()
	if len(active) != 0 || len(al.List()) != 0 {
		t.Fatalf("active %q", active)
	}
}
