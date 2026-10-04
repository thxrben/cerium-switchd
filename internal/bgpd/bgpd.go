// Package bgpd is BGP for every routing instance (reference 5.14, 5.8,
// PLAN.md Phase 9b): one speaker (pkg/bgp) per instance on the master,
// policies through internal/policy, routes to cer-ribd on the master and,
// replicated, on every member, exports from the routing table. cer-bgpd
// runs it.
package bgpd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/bfdd"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/policy"
	"github.com/thxrben/cerium-switchd/internal/ribd"
	"github.com/thxrben/cerium-switchd/pkg/bgp"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

// Config is what switchd gives cer-bgpd.
type Config struct {
	Instances []Instance      `json:"instances,omitempty"`
	Policies  *model.Policies `json:"policies,omitempty"`
}

// Instance is BGP of one routing instance.
type Instance struct {
	Name      string     `json:"name"` // "": default
	VRF       string     `json:"vrf,omitempty"`
	AS        uint32     `json:"as"`
	RouterID  netip.Addr `json:"router_id"`
	Neighbors []Neighbor `json:"neighbors,omitempty"`
}

// Neighbor is a neighbour with its policies and BFD.
type Neighbor struct {
	bgp.Neighbor
	Import []string   `json:"import,omitempty"`
	Export []string   `json:"export,omitempty"`
	BFDCfg *BFDConfig `json:"bfd,omitempty"`
}

// BFDConfig is a neighbour's bfd-liveness-detection (reference 5.12).
type BFDConfig struct {
	IntervalMs int    `json:"interval_ms"`
	Multiplier int    `json:"multiplier"`
	AuthType   string `json:"auth_type,omitempty"`
	AuthKeyID  int    `json:"auth_key_id,omitempty"`
	AuthKey    string `json:"auth_key,omitempty"`
	// Multihop (UDP 4784, RFC 5883) from Local: eBGP multihop and iBGP to
	// a neighbour that is not directly connected.
	Multihop bool       `json:"multihop,omitempty"`
	Local    netip.Addr `json:"local,omitempty"`
}

// Net is the sockets of an instance (the Linux implementation is in
// linux.go).
type Net interface {
	// Listen accepts sessions in the instance's VRF; keys are the
	// neighbours' TCP MD5 keys.
	Listen(vrf string, keys map[netip.Addr]string) (Listener, error)
	Dial(ctx context.Context, vrf string, n bgp.Neighbor) (net.Conn, error)
}

// Listener is an instance's listening socket.
type Listener interface {
	Accept() (net.Conn, error)
	SetKeys(keys map[netip.Addr]string) error
	Close() error
}

// RIB is cer-ribd for this daemon.
type RIB interface {
	SetRoutes(ctx context.Context, sr ribd.SetRoutes) error
	Active(ctx context.Context, instance string) ([]rib.Entry, error)
}

// Daemon runs the instances.
type Daemon struct {
	Net Net
	RIB RIB
	Log *slog.Logger
	// Replicate gives the routes to the other members (nil: standalone).
	Replicate func(sr ribd.SetRoutes)
	// BFD is cer-bfdd on this member (nil: no BFD).
	BFD BFD

	bfdMu    sync.Mutex
	bfdKeys  map[string]bfdRef // cer-bfdd key -> neighbour
	bfdSpecs []bfdd.SessionSpec
	bfdSent  bool
	bfdQ     chan []bfdd.SessionSpec

	mu     sync.Mutex
	cfg    Config
	master bool
	insts  map[string]*instance
	ctx    context.Context
}

type instance struct {
	d      *Daemon
	name   string   // the routing instance (never changes)
	cfg    Instance // guarded by d.mu (config())
	sp     *bgp.Speaker
	lis    Listener
	cancel context.CancelFunc
	pol    *policyAdapter

	mu      sync.Mutex
	sources map[string]bool // peer sources last given to cer-ribd
	full    bool
	last    map[string][]rib.Route
	pending *routesUpdate
	running bool
}

// routesUpdate is a table for cer-ribd (the newest one wins).
type routesUpdate struct {
	by   map[string][]rib.Route
	full bool
}

// New returns a daemon; Run runs it.
func New(n Net, r RIB, log *slog.Logger) *Daemon {
	if log == nil {
		log = slog.Default()
	}
	return &Daemon{Net: n, RIB: r, Log: log, insts: map[string]*instance{}}
}

// SetConfig takes switchd's configuration.
func (d *Daemon) SetConfig(c Config) {
	d.mu.Lock()
	d.cfg = c
	d.mu.Unlock()
	d.apply()
}

// SetMaster tells whether this member is the master (BGP runs there only).
func (d *Daemon) SetMaster(m bool) {
	d.mu.Lock()
	changed := d.master != m
	d.master = m
	d.mu.Unlock()
	if changed {
		d.apply()
	}
}

// Run serves until ctx ends: the exports every 5 s.
func (d *Daemon) Run(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
	d.apply()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			d.mu.Lock()
			for k, in := range d.insts {
				in.stop()
				delete(d.insts, k)
			}
			d.mu.Unlock()
			return
		case <-t.C:
			for _, in := range d.instances() {
				in.refreshExports(ctx)
			}
		}
	}
}

func (d *Daemon) instances() []*instance {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*instance, 0, len(d.insts))
	for _, k := range sortedKeys(d.insts) {
		out = append(out, d.insts[k])
	}
	return out
}

// apply converges the instances with the configuration and the role:
// a running instance takes a new configuration in place (only the
// neighbours whose session settings changed are reset).
func (d *Daemon) apply() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx == nil {
		return
	}
	want := map[string]Instance{}
	if d.master {
		for _, in := range d.cfg.Instances {
			if in.AS != 0 && in.RouterID.IsValid() {
				want[in.Name] = in
			}
		}
	}
	for k, in := range d.insts {
		if w, ok := want[k]; !ok || w.VRF != in.cfg.VRF {
			in.stop()
			delete(d.insts, k)
		}
	}
	eng := policy.New(d.cfg.Policies)
	for _, k := range sortedKeys(want) {
		w := want[k]
		in := d.insts[k]
		if in == nil {
			var err error
			if in, err = d.start(w, eng); err != nil {
				d.Log.Error("bgp: instance not started", "instance", w.Name, "err", err)
				continue
			}
			d.insts[k] = in
			continue
		}
		in.configure(w, eng)
	}
	d.syncBFD(want)
}

func (d *Daemon) start(c Instance, eng *policy.Engine) (*instance, error) {
	lis, err := d.Net.Listen(c.VRF, keys(c))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(d.ctx)
	in := &instance{d: d, name: c.Name, cfg: c, lis: lis, cancel: cancel, sources: map[string]bool{}}
	in.pol = &policyAdapter{eng: eng, chains: chains(c)}
	in.sp = bgp.New(dialer{d.Net, c.VRF}, in.pol.policy(), d.Log.With("instance", instName(c.Name)))
	in.sp.OnRoutes = in.onRoutes
	go in.sp.Run(ctx)
	in.sp.Configure(speakerConfig(c))
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			if tc, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
				ra, _ := netip.AddrFromSlice(tc.IP)
				for _, n := range in.config().Neighbors {
					if n.Addr == ra.Unmap() {
						SetAcceptedTTL(conn, n.Neighbor)
					}
				}
			}
			in.sp.Accept(conn)
		}
	}()
	go in.refreshExports(ctx)
	return in, nil
}

func (in *instance) config() Instance {
	in.d.mu.Lock()
	defer in.d.mu.Unlock()
	return in.cfg
}

func (in *instance) configure(c Instance, eng *policy.Engine) {
	old := in.cfg
	in.cfg = c
	if err := in.lis.SetKeys(keys(c)); err != nil {
		in.d.Log.Warn("bgp: TCP MD5 keys", "instance", instName(c.Name), "err", err)
	}
	in.sp.Configure(speakerConfig(c))
	np := &policyAdapter{eng: eng, chains: chains(c)}
	if !in.pol.same(np) || !samePolicies(old, c) {
		in.pol = np
		in.sp.SetPolicy(np.policy())
	}
}

func (in *instance) stop() {
	in.cancel()
	in.lis.Close()
	// The routes go with the instance (the routing table keeps them for
	// its grace time when cer-bgpd itself restarts).
	in.mu.Lock()
	srcs := in.sources
	in.sources = map[string]bool{}
	in.mu.Unlock()
	for s := range srcs {
		in.d.setRoutes(ribd.SetRoutes{Instance: in.name, Protocol: rib.BGP, Source: s})
	}
}

func instName(n string) string {
	if n == "" {
		return "default"
	}
	return n
}

func keys(c Instance) map[netip.Addr]string {
	out := map[netip.Addr]string{}
	for _, n := range c.Neighbors {
		if n.AuthKey != "" && !n.Disabled {
			out[n.Addr] = n.AuthKey
		}
	}
	return out
}

func chains(c Instance) map[netip.Addr][2][]string {
	out := map[netip.Addr][2][]string{}
	for _, n := range c.Neighbors {
		out[n.Addr] = [2][]string{n.Import, n.Export}
	}
	return out
}

func samePolicies(a, b Instance) bool {
	return fmt.Sprint(chains(a)) == fmt.Sprint(chains(b))
}

func speakerConfig(c Instance) bgp.Config {
	sc := bgp.Config{AS: c.AS, RouterID: c.RouterID}
	for _, n := range c.Neighbors {
		bn := n.Neighbor
		bn.BFD = n.BFDCfg != nil
		sc.Neighbors = append(sc.Neighbors, bn)
	}
	return sc
}

// dialer is the speaker's transport for one instance.
type dialer struct {
	n   Net
	vrf string
}

func (t dialer) Dial(ctx context.Context, n bgp.Neighbor) (net.Conn, error) {
	return t.n.Dial(ctx, t.vrf, n)
}

// onRoutes gives the speaker's table to cer-ribd: one source per
// neighbour, so "show route" lists every BGP path of a destination. It
// runs on the speaker's loop: the calls happen on a worker, newest first.
func (in *instance) onRoutes(rs []bgp.Route, converged bool) {
	by := map[string][]rib.Route{}
	for _, r := range rs {
		src := r.Peer.String()
		by[src] = append(by[src], ribRoute(r))
	}
	in.mu.Lock()
	in.pending = &routesUpdate{by: by, full: converged}
	if in.running {
		in.mu.Unlock()
		return
	}
	in.running = true
	in.mu.Unlock()
	go func() {
		for {
			in.mu.Lock()
			u := in.pending
			in.pending = nil
			if u == nil {
				in.running = false
				in.mu.Unlock()
				return
			}
			in.mu.Unlock()
			in.give(u.by, u.full)
		}
	}()
}

// give sends a table to cer-ribd (and the members).
func (in *instance) give(by map[string][]rib.Route, full bool) {
	in.mu.Lock()
	in.last = by
	gone := []string{}
	for s := range in.sources {
		if _, ok := by[s]; !ok {
			gone = append(gone, s)
		}
	}
	in.sources = map[string]bool{}
	for s := range by {
		in.sources[s] = true
	}
	in.mu.Unlock()
	for _, s := range sortedKeys(by) {
		in.d.setRoutes(ribd.SetRoutes{Instance: in.name, Protocol: rib.BGP, Source: s, Routes: by[s], Full: full})
	}
	for _, s := range gone {
		in.d.setRoutes(ribd.SetRoutes{Instance: in.name, Protocol: rib.BGP, Source: s, Full: full})
	}
	if len(by) == 0 && full {
		in.d.setRoutes(ribd.SetRoutes{Instance: in.name, Protocol: rib.BGP, Full: true})
	}
	in.mu.Lock()
	in.full = full
	in.mu.Unlock()
}

func (d *Daemon) setRoutes(sr ribd.SetRoutes) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.RIB.SetRoutes(ctx, sr); err != nil {
		d.Log.Warn("bgp: routes not given to the routing table", "err", err)
	}
	if d.Replicate != nil {
		d.Replicate(sr)
	}
}

// ribRoute converts a BGP path (its next hop is resolved by cer-ribd).
func ribRoute(r bgp.Route) rib.Route {
	out := rib.Route{Prefix: r.Prefix, Protocol: rib.BGP, Preference: rib.PrefBGP, Rank: r.Rank, Since: r.Since, Stale: r.Stale,
		NextHops: []rib.NextHop{{Gateway: r.NextHop}}}
	if r.MED != nil {
		out.Metric = *r.MED
	}
	a := &rib.Attrs{Peer: r.Peer.String(), PeerAS: r.PeerAS, ASPath: r.PathString(), LocalPref: r.LocalPref}
	if r.LocalPref == nil {
		lp := uint32(100)
		a.LocalPref = &lp
	}
	for _, c := range r.Communities {
		a.Communities = append(a.Communities, policy.FormatCommunity(c))
	}
	for _, l := range r.Large {
		a.Communities = append(a.Communities, fmt.Sprintf("large:%d:%d:%d", l[0], l[1], l[2]))
	}
	if r.OriginatorID.IsValid() {
		a.Originator = r.OriginatorID.String()
	}
	for _, c := range r.ClusterList {
		a.ClusterList = append(a.ClusterList, c.String())
	}
	if r.Rank > 0 {
		a.InactiveReason = "Not Best in its group"
	}
	out.Attrs = a
	return out
}

// refreshExports gives the speaker the active routes of the other
// protocols: an export policy decides per neighbour what is announced.
func (in *instance) refreshExports(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	es, err := in.d.RIB.Active(cctx, in.name)
	if err != nil {
		return
	}
	var local []bgp.Path
	for _, e := range es {
		if e.Active < 0 {
			continue
		}
		r := e.Routes[e.Active]
		if r.Protocol == rib.BGP {
			continue
		}
		p := bgp.Path{Prefix: e.Prefix, Source: strings.ToLower(r.Protocol.String()), Attrs: bgp.Attrs{Origin: bgp.OriginIGP}}
		if r.Protocol == rib.DHCP {
			p.Source = "static"
		}
		local = append(local, p)
	}
	in.sp.SetLocal(local, nil)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ---- policies ----

// policyAdapter evaluates the neighbours' policy chains on BGP paths.
type policyAdapter struct {
	eng    *policy.Engine
	chains map[netip.Addr][2][]string // import, export
}

func (a *policyAdapter) same(b *policyAdapter) bool {
	return fmt.Sprint(a.chains) == fmt.Sprint(b.chains) && a.eng == b.eng
}

func (a *policyAdapter) policy() bgp.Policy {
	return bgp.Policy{
		Import: func(n *bgp.Neighbor, p *bgp.Path) bool {
			r := toPolicy(p, "bgp")
			res := a.eng.Evaluate(a.chains[n.Addr][0], &r)
			if res == policy.Reject {
				return false
			}
			fromPolicy(&r, p)
			return true // the default import policy accepts
		},
		Export: func(n *bgp.Neighbor, p *bgp.Path) bool {
			proto := "bgp"
			if p.Local() {
				proto = p.Source
			}
			r := toPolicy(p, proto)
			switch a.eng.Evaluate(a.chains[n.Addr][1], &r) {
			case policy.Reject:
				return false
			case policy.Default:
				if p.Local() {
					return false // the default export policy announces BGP routes only
				}
			}
			fromPolicy(&r, p)
			return true
		},
	}
}

func toPolicy(p *bgp.Path, proto string) policy.Route {
	r := policy.Route{Prefix: p.Prefix, Protocol: proto, Neighbor: p.Peer, ASPath: p.ASNs(), Preference: -1}
	for _, c := range p.Communities {
		r.Communities = append(r.Communities, policy.FormatCommunity(c))
	}
	for _, l := range p.Large {
		r.Communities = append(r.Communities, fmt.Sprintf("large:%d:%d:%d", l[0], l[1], l[2]))
	}
	if p.LocalPref != nil {
		r.LocalPref = *p.LocalPref
	} else {
		r.LocalPref = 100
	}
	if p.MED != nil {
		r.MED, r.HasMED = *p.MED, true
	}
	return r
}

// fromPolicy takes the actions' results back into the path.
func fromPolicy(r *policy.Route, p *bgp.Path) {
	lp := r.LocalPref
	if p.LocalPref != nil || lp != 100 {
		p.LocalPref = &lp
	}
	if r.HasMetric {
		m := r.Metric
		p.MED, p.PolicyMED = &m, true
	}
	// AS path prepends are at the front of the flat list.
	if n := len(r.ASPath) - len(p.ASNs()); n > 0 {
		p.Prepend(r.ASPath[:n]...)
	}
	p.Communities, p.Large = nil, nil
	for _, c := range r.Communities {
		if v, ok := policy.ParseCommunity(c); ok {
			p.Communities = append(p.Communities, v)
			continue
		}
		var l [3]uint32
		if _, err := fmt.Sscanf(c, "large:%d:%d:%d", &l[0], &l[1], &l[2]); err == nil {
			p.Large = append(p.Large, l)
		}
	}
	switch nh := r.NextHop; {
	case nh == "self":
		p.NextHopSelf = true
	case nh != "" && nh != "discard":
		if a, err := netip.ParseAddr(nh); err == nil {
			p.NextHop, p.PolicyNextHop = a, true
		}
	}
}

// ---- show and clear ----

// Methods served by cer-bgpd.
const (
	MethodStatus = "bgp.status"
	MethodAdj    = "bgp.adj"
	MethodClear  = "bgp.clear"
	// StackRoutes is the master's routes for the other members.
	StackRoutes = "bgp-routes"
)

// InstanceStatus is show bgp summary|neighbor of one instance.
type InstanceStatus struct {
	Instance  string               `json:"instance"`
	AS        uint32               `json:"as"`
	RouterID  netip.Addr           `json:"router_id"`
	Neighbors []bgp.NeighborStatus `json:"neighbors"`
}

// Status lists the instances (instance nil: all).
func (d *Daemon) Status(instance *string) []InstanceStatus {
	var out []InstanceStatus
	for _, in := range d.instances() {
		if instance != nil && *instance != in.name {
			continue
		}
		ns := in.sp.Status()
		cfg := in.config()
		byAddr := map[netip.Addr]Neighbor{}
		for _, n := range cfg.Neighbors {
			byAddr[n.Addr] = n
		}
		for i := range ns {
			n := byAddr[ns[i].Addr]
			ns[i].Group, ns[i].Import, ns[i].Export = n.Group, n.Import, n.Export
		}
		out = append(out, InstanceStatus{Instance: in.name, AS: cfg.AS, RouterID: cfg.RouterID, Neighbors: ns})
	}
	return out
}

// AdjRequest asks for a neighbour's Adj-RIB-In (received) or -Out.
type AdjRequest struct {
	Instance string     `json:"instance"`
	Neighbor netip.Addr `json:"neighbor"`
	Out      bool       `json:"out,omitempty"`
}

// Adj answers an AdjRequest.
func (d *Daemon) Adj(q AdjRequest) ([]bgp.InPath, error) {
	for _, in := range d.instances() {
		if in.name != q.Instance {
			continue
		}
		if q.Out {
			var out []bgp.InPath
			for _, p := range in.sp.AdjOut(q.Neighbor) {
				out = append(out, bgp.InPath{Path: p})
			}
			return out, nil
		}
		return in.sp.AdjIn(q.Neighbor), nil
	}
	return nil, fmt.Errorf("BGP does not run in instance %s", instName(q.Instance))
}

// ClearRequest is clear bgp neighbor.
type ClearRequest struct {
	Instance string     `json:"instance"`
	Neighbor netip.Addr `json:"neighbor,omitempty"` // invalid: all
	Mode     string     `json:"mode,omitempty"`     // "", soft, soft-inbound
}

// Clear answers a ClearRequest with the number of neighbours.
func (d *Daemon) Clear(q ClearRequest) (int, error) {
	for _, in := range d.instances() {
		if in.name == q.Instance {
			return in.sp.Clear(q.Neighbor, q.Mode), nil
		}
	}
	return 0, fmt.Errorf("BGP does not run in instance %s", instName(q.Instance))
}

// Resend gives the routes to the members again (a member became
// reachable).
func (d *Daemon) Resend() {
	for _, in := range d.instances() {
		in.mu.Lock()
		last := in.last
		full := in.full
		in.mu.Unlock()
		if d.Replicate == nil {
			continue
		}
		for _, s := range sortedKeys(last) {
			d.Replicate(ribd.SetRoutes{Instance: in.name, Protocol: rib.BGP, Source: s, Routes: last[s], Full: full})
		}
	}
}
