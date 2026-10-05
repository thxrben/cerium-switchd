package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
)

type alarmOps struct {
	fakeOps
	as []Alarm
}

func (f *alarmOps) Alarms() ([]Alarm, error) { return f.as, nil }

func TestShowAlarms(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ops := &alarmOps{}
	ts.sh.env.Ops = ops
	contains(t, ts.ok("show system alarms"), "No alarms currently active")
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	ops.as = []Alarm{
		{Member: 2, Class: "Minor", Text: "2/0/3 received a BPDU", Since: t0.Add(time.Minute)},
		{Member: 1, Class: "Major", Text: "/var does not answer (sync)", Since: t0},
	}
	out := ts.ok("show system alarms")
	contains(t, out, "2 alarms currently active", "Alarm time", "2026-10-04 12:00:00 UTC Major  1      /var does not answer",
		"Minor  2      2/0/3 received a BPDU")
	if i, j := indexOf(out, "/var"), indexOf(out, "2/0/3"); i > j {
		t.Fatalf("oldest first:\n%s", out)
	}
}

func indexOf(s, sub string) int { return strings.Index(s, sub) }
