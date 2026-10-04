package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/thxrben/cerium-switchd/internal/alarms"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

const swapAlarm = "switchd/swap"

// swapGuard keeps swap off (reference 1.9): a daemon whose memory was
// swapped out misses its protocol timers.
type swapGuard struct {
	member int
	log    *slog.Logger
	notify func(string)
	alarms *alarms.Set
	// swaps lists the active swap areas; off turns one off (tests
	// replace both).
	swaps func() ([]string, error)
	off   func(dev string) error

	failed map[string]bool // swap areas that could not be turned off
}

func newSwapGuard(member int, log *slog.Logger, notify func(string), al *alarms.Set) *swapGuard {
	return &swapGuard{member: member, log: log, notify: notify, alarms: al, swaps: readSwaps, off: swapOff}
}

func (g *swapGuard) loop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		g.step()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// step turns every active swap area off.
func (g *swapGuard) step() {
	devs, err := g.swaps()
	if err != nil {
		return
	}
	failed := map[string]bool{}
	var left []string
	for _, d := range devs {
		if err := g.off(d); err != nil {
			if !g.failed[d] { // logged once
				g.log.Error("swap cannot be turned off", "swap", d, "err", err)
			}
			failed[d] = true
			left = append(left, fmt.Sprintf("%s (%v)", d, err))
			continue
		}
		g.log.Warn("swap turned off: a switch does not swap", "swap", d)
	}
	g.failed = failed
	if len(left) > 0 {
		if g.alarms.Raise(swapAlarm, alarms.Major, "swap is active and cannot be turned off: "+strings.Join(left, ", ")) {
			g.notify(fmt.Sprintf("member %d: ALARM: swap is active and cannot be turned off: %s", g.member, strings.Join(left, ", ")))
		}
		return
	}
	if g.alarms.Clear(swapAlarm) {
		g.notify(fmt.Sprintf("member %d: alarm cleared: swap is off", g.member))
	}
}

// readSwaps lists the active swap areas (/proc/swaps).
func readSwaps() ([]string, error) {
	b, err := hwio.ReadFile("/proc/swaps")
	if err != nil {
		return nil, err
	}
	return parseSwaps(string(b)), nil
}

func parseSwaps(s string) []string {
	var out []string
	for i, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if i == 0 || len(f) == 0 { // the header
			continue
		}
		// Names with spaces are escaped as \040.
		out = append(out, strings.ReplaceAll(f[0], `\040`, " "))
	}
	return out
}

// swapOff turns one swap area off. It moves the swapped pages back into
// memory first, which takes as long as that needs (no deadline: the guard
// runs on its own).
func swapOff(dev string) error {
	p, err := unix.BytePtrFromString(dev)
	if err != nil {
		return err
	}
	_, _, e := unix.Syscall(unix.SYS_SWAPOFF, uintptr(unsafe.Pointer(p)), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
