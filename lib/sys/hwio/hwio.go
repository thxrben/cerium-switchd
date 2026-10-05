// Package hwio runs calls that can hang in the kernel (disk and block I/O,
// netlink, ioctls, sysfs) with a deadline, so a program never stalls on a
// device that stopped answering.
//
// Linux cannot cancel a system call that waits inside the kernel (a dying
// disk, a NIC driver that holds a lock). A call therefore runs on its own
// goroutine; when its deadline passes, the caller gets an error and goes on,
// and the call is recorded as stuck on its resource (a disk, "netlink", a
// device). Further calls on a stuck resource fail at once instead of piling
// up behind it, until the stuck call returns. Stuck() lists them and
// Watch reports every change, so a program can tell its operators.
//
// A call that timed out may still complete later: its outcome is unknown.
package hwio

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// The deadlines (system timeouts disk-operation and kernel-call,
// reference 5.1); SetDeadlines changes them at run time.
var fileDeadline, kernelDeadline atomic.Int64

func init() { SetDeadlines(10*time.Second, 5*time.Second) }

// FileDeadline is the deadline of one file operation on a disk (open,
// read, write, sync, rename …).
func FileDeadline() time.Duration { return time.Duration(fileDeadline.Load()) }

// KernelDeadline is the deadline of one netlink, ioctl or sysfs call.
func KernelDeadline() time.Duration { return time.Duration(kernelDeadline.Load()) }

// SetDeadlines sets the deadlines (zero: unchanged); calls already running
// keep theirs.
func SetDeadlines(file, kernel time.Duration) {
	if file > 0 {
		fileDeadline.Store(int64(file))
	}
	if kernel > 0 {
		kernelDeadline.Store(int64(kernel))
	}
}

// ErrTimeout is matched (errors.Is) by every error of a call that did not
// end in time, or that was refused because its resource is stuck.
var ErrTimeout = errors.New("no answer in time")

// Error is a call that did not end in time.
type Error struct {
	Resource string
	Op       string
	// After: the deadline that passed (0: refused, the resource was stuck
	// already since Since by StuckOp).
	After   time.Duration
	Since   time.Time
	StuckOp string
}

func (e *Error) Error() string {
	if e.After > 0 {
		return fmt.Sprintf("%s: %s: no answer after %v (the device may hang)", e.Resource, e.Op, e.After)
	}
	return fmt.Sprintf("%s: %s: not tried, %s hangs since %s (%s)", e.Resource, e.Op, e.Resource,
		e.Since.Format("15:04:05"), e.StuckOp)
}

// Is makes errors.Is(err, ErrTimeout) true.
func (e *Error) Is(target error) bool { return target == ErrTimeout }

// Call is a call that is stuck.
type Call struct {
	Resource string    `json:"resource"`
	Op       string    `json:"op"`
	Since    time.Time `json:"since"` // when it started
}

var (
	mu       sync.Mutex
	stuck    = map[uint64]Call{}
	nextID   uint64
	watchers []func([]Call)
)

// Stuck returns the calls that are stuck now, oldest first.
func Stuck() []Call {
	mu.Lock()
	defer mu.Unlock()
	return stuckLocked()
}

func stuckLocked() []Call {
	out := make([]Call, 0, len(stuck))
	for _, c := range stuck {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Call) int { return a.Since.Compare(b.Since) })
	return out
}

// Watch calls fn with the stuck calls whenever a call gets stuck or a stuck
// call returns. fn must not block.
func Watch(fn func([]Call)) {
	mu.Lock()
	watchers = append(watchers, fn)
	mu.Unlock()
}

func notify() {
	mu.Lock()
	list, ws := stuckLocked(), slices.Clone(watchers)
	mu.Unlock()
	for _, w := range ws {
		w(list)
	}
}

// busy returns the stuck call on res, if any.
func busy(res string) (Call, bool) {
	mu.Lock()
	defer mu.Unlock()
	for _, c := range stuck {
		if c.Resource == res {
			return c, true
		}
	}
	return Call{}, false
}

// Do runs fn with deadline d (0: KernelDeadline) on resource res.
func Do[T any](res, op string, d time.Duration, fn func() (T, error)) (T, error) {
	return DoCtx(context.Background(), res, op, d, fn)
}

// DoErr is Do for calls without a result.
func DoErr(res, op string, d time.Duration, fn func() error) error {
	_, err := Do(res, op, d, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

// DoCtx is Do that also ends when ctx does (with ctx's error).
func DoCtx[T any](ctx context.Context, res, op string, d time.Duration, fn func() (T, error)) (T, error) {
	var zero T
	if d <= 0 {
		d = KernelDeadline()
	}
	if c, ok := busy(res); ok {
		return zero, &Error{Resource: res, Op: op, Since: c.Since, StuckOp: c.Op}
	}
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	start := time.Now()
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()
	t := time.NewTimer(d)
	select {
	case r := <-ch:
		t.Stop()
		return r.v, r.err
	case <-t.C:
		hung(res, op, start, ch)
		return zero, &Error{Resource: res, Op: op, After: d}
	case <-ctx.Done():
		// The caller gave up; the call counts as stuck only once its own
		// deadline passed too.
		go func() {
			select {
			case <-ch:
				t.Stop()
			case <-t.C:
				hung(res, op, start, ch)
			}
		}()
		return zero, ctx.Err()
	}
}

// hung records a stuck call (before it returns: the next call on res sees
// it) until it ends (done).
func hung[T any](res, op string, start time.Time, done <-chan T) {
	mu.Lock()
	nextID++
	id := nextID
	stuck[id] = Call{Resource: res, Op: op, Since: start}
	mu.Unlock()
	go func() {
		notify()
		<-done
		mu.Lock()
		delete(stuck, id)
		mu.Unlock()
		notify()
	}()
}

// WatchResources calls fn when a resource gets stuck (raised, with its
// oldest stuck call) and when it answers again (raised false). fn runs on
// a goroutine of its own, one event after the other, and may block.
func WatchResources(fn func(c Call, raised bool)) {
	type ev struct {
		c      Call
		raised bool
	}
	ch := make(chan ev, 256)
	var (
		wmu  sync.Mutex
		have = map[string]Call{}
	)
	Watch(func(list []Call) {
		wmu.Lock()
		defer wmu.Unlock()
		now := map[string]Call{}
		for _, c := range list {
			if _, ok := now[c.Resource]; !ok {
				now[c.Resource] = c
			}
		}
		for r, c := range now {
			if _, ok := have[r]; !ok {
				select {
				case ch <- ev{c, true}:
				default:
				}
			}
		}
		for r, c := range have {
			if _, ok := now[r]; !ok {
				select {
				case ch <- ev{c, false}:
				default:
				}
			}
		}
		have = now
	})
	go func() {
		for e := range ch {
			fn(e.c, e.raised)
		}
	}()
}
