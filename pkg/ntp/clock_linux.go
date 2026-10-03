//go:build linux

package ntp

import (
	"time"

	"golang.org/x/sys/unix"
)

// SystemClock sets the Linux system clock.
type SystemClock struct{}

// Step sets CLOCK_REALTIME d ahead.
func (SystemClock) Step(d time.Duration) error {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return err
	}
	n := time.Unix(int64(ts.Sec), int64(ts.Nsec)).Add(d)
	nts := unix.NsecToTimespec(n.UnixNano())
	return unix.ClockSettime(unix.CLOCK_REALTIME, &nts)
}

// Slew asks the kernel to correct d gradually (adjtime semantics).
func (SystemClock) Slew(d time.Duration) error {
	tx := unix.Timex{Modes: unix.ADJ_OFFSET_SINGLESHOT}
	setOffset(&tx.Offset, int64(d/time.Microsecond))
	_, err := unix.Adjtimex(&tx)
	return err
}

// setOffset stores v in a Timex field whose type is int32 on 32-bit
// systems and int64 on 64-bit ones.
func setOffset[T ~int32 | ~int64](p *T, v int64) { *p = T(v) }
