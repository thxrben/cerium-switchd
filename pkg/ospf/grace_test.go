package ospf

import (
	"testing"
	"time"
)

// grNet: r1 - l1 - r2 - l2 - r3, r3 with a stub prefix; graceful restart on.
func grNet(t *testing.T, v Version) (*simNet, string) {
	n := newSim(t, v)
	for _, r := range []struct{ name, id string }{{"r1", "1.1.1.1"}, {"r2", "2.2.2.2"}, {"r3", "3.3.3.3"}} {
		sr := n.router(r.name, r.id)
		sr.cfg.GracefulRestart, sr.cfg.RestartDuration = true, 60*time.Second
	}
	n.connect("l1", []string{"r1", "r2"})
	n.connect("l2", []string{"r2", "r3"})
	lo := v4v6(v, "192.0.2.3/32", "2001:db8:ff::3/128")
	n.stub("r3", "lo", lo)
	n.start()
	n.run(60 * time.Second)
	if n.route("r1", lo) == nil {
		t.Fatalf("no route to r3:\n%s", n.dump("r1"))
	}
	return n, lo
}

func (n *simNet) helping(router, ifname string, nbr ID) bool {
	for _, x := range n.routers[router].r.ifaces[ifname].nbrs {
		if x.id == nbr {
			return x.helping()
		}
	}
	return false
}

// r2's program restarts (no warning): its neighbours help it, r1's route
// through r2 never disappears, r2 reports no routes until its restart is
// over and then the full table.
func TestGracefulRestartUnplanned(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n, lo := grNet(t, v)
		sr := n.routers["r2"]
		nbrs := sr.r.AdjacentNeighbors()
		if len(nbrs["l1"]) != 1 || len(nbrs["l2"]) != 1 {
			t.Fatalf("neighbours %v", nbrs)
		}
		reports := 0
		sr.r = New(v, sr, quiet, n.now)
		sr.r.OnRoutes = func([]Route) { reports++ }
		sr.r.Configure(sr.cfg, n.now)
		sr.r.StartRestart(nbrs, n.now, ReasonUnknown)
		began := n.now
		if !sr.r.Restarting() {
			t.Fatal("not restarting")
		}
		helped := false
		for range 60 {
			n.run(time.Second)
			if rt := n.route("r1", lo); rt == nil {
				t.Fatalf("r1 lost the route through r2 during the restart:\n%s", n.dump("r1"))
			}
			helped = helped || n.helping("r1", "l1", id("2.2.2.2"))
			if sr.r.Restarting() && reports > 0 {
				t.Fatal("r2 reported routes while restarting")
			}
			if !sr.r.Restarting() {
				break
			}
		}
		if sr.r.Restarting() || !helped {
			t.Fatalf("restarting %v, r1 helped %v", sr.r.Restarting(), helped)
		}
		if took := n.now.Sub(began); took > 15*time.Second {
			t.Errorf("the restart took %s (the adjacencies should be back within a few hellos)", took)
		}
		n.run(5 * time.Second)
		if reports == 0 || n.route("r2", lo) == nil {
			t.Fatalf("r2's routes after the restart (%d reports):\n%s", reports, n.dump("r2"))
		}
		if n.helping("r1", "l1", id("2.2.2.2")) || n.helping("r3", "l2", id("2.2.2.2")) {
			t.Fatal("still helping after the grace LSA was flushed")
		}
	})
}

// A neighbour that announced a restart but never comes back: helping ends
// with the grace period, then the adjacency and the routes go.
func TestGracefulRestartHelperTimeout(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n, lo := grNet(t, v)
		sr := n.routers["r2"]
		sr.r.PrepareRestart(ReasonSoftware) // planned: grace LSAs flooded
		n.deliver()
		if !n.helping("r1", "l1", id("2.2.2.2")) {
			t.Fatal("r1 does not help after the planned announcement")
		}
		n.segs["l1"].down, n.segs["l2"].down = true, true // r2 is gone
		n.run(50 * time.Second)
		if !n.helping("r1", "l1", id("2.2.2.2")) || n.route("r1", lo) == nil {
			t.Fatal("helping ended before the grace period (and the dead interval passed)")
		}
		n.run(15 * time.Second)
		if n.helping("r1", "l1", id("2.2.2.2")) {
			t.Fatal("still helping after the grace period")
		}
		if rt := n.route("r1", lo); rt != nil {
			t.Fatalf("route through the dead neighbour kept: %+v", rt)
		}
	})
}

// Strict LSA checking: a topology change while helping ends the help (the
// restarting router would not learn it in time).
func TestGracefulRestartTopologyChange(t *testing.T) {
	versions(t, func(t *testing.T, v Version) {
		n, _ := grNet(t, v)
		sr := n.routers["r2"]
		nbrs := sr.r.AdjacentNeighbors()
		sr.r = New(v, sr, quiet, n.now)
		n.segs["l1"].down = true // r2 restarts but cannot reach r1 yet
		sr.r.Configure(sr.cfg, n.now)
		sr.r.StartRestart(nbrs, n.now, ReasonUnknown)
		n.segs["l1"].down = false
		n.run(200 * time.Millisecond)
		if !n.helping("r1", "l1", id("2.2.2.2")) {
			t.Fatal("r1 does not help")
		}
		// r1's cost changes: its router LSA changes and is flooded to r2.
		r1 := n.routers["r1"]
		r1.cfg.Interfaces[0].Cost = 25
		r1.r.Configure(r1.cfg, n.now)
		n.run(time.Second)
		if !sr.r.Restarting() {
			t.Fatal("the restart ended before the change: the test proves nothing")
		}
		if n.helping("r1", "l1", id("2.2.2.2")) {
			t.Fatal("still helping after a topology change")
		}
	})
}
