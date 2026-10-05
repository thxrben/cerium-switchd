//go:build linux

package lacp

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"golang.org/x/sys/unix"
)

// Kernel lets member ports of LACP bundles carry traffic.
type Kernel interface {
	SetPort(bundle, port string, on bool) error
	PortsEnabled(bundle string) (map[string]bool, error)
}

// BundleSpec is an LACP bundle of this switch.
type BundleSpec struct {
	Name     string // the bundle device (aeN)
	Config   Config
	MinLinks int
	Ports    []PortSpec
}

// PortSpec is a member port.
type PortSpec struct {
	Linux    string // kernel name
	Name     string // configuration name (x/y/z)
	Number   uint16
	Priority uint16
	// Team is the device that carries the port's traffic in the bundle (a
	// secured port's MACsec device; "": the port itself). LACPDUs always
	// use the port.
	Team string `json:",omitempty"`
	// Held: the port cannot carry traffic yet (MACsec not secured): it is
	// kept out of the bundle, LACP says "not in sync".
	Held bool `json:",omitempty"`
}

// team is the device enabled in the bundle for a port.
func (p PortSpec) team() string {
	if p.Team != "" {
		return p.Team
	}
	return p.Linux
}

// Runtime runs LACP for all bundles of this switch.
type Runtime struct {
	Kernel    Kernel
	StateFile string // negotiated state, for hitless restarts
	SysRoot   string // "/sys"
	Log       *slog.Logger
	// BeforeLeave runs when a held bundle is about to lose its last port
	// in the kernel (its partner has stopped sending on it): the traffic
	// towards the partner can be moved elsewhere first. Called with the
	// runtime locked; it must not call back.
	BeforeLeave func(bundle string)
	// BeforeJoin runs when a bundle is about to carry traffic again (its
	// first port is enabled): the MC-LAG peer must filter traffic from the
	// stack towards its own leg before this leg forwards, or the partner's
	// frames come back to it through the stack. Called with the runtime
	// locked; it must not call back, and it should return quickly.
	BeforeJoin func(bundle string)
	// OnChange is called (with the runtime locked: it must not block or
	// call back) after a port started or stopped carrying traffic.
	OnChange func()

	mu       sync.Mutex
	bundles  map[string]*rtBundle
	socks    map[string]*rtSock // by kernel port name
	restore  map[string]map[string]PortSnapshot
	loaded   bool
	lastSave string
	rx       chan rxFrame
}

type rtBundle struct {
	spec    BundleSpec
	b       *Bundle
	enabled map[string]bool   // what the kernel was told
	team    map[string]string // port -> the device enabled for it (PortSpec.Team)
	// peerReady: ports the MC-LAG peer has ready (minimum-links counts
	// both members' ports).
	peerReady  int
	slowWarned map[string]bool
	deafWarned map[string]bool
}

type rtSock struct {
	fd      int
	ifindex int
	mac     net.HardwareAddr
	bundle  string
	stop    chan struct{}
	sendErr string // the last send error (logged once until a send works)
}

type rxFrame struct {
	port  string
	frame []byte
}

func (r *Runtime) init() {
	if r.bundles == nil {
		r.bundles, r.socks, r.rx = map[string]*rtBundle{}, map[string]*rtSock{}, make(chan rxFrame, 1024)
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.SysRoot == "" {
		r.SysRoot = "/sys"
	}
	if !r.loaded {
		r.loaded = true
		r.restore = map[string]map[string]PortSnapshot{}
		if raw, err := hwio.ReadFile(r.StateFile); err == nil {
			_ = json.Unmarshal(raw, &r.restore)
		}
	}
}

// Sync makes the running bundles match specs (after every apply).
func (r *Runtime) Sync(specs []BundleSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	now := time.Now()
	want := map[string]BundleSpec{}
	for _, s := range specs {
		want[s.Name] = s
	}
	for name, rb := range r.bundles {
		if _, ok := want[name]; !ok {
			for _, p := range rb.b.Ports() {
				r.closeSock(p)
			}
			delete(r.bundles, name)
		}
	}
	for _, s := range specs {
		rb := r.bundles[s.Name]
		if rb == nil {
			rb = &rtBundle{enabled: map[string]bool{}, team: map[string]string{}}
			rb.b = NewBundle(s.Config, func(port string, p *PDU) { r.send(port, p) })
			r.bundles[s.Name] = rb
		}
		rb.spec = s
		rb.b.SetConfig(s.Config)
		wantPorts := map[string]bool{}
		for _, p := range s.Ports {
			wantPorts[p.Linux] = true
		}
		for _, p := range rb.b.Ports() {
			if !wantPorts[p] {
				rb.b.RemovePort(p)
				r.closeSock(p)
				delete(rb.enabled, p)
			}
		}
		// What the kernel does now (after a restart: from the previous run).
		kernel, _ := r.Kernel.PortsEnabled(s.Name)
		for _, p := range s.Ports {
			if r.socks[p.Linux] == nil {
				if err := r.openSock(p.Linux, s.Name); err != nil {
					r.Log.Warn("lacp: port", "port", p.Name, "err", err)
					continue
				}
			}
			if slices.Contains(rb.b.Ports(), p.Linux) {
				rb.b.SetPortHold(p.Linux, p.Held)
				if rb.team[p.Linux] != p.team() {
					// Another device carries it now (MACsec secured or lost):
					// what the kernel has for that one.
					rb.team[p.Linux] = p.team()
					rb.enabled[p.Linux] = kernel[p.team()]
				}
				continue
			}
			rb.b.AddPort(p.Linux, p.Number, p.Priority)
			rb.b.SetPortHold(p.Linux, p.Held)
			rb.team[p.Linux] = p.team()
			rb.enabled[p.Linux] = kernel[p.team()]
			if snap, ok := r.restore[s.Name][p.Linux]; ok && kernel[p.team()] && r.carrier(p.Linux) {
				rb.b.Restore(p.Linux, snap, now)
				r.Log.Info("lacp: port state restored", "bundle", s.Name, "port", p.Name)
			}
		}
	}
	r.restore = nil // only right after starting
}

// Run drives the machines until ctx ends.
func (r *Runtime) Run(ctx context.Context) {
	r.mu.Lock()
	r.init()
	r.mu.Unlock()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			for p := range r.socks {
				r.closeSock(p)
			}
			r.mu.Unlock()
			return
		case f := <-r.rx:
			r.mu.Lock()
			r.receive(f, time.Now())
			r.mu.Unlock()
		case <-t.C:
			r.mu.Lock()
			r.tick(time.Now())
			r.mu.Unlock()
		}
	}
}

func (r *Runtime) receive(f rxFrame, now time.Time) {
	s := r.socks[f.port]
	if s == nil {
		return
	}
	rb := r.bundles[s.bundle]
	if rb == nil {
		return
	}
	pdu, err := ParseFrame(f.frame)
	if err != nil {
		if err != errNotLACP {
			rb.b.RxError(f.port)
		}
		return
	}
	rb.b.Receive(f.port, pdu, now)
	rb.b.Tick(now)
	r.enforce(rb)
}

func (r *Runtime) tick(now time.Time) {
	for _, rb := range r.bundles {
		for _, p := range rb.b.Ports() {
			rb.b.SetLink(p, r.carrier(p), now)
		}
		rb.b.Tick(now)
		r.enforce(rb)
	}
	r.save()
}

// enforce tells the kernel which ports carry traffic (none below
// minimum-links).
func (r *Runtime) enforce(rb *rtBundle) {
	dist := rb.b.Distributing()
	if len(dist) == 0 || len(dist)+rb.peerReady < max(rb.spec.MinLinks, 1) {
		dist = nil
	}
	slow := rb.b.SlowPartners()
	for p := range rb.slowWarned {
		if !slices.Contains(slow, p) {
			delete(rb.slowWarned, p) // warn again if it comes back
		}
	}
	for _, p := range slow {
		if !rb.slowWarned[p] {
			if rb.slowWarned == nil {
				rb.slowWarned = map[string]bool{}
			}
			rb.slowWarned[p] = true
			r.Log.Warn("lacp: the partner sends LACPDUs less often than 'periodic fast' needs; this side times out and the port cannot stay in the bundle. Configure 'aggregated-ether-options lacp periodic slow'",
				"bundle", rb.spec.Name, "port", r.portName(rb, p))
		}
	}
	deaf := rb.b.DeafPartners()
	for p := range rb.deafWarned {
		if !slices.Contains(deaf, p) {
			delete(rb.deafWarned, p)
			r.Log.Info("lacp: the partner receives our LACPDUs again", "bundle", rb.spec.Name, "port", r.portName(rb, p))
		}
	}
	for _, p := range deaf {
		if !rb.deafWarned[p] {
			if rb.deafWarned == nil {
				rb.deafWarned = map[string]bool{}
			}
			rb.deafWarned[p] = true
			r.Log.Warn("lacp: the partner does not receive our LACPDUs (its LACPDUs do not name this port): check the cable, this port's transmit path and the partner's port",
				"bundle", rb.spec.Name, "port", r.portName(rb, p))
		}
	}
	if len(dist) == 0 && rb.b.Held() && r.BeforeLeave != nil {
		for _, on := range rb.enabled {
			if on {
				r.BeforeLeave(rb.spec.Name)
				break
			}
		}
	}
	if len(dist) > 0 && r.BeforeJoin != nil && !slices.Contains(slices.Collect(maps.Values(rb.enabled)), true) {
		r.BeforeJoin(rb.spec.Name)
	}
	for _, p := range rb.b.Ports() {
		on := slices.Contains(dist, p)
		if rb.enabled[p] == on {
			continue
		}
		dev := rb.team[p]
		if dev == "" {
			dev = p
		}
		if err := r.Kernel.SetPort(rb.spec.Name, dev, on); err != nil {
			r.Log.Warn("lacp: port", "bundle", rb.spec.Name, "port", p, "err", err)
			continue
		}
		rb.enabled[p] = on
		if r.OnChange != nil {
			r.OnChange()
		}
		state := "left"
		if on {
			state = "joined"
		}
		r.Log.Info("lacp: port "+state+" the bundle", "bundle", rb.spec.Name, "port", r.portName(rb, p))
	}
}

func (r *Runtime) portName(rb *rtBundle, linux string) string {
	for _, p := range rb.spec.Ports {
		if p.Linux == linux {
			return p.Name
		}
	}
	return linux
}

func (r *Runtime) save() {
	if r.StateFile == "" {
		return
	}
	all := map[string]map[string]PortSnapshot{}
	for n, rb := range r.bundles {
		if s := rb.b.Snapshot(); len(s) > 0 {
			all[n] = s
		}
	}
	raw, _ := json.Marshal(all)
	if string(raw) == r.lastSave {
		return
	}
	tmp := r.StateFile + ".tmp"
	if hwio.WriteFile(tmp, raw, 0o600) == nil && hwio.Rename(tmp, r.StateFile) == nil {
		r.lastSave = string(raw)
	}
}

func (r *Runtime) carrier(port string) bool {
	b, err := hwio.ReadFile(filepath.Join(r.SysRoot, "class", "net", port, "carrier"))
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// SetHold holds a bundle's ports out of the bundle (MC-LAG) or releases
// them.
func (r *Runtime) SetHold(bundle string, hold bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rb := r.bundles[bundle]; rb != nil && rb.b.Held() != hold {
		rb.b.SetHold(hold)
		now := time.Now()
		rb.b.Tick(now)
		r.enforce(rb)
	}
}

// PortEnabled reports whether a member port carries traffic for its bundle
// (the kernel was told so); known is false for ports of no LACP bundle.
func (r *Runtime) PortEnabled(linux string) (enabled, known bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rb := range r.bundles {
		if on, ok := rb.enabled[linux]; ok {
			return on, true
		}
	}
	return false, false
}

// EnabledPorts reports every member port of an LACP bundle (kernel name)
// and whether it carries traffic.
func (r *Runtime) EnabledPorts() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for _, rb := range r.bundles {
		for p, on := range rb.enabled {
			out[p] = on
		}
	}
	return out
}

// Ready reports per bundle how many ports LACP has collecting and
// distributing (before minimum-links).
func (r *Runtime) Ready() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for n, rb := range r.bundles {
		out[n] = len(rb.b.Distributing())
	}
	return out
}

// SetPeerReady tells a bundle how many ports the MC-LAG peer has ready
// (minimum-links counts both members' ports, reference 5.3.2).
func (r *Runtime) SetPeerReady(bundle string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rb := r.bundles[bundle]; rb != nil && rb.peerReady != n {
		rb.peerReady = n
		r.enforce(rb)
	}
}

// Legs reports per bundle whether it carries traffic on this switch (at
// least minimum-links ports are collecting and distributing).
func (r *Runtime) Legs() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for n, rb := range r.bundles {
		up := false
		for _, on := range rb.enabled {
			up = up || on
		}
		out[n] = up
	}
	return out
}

// Status reports every bundle.
func (r *Runtime) Status() []BundleStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []BundleStatus
	for _, n := range sortedKeys(r.bundles) {
		rb := r.bundles[n]
		bs := BundleStatus{Name: n, Ports: rb.b.Status(), PortNames: map[string]string{}}
		for _, p := range rb.spec.Ports {
			bs.PortNames[p.Linux] = p.Name
		}
		out = append(out, bs)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ---- AF_PACKET ----

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func (r *Runtime) openSock(port, bundle string) error {
	ifi, err := net.InterfaceByName(port)
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(EtherType)))
	if err != nil {
		return err
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(EtherType), Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return err
	}
	tv := unix.NsecToTimeval(int64(500 * time.Millisecond))
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	s := &rtSock{fd: fd, ifindex: ifi.Index, mac: ifi.HardwareAddr, bundle: bundle, stop: make(chan struct{})}
	r.socks[port] = s
	go r.read(port, s)
	return nil
}

func (r *Runtime) read(port string, s *rtSock) {
	buf := make([]byte, 1600)
	for {
		select {
		case <-s.stop:
			unix.Close(s.fd)
			return
		default:
		}
		n, _, err := unix.Recvfrom(s.fd, buf, 0)
		if err != nil || n < 14 {
			continue // timeout (checks stop) or a short frame
		}
		f := append([]byte(nil), buf[:n]...)
		select {
		case r.rx <- rxFrame{port, f}:
		default: // overloaded: LACPDUs are repeated
		}
	}
}

func (r *Runtime) closeSock(port string) {
	if s := r.socks[port]; s != nil {
		close(s.stop)
		delete(r.socks, port)
	}
}

func (r *Runtime) send(port string, p *PDU) {
	s := r.socks[port]
	if s == nil {
		return
	}
	var addr [8]byte
	copy(addr[:], Dest)
	err := unix.Sendto(s.fd, p.Frame(s.mac), 0, &unix.SockaddrLinklayer{Protocol: htons(EtherType), Ifindex: s.ifindex, Halen: 6, Addr: addr})
	switch {
	case err != nil && err.Error() != s.sendErr:
		// The LACPDU is counted as sent but did not leave: the partner
		// never sees this port.
		s.sendErr = err.Error()
		r.Log.Warn("lacp: cannot send LACPDUs", "port", port, "err", err)
	case err == nil && s.sendErr != "":
		s.sendErr = ""
		r.Log.Info("lacp: sending LACPDUs again", "port", port)
	}
}
