// Package ospfd is OSPF and OSPFv3 for every routing instance (reference
// 5.13, 5.8, PLAN.md Phase 9c): it runs the protocol core (pkg/ospf) on the
// master, reads the interfaces from the kernel, exports routes by policy
// and gives its routes to cer-ribd, on the master and, replicated, on every
// member. cer-ospfd runs it.
package ospfd

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/hwio"

	"github.com/thxrben/cerium-switchd/internal/api/ribapi"
	"github.com/thxrben/cerium-switchd/internal/policy"
	"github.com/thxrben/cerium-switchd/pkg/ospf"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

// LinkInfo is what the kernel says about a device.
type LinkInfo struct {
	Index     int
	MTU       int
	Up        bool       // administratively up with carrier
	LinkLocal netip.Addr // IPv6 link-local address (invalid: none yet)
	SpeedMbps uint64     // 0: unknown
}

// Kernel reads devices (the Linux implementation is in linux.go).
type Kernel interface {
	Link(dev string) (LinkInfo, bool)
}

// Net opens the packet I/O of an interface.
type Net interface {
	Open(v ospf.Version, dev string, ifindex int, rx func(src, dst netip.Addr, pkt []byte)) (Port, error)
}

// Port is an interface's packet I/O.
type Port interface {
	Send(src, dst netip.Addr, pkt []byte) error
	Close() error
}

// RIB is cer-ribd for this daemon.
type RIB interface {
	SetRoutes(ctx context.Context, sr ribapi.SetRoutes) error
	Active(ctx context.Context, instance string) ([]rib.Entry, error)
}

// Daemon runs the instances. Events are serialised on one goroutine.
type Daemon struct {
	Kernel Kernel
	Net    Net
	RIB    RIB
	Log    *slog.Logger
	// RestartFile keeps the neighbours for a graceful restart (restart.go;
	// "": none); Planned reports at the stop whether it is a restart the
	// supervisor makes (then the neighbours are told, reference 5.13).
	RestartFile string
	Planned     func() bool
	restartFrom *restartData
	restartRead bool
	lastSaved   restartData
	// Replicate gives the routes to the other members (nil: standalone).
	Replicate func(sr ribapi.SetRoutes)
	// Settle is the least time after the start before the routes count as
	// complete (0: 40 s, at least every dead interval).
	Settle time.Duration
	// Member is this member's id; StackCall calls cer-ospfd on another
	// member (the relay of routed interfaces of other members; nil:
	// standalone).
	Member    int
	StackCall func(ctx context.Context, member int, method string, req, resp any) error
	// BFD is cer-bfdd on this member (nil: no BFD).
	BFD BFD
	// Full is told when the external database overflows (a memory slot
	// purpose is full) and when it fits again (nil: nobody).
	Full func(purpose string, full bool)

	events  chan func()
	started time.Time

	mu       sync.Mutex // guards cfg, master and masterID for the setters
	cfg      Config
	master   bool
	masterID int

	// Owned by the event loop.
	insts  map[string]*instance
	rel    relayState                   // a non-master's relay sockets
	remote map[string]map[int]RelayLink // the master's view of relayed devices (key|unit -> member)
	bfd    bfdState
}

type instance struct {
	d     *Daemon
	cfg   Instance
	r     *ospf.Router
	ports map[string]Port // unit -> I/O
	links map[string]LinkInfo
	ifcfg map[string]ospf.IfaceConfig
	owner map[string]int // unit -> the member sending for it (0: this one)
	// sent: the routes last given to the RIB; full: they were complete.
	sent    []rib.Route
	full    bool
	routes  []ospf.Route
	ext     []ospf.External
	started time.Time
}

// New returns a daemon; Run runs it.
func New(k Kernel, n Net, r RIB, log *slog.Logger) *Daemon {
	if log == nil {
		log = slog.Default()
	}
	return &Daemon{Kernel: k, Net: n, RIB: r, Log: log, events: make(chan func(), 1024), insts: map[string]*instance{},
		started: time.Now()}
}

func (d *Daemon) do(f func()) { d.events <- f }

// SetConfig takes switchd's configuration.
func (d *Daemon) SetConfig(c Config) {
	d.mu.Lock()
	d.cfg = c
	d.mu.Unlock()
	d.do(d.apply)
}

// SetMaster tells whether this member is the master (the protocol runs
// there only); standalone or tests.
func (d *Daemon) SetMaster(m bool) {
	id := 0
	if m {
		id = d.Member
	}
	d.SetRole(m, id)
}

// SetRole tells whether this member is the master and which member is.
func (d *Daemon) SetRole(m bool, masterID int) {
	d.mu.Lock()
	changed := d.master != m || d.masterID != masterID
	d.master, d.masterID = m, masterID
	d.mu.Unlock()
	if changed {
		d.do(d.apply)
	}
}

// Run serves events until ctx ends.
func (d *Daemon) Run(ctx context.Context) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	links := time.NewTicker(time.Second)
	defer links.Stop()
	export := time.NewTicker(5 * time.Second)
	defer export.Stop()
	for {
		select {
		case <-ctx.Done():
			d.stopAll()
			return
		case f := <-d.events:
			f()
		case now := <-tick.C:
			for _, k := range d.sortedKeys() {
				d.insts[k].r.Tick(now)
			}
			d.syncBFD(now)
		case now := <-links.C:
			d.refreshLinks()
			d.saveRestart(now, false)
		case <-export.C:
			d.refreshExports(ctx)
		}
	}
}

func (d *Daemon) sortedKeys() []string {
	out := make([]string, 0, len(d.insts))
	for k := range d.insts {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (d *Daemon) stopAll() {
	for _, k := range d.sortedKeys() {
		d.insts[k].stop()
	}
	d.insts = map[string]*instance{}
}

// apply converges the instances with the configuration and the role.
func (d *Daemon) apply() {
	d.mu.Lock()
	cfg, master, masterID := d.cfg, d.master, d.masterID
	d.mu.Unlock()
	me := d.Member
	if master {
		me = masterID
	}
	d.relay(cfg, me, masterID)
	d.relayRole(master, masterID)
	want := map[string]Instance{}
	if master {
		for _, in := range cfg.Instances {
			if in.RouterID != 0 {
				want[in.Key()] = in
			}
		}
	}
	for k, in := range d.insts {
		if w, ok := want[k]; !ok || w.RouterID != in.cfg.RouterID || w.VRF != in.cfg.VRF {
			in.stop()
			delete(d.insts, k)
		}
	}
	now := time.Now()
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range want {
			if !yield(k) {
				return
			}
		}
	}) {
		in := d.insts[k]
		if in == nil {
			in = &instance{d: d, ports: map[string]Port{}, links: map[string]LinkInfo{}, ifcfg: map[string]ospf.IfaceConfig{},
				owner: map[string]int{}, started: now}
			in.r = ospf.New(want[k].Version, (*ioAdapter)(in), d.Log.With("instance", want[k].Name), now)
			in.r.OnRoutes = in.onRoutes
			in.r.OnOverflow = func(o bool) {
				if d.Full != nil {
					go d.Full("ospf", o)
				}
			}
			d.insts[k] = in
		}
		in.r.ExtLimit = cfg.ExtLimit
		fresh := in.cfg.RouterID == 0 // created just now
		in.cfg = want[k]
		in.configure(now)
		if fresh {
			d.startRestart(k, in) // the previous run's neighbours (restart.go)
		}
	}
}

// refreshLinks follows the kernel (carrier, addresses, MTU, speed).
func (d *Daemon) refreshLinks() {
	now := time.Now()
	d.mu.Lock()
	cfg, master, masterID := d.cfg, d.master, d.masterID
	d.mu.Unlock()
	if !master {
		d.relay(cfg, d.Member, masterID)
	}
	for _, k := range d.sortedKeys() {
		in := d.insts[k]
		changed := false
		for _, ic := range in.cfg.Interfaces {
			li, _ := d.Kernel.Link(ic.Device)
			if ic.Device != "" && li != in.links[ic.Unit] {
				changed = true
			}
		}
		if changed || in.overloadChanged(now) {
			in.configure(now)
		}
	}
}

func (in *instance) overloaded(now time.Time) bool {
	c := in.cfg
	return c.Overload && (c.OverloadTimeout == 0 || now.Sub(in.started) < time.Duration(c.OverloadTimeout)*time.Second)
}

func (in *instance) overloadChanged(now time.Time) bool {
	return in.overloaded(now) != in.r.Overloaded()
}

// cost is the interface's cost: configured, or the reference bandwidth
// divided by the speed (at least 1). A device without a known speed (a
// loopback-like unit, a virtual NIC) costs 1.
func cost(ic Iface, refBW uint64, li LinkInfo) uint16 {
	if ic.Metric > 0 {
		return uint16(min(ic.Metric, 65535))
	}
	if li.SpeedMbps == 0 {
		return 1
	}
	c := refBW / (li.SpeedMbps * 1_000_000)
	return uint16(min(max(c, 1), 65535))
}

// configure (re)configures the router from the configuration and the
// kernel; ports open and close with the interfaces.
func (in *instance) configure(now time.Time) {
	d := in.d
	c := in.cfg
	rc := ospf.Config{RouterID: c.RouterID, Overload: in.overloaded(now), Externals: in.ext,
		GracefulRestart: c.GracefulRestart, RestartDuration: time.Duration(c.RestartDuration) * time.Second}
	seen := map[string]bool{}
	for _, ic := range c.Interfaces {
		seen[ic.Unit] = true
		li, ok := LinkInfo{}, false
		owner := 0 // the member sending for us (0: this one)
		if ic.Device != "" {
			li, ok = d.Kernel.Link(ic.Device)
		} else if rel, _ := relayed(ic, -1, d.Member); rel {
			li, owner, ok = d.remoteLink(c.Key(), ic)
		}
		in.links[ic.Unit] = li
		in.owner[ic.Unit] = owner
		ifID := uint32(li.Index)
		if owner != 0 {
			ifID = uint32(owner)<<24 | uint32(li.Index)&0xffffff // unique among this router's interfaces
		}
		oc := ospf.IfaceConfig{Name: ic.Unit, Area: ic.Area, ID: ifID, P2P: ic.P2P, Passive: ic.Passive,
			Cost: cost(ic, c.ReferenceBW, li), Priority: uint8(ic.Priority), Hello: uint16(ic.Hello), Dead: uint32(ic.Dead),
			Retransmit: uint16(ic.Retransmit), TransitDelay: uint16(ic.TransitDelay), MTU: uint16(li.MTU)}
		if c.Version == ospf.V2 {
			for _, p := range ic.Prefixes {
				if p.Addr().Is4() {
					oc.Prefixes = append(oc.Prefixes, p)
				}
			}
			if len(oc.Prefixes) > 0 {
				oc.Addr = oc.Prefixes[0].Addr()
			}
			oc.Auth = auth(ic)
		} else {
			for _, p := range ic.Prefixes {
				if p.Addr().Is6() && !p.Addr().IsLinkLocalUnicast() {
					oc.Prefixes = append(oc.Prefixes, p)
				}
			}
			oc.Addr = li.LinkLocal
		}
		oc.Up = ok && li.Up && oc.Addr.IsValid() && li.Index > 0
		in.ifcfg[ic.Unit] = oc
		rc.Interfaces = append(rc.Interfaces, oc)
		// The port: open on an interface that is up and not passive.
		need := oc.Up && !ic.Passive
		p := in.ports[ic.Unit]
		if rp, isRelay := p.(*relayPort); isRelay && (owner == 0 || rp.member != owner) {
			delete(in.ports, ic.Unit) // another owner, or local now
			p = nil
		}
		switch {
		case need && p == nil && owner != 0:
			if d.StackCall == nil {
				continue
			}
			in.ports[ic.Unit] = &relayPort{d: d, key: c.Key(), unit: ic.Unit, member: owner}
		case need && p == nil:
			unit := ic.Unit
			np, err := d.Net.Open(c.Version, ic.Device, li.Index, func(src, dst netip.Addr, pkt []byte) {
				d.do(func() {
					if cur := d.insts[c.Key()]; cur == in {
						in.r.Receive(unit, src, dst, pkt, time.Now())
					}
				})
			})
			if err != nil {
				d.Log.Warn("ospf: cannot open the interface", "version", c.Version, "unit", ic.Unit, "device", ic.Device, "err", err)
				oc.Up = false
				rc.Interfaces[len(rc.Interfaces)-1] = oc
				in.ifcfg[ic.Unit] = oc
				continue
			}
			in.ports[ic.Unit] = np
		case !need && p != nil:
			p.Close()
			delete(in.ports, ic.Unit)
		}
	}
	for unit, p := range in.ports {
		if !seen[unit] {
			p.Close()
			delete(in.ports, unit)
		}
	}
	in.r.Configure(rc, now)
}

func auth(ic Iface) *ospf.Auth {
	switch {
	case len(ic.MD5) > 0:
		a := &ospf.Auth{Type: ospf.AuthCrypto, Keys: map[uint8]string{}}
		for id, k := range ic.MD5 {
			a.Keys[uint8(id)] = k
			if uint8(id) > a.SendKey || len(a.Keys) == 1 {
				a.SendKey = uint8(id) // the newest (highest) key sends
			}
		}
		return a
	case ic.Simple != "":
		return &ospf.Auth{Type: ospf.AuthSimple, Simple: ic.Simple}
	}
	return nil
}

func (in *instance) stop() {
	for unit, p := range in.ports {
		p.Close()
		delete(in.ports, unit)
	}
}

// ioAdapter sends a router's packets through the instance's ports.
type ioAdapter instance

func (a *ioAdapter) Send(unit string, dst netip.Addr, pkt []byte) {
	in := (*instance)(a)
	p := in.ports[unit]
	if p == nil {
		return
	}
	if err := p.Send(in.ifcfg[unit].Addr, dst, pkt); err != nil {
		in.d.Log.Debug("ospf: send", "unit", unit, "err", err)
	}
}

// ---- routes ----

func (in *instance) protocol() rib.Protocol {
	if in.cfg.Version == ospf.V3 {
		return rib.OSPF3
	}
	return rib.OSPF
}

// converged: every adjacency that can form had the time to (the dead
// interval since the start) and none is still exchanging databases; then
// the routes are complete (cer-ribd may drop older ones of the protocol).
func (in *instance) converged(now time.Time) bool {
	wait := in.d.Settle
	if wait == 0 {
		wait = 40 * time.Second
		for _, ic := range in.cfg.Interfaces {
			wait = max(wait, time.Duration(ic.Dead)*time.Second)
		}
	}
	return now.Sub(in.started) >= wait && !in.r.Exchanging()
}

func (in *instance) onRoutes(rs []ospf.Route) {
	in.routes = rs
	in.push(false)
}

// push gives the routes to cer-ribd when they changed or became complete.
func (in *instance) push(force bool) {
	now := time.Now()
	var out []rib.Route
	for _, r := range in.routes {
		if r.Direct {
			continue
		}
		rr := rib.Route{Prefix: r.Prefix, Protocol: in.protocol(), Preference: rib.PrefOSPF, Metric: r.Cost,
			Attrs: &rib.Attrs{Area: r.Area.String(), PathType: r.Type.String(), Tag: r.Tag}}
		if r.Type >= ospf.External1 {
			rr.Preference = rib.PrefOSPFExternal
		}
		if r.Type == ospf.External2 {
			rr.Metric, rr.Metric2 = r.Cost2, r.Cost
		}
		for _, h := range r.NextHops {
			rr.NextHops = append(rr.NextHops, rib.NextHop{Gateway: h.Gateway, Interface: h.Iface})
		}
		out = append(out, rr)
	}
	full := in.converged(now)
	if !force && full == in.full && slices.EqualFunc(out, in.sent, sameRoute) {
		return
	}
	in.sent, in.full = out, full
	sr := ribapi.SetRoutes{Instance: in.cfg.Name, Protocol: in.protocol(), Routes: out, Full: full}
	d := in.d
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.RIB.SetRoutes(ctx, sr); err != nil {
			d.Log.Warn("ospf: routes to cer-ribd", "err", err)
		}
		if d.Replicate != nil {
			d.Replicate(sr)
		}
	}()
}

func sameRoute(a, b rib.Route) bool {
	return a.Prefix == b.Prefix && a.Metric == b.Metric && a.Metric2 == b.Metric2 && a.Preference == b.Preference &&
		slices.Equal(a.NextHops, b.NextHops)
}

// ---- export (reference 5.11, 5.13) ----

// refreshExports evaluates the export policies on the RIB's active routes
// and re-sends the routes when they became complete.
func (d *Daemon) refreshExports(ctx context.Context) {
	d.mu.Lock()
	pol := policy.New(d.cfg.Policies)
	d.mu.Unlock()
	for _, k := range d.sortedKeys() {
		in := d.insts[k]
		in.push(false)
		if len(in.cfg.Export) == 0 {
			if len(in.ext) > 0 {
				in.ext = nil
				in.r.SetExternals(nil, time.Now())
			}
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		entries, err := d.RIB.Active(cctx, in.cfg.Name)
		cancel()
		if err != nil {
			continue
		}
		ext := exports(pol, in.cfg, entries)
		if !slices.Equal(ext, in.ext) {
			in.ext = ext
			in.r.SetExternals(ext, time.Now())
		}
	}
}

func protoName(p rib.Protocol) string {
	switch p {
	case rib.Direct:
		return "direct"
	case rib.Local:
		return "local"
	case rib.Static, rib.DHCP:
		return "static"
	case rib.OSPF:
		return "ospf"
	case rib.OSPF3:
		return "ospf3"
	case rib.BGP:
		return "bgp"
	}
	return ""
}

// exports returns the routes the export policies accept: the active
// routes of other protocols of the instance's family (OSPF's own are never
// exported again; the attached networks of OSPF interfaces are announced
// as such).
func exports(pol *policy.Engine, c Instance, entries []rib.Entry) []ospf.External {
	var out []ospf.External
	for _, e := range entries {
		if e.Active < 0 || e.Prefix.Addr().Is4() != (c.Version == ospf.V2) {
			continue
		}
		r := e.Routes[e.Active]
		if r.Protocol == rib.OSPF || r.Protocol == rib.OSPF3 || r.Protocol == rib.Local {
			continue
		}
		pr := &policy.Route{Prefix: e.Prefix, Protocol: protoName(r.Protocol), Preference: -1}
		if pol.Evaluate(c.Export, pr) != policy.Accept {
			continue
		}
		x := ospf.External{Prefix: e.Prefix, Type1: pr.ExternalType == 1, Tag: pr.Tag}
		if pr.HasMetric {
			x.Metric = pr.Metric
		}
		out = append(out, x)
	}
	slices.SortFunc(out, func(a, b ospf.External) int {
		if c := a.Prefix.Addr().Compare(b.Prefix.Addr()); c != 0 {
			return c
		}
		return cmp.Compare(a.Prefix.Bits(), b.Prefix.Bits())
	})
	return out
}

// ---- show and clear ----

// Methods served by cer-ospfd, and the stacking-protocol method that
// carries the master's routes to the other members.
const (
	StackRoutes   = "ospf-routes"
	callTimeout   = 2 * time.Second
	notMasterNote = "OSPF runs on the master"
)

// ErrBusy is returned when the event loop does not answer in time.
var ErrBusy = fmt.Errorf("cer-ospfd is busy")

func (d *Daemon) query(f func()) error {
	done := make(chan struct{})
	select {
	case d.events <- func() { f(); close(done) }:
	case <-time.After(callTimeout):
		return ErrBusy
	}
	select {
	case <-done:
		return nil
	case <-time.After(callTimeout):
		return ErrBusy
	}
}

// Status returns the instances' state.
func (d *Daemon) Status(q StatusRequest) ([]InstanceStatus, error) {
	var out []InstanceStatus
	err := d.query(func() {
		for _, k := range d.sortedKeys() {
			in := d.insts[k]
			if (q.Version != 0 && in.cfg.Version != q.Version) || (q.Instance != nil && *q.Instance != in.cfg.Name) {
				continue
			}
			out = append(out, InstanceStatus{Instance: in.cfg.Name, Version: in.cfg.Version, Status: in.r.Status(q.Detail)})
		}
	})
	return out, err
}

// Clear restarts adjacencies; it returns how many.
func (d *Daemon) Clear(q ClearRequest) (int, error) {
	n := 0
	err := d.query(func() {
		for _, in := range d.insts {
			if in.cfg.Version == q.Version && in.cfg.Name == q.Instance {
				n += in.r.ClearNeighbors(q.Neighbor, time.Now())
			}
		}
	})
	return n, err
}

// Resend gives every instance's routes to the RIB and the members again.
func (d *Daemon) Resend() {
	d.do(func() {
		for _, k := range d.sortedKeys() {
			d.insts[k].push(true)
		}
	})
}

// Shutdown ends the adjacencies (the neighbours notice at once) and closes
// the interfaces.
func (d *Daemon) Shutdown(ctx context.Context) {
	planned := d.Planned != nil && d.Planned()
	done := make(chan struct{})
	select {
	case d.events <- func() {
		now := time.Now()
		if planned {
			// A restart: the neighbours keep forwarding to us for the
			// grace period (the kernel keeps our routes); nothing is ended.
			for _, k := range d.sortedKeys() {
				d.insts[k].r.PrepareRestart(ospf.ReasonSoftware)
			}
			d.saveRestart(now, true)
		} else {
			for _, k := range d.sortedKeys() {
				d.insts[k].r.Shutdown(now)
			}
			if d.RestartFile != "" {
				_ = hwio.Remove(d.RestartFile) // a real stop: no graceful restart
			}
		}
		close(done)
	}:
	case <-ctx.Done():
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
		return
	}
	if planned {
		// The acknowledgements of the grace LSAs (the loop keeps running).
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
	}
}
