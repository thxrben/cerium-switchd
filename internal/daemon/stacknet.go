package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"time"

	"github.com/thxrben/cerium-switchd/internal/dataplane"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/stack"
)

// runStackNet keeps the underlay of the stack tunnels (reference 5.2) in
// step with the stacking links and the stack topology: it recomputes every
// 100 ms and on every topology change, applies only when something
// differs, and checks the kernel fully every 10 s.
func runStackNet(ctx context.Context, vc *stack.Manager, active func() *model.Config, sec *stackMACsec, log *slog.Logger) {
	warned := map[string]string{}
	// The active configuration, built at most once a second (a commit
	// reaches the stacking links within a second).
	var cfg *model.Config
	var built time.Time
	var last string
	var lastFull time.Time
	failing := false
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		if now := time.Now(); cfg == nil || now.Sub(built) >= time.Second {
			if c := active(); c != nil {
				cfg = c
			}
			built = now
		}
		mesh := vc.Mesh()
		var changed <-chan struct{}
		if mesh != nil {
			changed = mesh.Changed()
			// MACsec first: an encrypted link's traffic goes through its
			// device (reference 5.2).
			sec.want(macsecLinks(cfg, vc.Member(), vc.Links()))
			u := stackUnderlay(vc)
			for i := range u.Links {
				u.Links[i].Dev = sec.DataDev(u.Links[i].Port)
			}
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
		// A cable that verifiably cannot carry the largest data frame + the
		// tunnel overhead loses those frames (reference 5.2, stack MTU).
		if cfg != nil && len(cfg.SwitchMembers()) > 1 {
			mtu, _ := cfg.MaxDataMTU()
			for _, p := range vc.Ports() {
				if p.PathMTU == 0 || p.State != "up" {
					continue
				}
				n := mtu + cfg.StackPortOverhead(fmt.Sprintf("%d/%s", vc.Member(), p.Port), p.MACsecOffload)
				msg := ""
				if p.PathMTU+model.EthHeader < n {
					msg = fmt.Sprintf("%d/%d", p.PathMTU+model.EthHeader, n)
				}
				if warned[p.Port] != msg {
					warned[p.Port] = msg
					if msg != "" {
						log.Warn("stack: the stacking cable carries smaller frames than the largest data mtu needs; larger frames are lost",
							"port", p.Port, "cable_frame_bytes", p.PathMTU+model.EthHeader, "needed_frame_bytes", n)
					} else {
						log.Info("stack: the stacking cable carries the frames the largest data mtu needs", "port", p.Port)
					}
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

// macsecLinks decides MACsec for every stacking link (reference 5.2): both
// ends' modes from the configuration, both ports' offload from the
// stacking handshake.
func macsecLinks(cfg *model.Config, member int, links []stack.StackLink) []stackLinkSpec {
	var specs []stackLinkSpec
	for _, l := range links {
		idx := 0
		if i, err := net.InterfaceByName(l.Linux); err == nil {
			idx = i.Index
		}
		specs = append(specs, decideLink(cfg, member, l, idx))
	}
	return specs
}

func decideLink(cfg *model.Config, member int, l stack.StackLink, idx int) stackLinkSpec {
	local, peer := fmt.Sprintf("%d/%s", member, l.Port), fmt.Sprintf("%d/%s", l.Neighbor, l.PeerPort)
	spec := stackLinkSpec{Port: l.Linux, Index: idx, Neighbor: l.Neighbor, PeerMAC: l.NeighborMAC.String(),
		Name: local, PeerName: peer, Offload: l.Offload}
	if cfg == nil {
		spec.Why = "no valid configuration"
		return spec
	}
	spec.Encrypt, spec.Why = cfg.StackLinkMACsec(local, peer, l.Offload, l.PeerOffload)
	spec.Software = cfg.StackMACsecMode(local) == "on" || cfg.StackMACsecMode(peer) == "on"
	return spec
}
