package sdnotify

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// Liveness feeds the systemd watchdog only while every loop of the program
// that registered makes progress: a loop that hangs stops the pings, and
// systemd restarts the program. A loop beats (Beat) at least every Max.
type Liveness struct {
	// Max is how long a loop may go without a beat (0: 20 s).
	Max time.Duration

	mu    sync.Mutex
	beats map[string]time.Time
	now   func() time.Time
}

func (l *Liveness) max() time.Duration {
	if l.Max > 0 {
		return l.Max
	}
	return 20 * time.Second
}

// Beat records progress of a loop (and registers it).
func (l *Liveness) Beat(loop string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.beats == nil {
		l.beats = map[string]time.Time{}
	}
	l.beats[loop] = l.clock()
}

// Forget unregisters a loop that ended on purpose.
func (l *Liveness) Forget(loop string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.beats, loop)
}

func (l *Liveness) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// Late returns the loops without a beat for longer than Max.
func (l *Liveness) Late() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	now := l.clock()
	for name, t := range l.beats {
		if now.Sub(t) > l.max() {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Run pings the watchdog (if the unit has one) while no loop is late;
// onLate is told about late loops (once per change, may be nil).
func (l *Liveness) Run(ctx context.Context, onLate func([]string)) {
	iv := WatchdogInterval()
	if iv <= 0 {
		iv = 5 * time.Second // no watchdog: still report late loops
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	var reported string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		late := l.Late()
		if key := strings.Join(late, ","); key != reported {
			reported = key
			if onLate != nil {
				onLate(late)
			}
		}
		if len(late) == 0 && WatchdogInterval() > 0 {
			Alive()
		}
	}
}
