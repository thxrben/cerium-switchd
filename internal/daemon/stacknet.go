package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"mclag/internal/dataplane"
	"mclag/internal/stack"
)

// runStackNet keeps the underlay of the stack tunnels (reference 5.2) in
// step with the stacking links and the stack topology: it recomputes every
// 100 ms and on every topology change, applies only when something
// differs, and checks the kernel fully every 10 s.
func runStackNet(ctx context.Context, vc *stack.Manager, log *slog.Logger) {
	var last string
	var lastFull time.Time
	failing := false
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		mesh := vc.Mesh()
		var changed <-chan struct{}
		if mesh != nil {
			changed = mesh.Changed()
			u := stackUnderlay(vc)
			key := fmt.Sprint(u)
			if now := time.Now(); key != last || now.Sub(lastFull) >= 10*time.Second {
				ch, err := dataplane.SyncStackUnderlay(u)
				switch {
				case err != nil && !failing:
					log.Warn("stack: tunnel underlay", "err", err)
					failing = true
				case err == nil:
					if failing {
						log.Info("stack: tunnel underlay is complete again")
					}
					failing = false
					last, lastFull = key, now
				}
				if ch {
					log.Debug("stack: tunnel underlay updated", "routes", len(u.Hops), "links", len(u.Links))
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-changed:
		}
	}
}

// stackUnderlay collects the desired underlay (sorted, so that equal
// states print equally).
func stackUnderlay(vc *stack.Manager) dataplane.StackUnderlay {
	u := dataplane.StackUnderlay{Member: vc.Member(), Hops: vc.Mesh().FirstHops()}
	for _, p := range vc.Ports() {
		if p.Linux != "" && p.State != "absent" {
			u.Ports = append(u.Ports, p.Linux)
		}
	}
	slices.Sort(u.Ports)
	for _, l := range vc.Links() {
		u.Links = append(u.Links, dataplane.StackLink{Port: l.Linux, Neighbor: l.Neighbor, MAC: l.NeighborMAC})
	}
	slices.SortFunc(u.Links, func(a, b dataplane.StackLink) int {
		if a.Port < b.Port {
			return -1
		}
		if a.Port > b.Port {
			return 1
		}
		return 0
	})
	for _, h := range u.Hops {
		slices.Sort(h)
	}
	return u
}
