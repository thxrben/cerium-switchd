package supervise

import (
	"math"
	"testing"
	"time"
)

func TestUnitPath(t *testing.T) {
	for in, want := range map[string]string{
		"cer-lacpd.service":      "/org/freedesktop/systemd1/unit/cer_2dlacpd_2eservice",
		"switchd-update.service": "/org/freedesktop/systemd1/unit/switchd_2dupdate_2eservice",
		"getty@tty1.service":     "/org/freedesktop/systemd1/unit/getty_40tty1_2eservice",
		"1x.service":             "/org/freedesktop/systemd1/unit/_31x_2eservice",
		"a_b.service":            "/org/freedesktop/systemd1/unit/a_5fb_2eservice",
	} {
		if got := string(UnitPath(in)); got != want {
			t.Errorf("UnitPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnitState(t *testing.T) {
	u := unitState(map[string]any{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "running", "Result": "success",
		"MainPID": uint32(712), "NRestarts": uint32(2), "ExecMainCode": int32(0), "ExecMainStatus": int32(0),
		"ActiveEnterTimestamp": uint64(1791023410123456), "MemoryCurrent": uint64(26984448),
		"CPUUsageNSec": uint64(math.MaxUint64), // accounting off: "not set"
	})
	want := UnitState{Loaded: true, Active: "active", Sub: "running", Result: "success", PID: 712, NRestarts: 2,
		Since: time.Unix(1791023410, 0), Memory: 26984448}
	if !u.Since.Equal(want.Since) {
		t.Fatalf("since %v, want %v", u.Since, want.Since)
	}
	u.Since = want.Since
	if u != want {
		t.Fatalf("%+v\nwant %+v", u, want)
	}
	if u := unitState(nil); u.Loaded || u.Active != "" {
		t.Fatalf("empty: %+v", u)
	}
}
