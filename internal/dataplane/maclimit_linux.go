//go:build linux

package dataplane

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// macLimits enforces per-port MAC learning limits in userspace (Linux only
// has a bridge-wide limit): learning is switched off on a port while it has
// reached its limit (reference 5.3.2 mac-limit).
type macLimits struct {
	mu       sync.Mutex
	limits   map[string]int  // port -> limit
	disabled map[string]bool // ports where switchd switched learning off
	kick     chan struct{}
}

func (k *Netlink) ml() *macLimits {
	k.mlOnce.Do(func() {
		k.macl = &macLimits{limits: map[string]int{}, disabled: map[string]bool{}, kick: make(chan struct{}, 1)}
	})
	return k.macl
}

// setMaxLearned records the limit of a port and triggers enforcement.
func (k *Netlink) setMaxLearned(port string, limit int) {
	m := k.ml()
	m.mu.Lock()
	if limit == 0 {
		delete(m.limits, port)
	} else {
		m.limits[port] = limit
	}
	m.mu.Unlock()
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (k *Netlink) maxLearned(port string) int {
	m := k.ml()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limits[port]
}

// EnforceMACLimits runs until ctx is done.
func (k *Netlink) EnforceMACLimits(ctx context.Context, log *slog.Logger) {
	m := k.ml()
	updates := make(chan netlink.NeighUpdate, 1024)
	done := make(chan struct{})
	defer close(done)
	if err := netlink.NeighSubscribeWithOptions(updates, done, netlink.NeighSubscribeOptions{
		ErrorCallback: func(err error) { log.Warn("mac-limit: neighbour events", "err", err) },
	}); err != nil {
		log.Warn("mac-limit: no FDB events; checking every 5 s", "err", err)
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case u, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if u.Family != unix.AF_BRIDGE {
				continue
			}
		case <-m.kick:
		case <-tick.C:
		}
		k.enforceOnce(log)
	}
}

func (k *Netlink) enforceOnce(log *slog.Logger) {
	m := k.ml()
	m.mu.Lock()
	limits := make(map[string]int, len(m.limits))
	for p, l := range m.limits {
		limits[p] = l
	}
	disabled := make(map[string]bool, len(m.disabled))
	for p := range m.disabled {
		disabled[p] = true
	}
	m.mu.Unlock()
	if len(limits) == 0 && len(disabled) == 0 {
		return
	}
	counts := map[string]int{}
	if fdb, err := k.FDB(); err == nil {
		for _, e := range fdb {
			if !e.Static {
				counts[e.Port]++
			}
		}
	}
	set := func(port string, learn bool) {
		l, err := netlink.LinkByName(port)
		if err != nil {
			return
		}
		if err := netlink.LinkSetLearning(l, learn); err != nil {
			log.Error("mac-limit: cannot change learning", "port", port, "err", err)
			return
		}
		m.mu.Lock()
		if learn {
			delete(m.disabled, port)
		} else {
			m.disabled[port] = true
		}
		m.mu.Unlock()
	}
	for port, limit := range limits {
		switch n := counts[port]; {
		case n >= limit && !disabled[port]:
			log.Warn("mac-limit reached: learning disabled, new addresses are flooded", "port", port, "limit", limit, "learned", n)
			set(port, false)
		case n < limit && disabled[port]:
			log.Info("mac-limit: below the limit again, learning enabled", "port", port, "limit", limit, "learned", n)
			set(port, true)
		}
	}
	// Ports that lost their limit (or were released) learn again.
	for port := range disabled {
		if _, ok := limits[port]; !ok {
			set(port, true)
		}
	}
}
