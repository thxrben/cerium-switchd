package ntp

import (
	"context"
	"log/slog"
	"time"
)

// Config is what an NTP client runs (computed by switchd for cer-ntpd):
// the servers, used on the master, and the routing instance queries leave
// through.
type Config struct {
	Servers []Server `json:"servers,omitempty"`
	VRF     string   `json:"vrf,omitempty"`
}

// Follower sets the clock to another system's time (a stack member takes
// the master's, reference 1.8): offset = their time + half the round trip
// - local time; stepped above StepLimit, slewed below.
type Follower struct {
	Clock Clock
	Log   *slog.Logger
	// Query returns the other system's time (false: there is none to ask
	// now, e.g. this member is the master).
	Query func(ctx context.Context) (time.Time, bool, error)
	// Interval between adjustments (default 16 s).
	Interval time.Duration
}

// Run adjusts the clock until ctx ends.
func (f *Follower) Run(ctx context.Context) {
	iv := f.Interval
	if iv == 0 {
		iv = 16 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	first := time.After(2 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-first:
		case <-t.C:
		}
		f.once(ctx)
	}
}

func (f *Follower) once(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	t1 := time.Now()
	theirs, ok, err := f.Query(qctx)
	t4 := time.Now()
	if !ok || err != nil {
		return
	}
	rtt := t4.Sub(t1)
	if rtt > time.Second {
		return // too slow to be useful
	}
	off := theirs.Add(rtt / 2).Sub(t4)
	switch {
	case off > StepLimit || off < -StepLimit:
		if err := f.Clock.Step(off); err == nil && f.Log != nil {
			f.Log.Warn("clock stepped to the master's time", "offset", off.String())
		}
	case off > time.Millisecond || off < -time.Millisecond:
		_ = f.Clock.Slew(off)
	}
}
