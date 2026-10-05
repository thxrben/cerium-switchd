package stp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thxrben/cerium-switchd/internal/names"
	"github.com/thxrben/cerium-switchd/internal/schema"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"github.com/thxrben/cerium-switchd/pkg/netdev"
	"github.com/thxrben/cerium-switchd/pkg/rstp"
)

// RSTP for the whole stack as one bridge (reference 5.5): every member
// watches its own RSTP ports (link, speed, BPDUs), and the RSTP owner (the
// lowest member id it reaches) runs the state machines of all ports,
// sends the members their port states and the BPDUs to transmit, and
// copies its state to every member so the next owner continues from it.

// rstpFact is what a member reports about one of its RSTP ports.
type rstpFact struct {
	Up         bool `json:"up"`
	Mbps       int  `json:"mbps,omitempty"`
	FullDuplex bool `json:"fd,omitempty"`
	// Flaps counts link resets the member noticed (the kernel put the
	// port back to blocking): the owner restarts the port's handshake.
	Flaps int `json:"flaps,omitempty"`
}

type rstpFactsMsg struct {
	Facts map[string]rstpFact `json:"facts"`
}

type rstpRxMsg struct {
	Port string `json:"port"`
	BPDU []byte `json:"bpdu"`
}

type rstpTx struct {
	Port string `json:"port"`
	BPDU []byte `json:"bpdu"`
}

// rstpCmdMsg is what the owner sends a member.
type rstpCmdMsg struct {
	States map[string]int `json:"states,omitempty"` // this member's ports -> kernel state (complete)
	Tx     []rstpTx       `json:"tx,omitempty"`
	Flush  []string       `json:"flush,omitempty"`
}

type rstpSnapMsg struct {
	Snap    rstp.Snapshot     `json:"snap"`
	Numbers map[string]uint16 `json:"numbers"`
	At      time.Time         `json:"at"`
}

// rstpSnapFresh: a copy older than this is not used to continue (after a
// reboot the kernel starts every port blocking anyway).
const rstpSnapFresh = 10 * time.Second

type rstpSock struct {
	fd      int
	ifindex int
	mac     [6]byte
	stop    chan struct{}
}

type Controller struct {
	member    int
	stack     Stack // nil: standalone
	stateFile string
	log       *slog.Logger
	// legs reports per LACP bundle whether this member's leg carries
	// traffic (nil: carrier only).
	Legs func() map[string]bool
	// Alarm tells the operators (every CLI session) when RSTP cannot run
	// although configured (nil: log only).
	Alarm   func(text string)
	lastErr string

	mu  sync.Mutex
	cfg *Config
	on  bool // RSTP configured (bridge in user-space STP)

	// Member side.
	socks   map[string]*rstpSock // local RSTP port -> socket (by port name)
	devs    map[string]string    // local RSTP port -> kernel device
	facts   map[string]rstpFact
	sentAt  time.Time
	sentTo  int
	desired map[string]int // from the owner: local port -> kernel state
	snap    *rstpSnapMsg
	savedAt time.Time
	queues  map[int]chan func()
	lastOwn int

	// Owner side.
	br          *rstp.Bridge
	numbers     map[string]uint16
	byNum       map[uint16]string
	remote      map[int]map[string]rstpFact // member -> its facts
	flaps       map[string]int              // flaps already handled
	states      map[string]int              // port -> kernel state
	pending     map[int]*rstpCmdMsg         // outputs of the current run
	snapChanged bool
	lastSnap    time.Time
	lastTick    time.Time
}

func New(member int, stack Stack, stateDir string, log *slog.Logger) *Controller {
	r := &Controller{member: member, stack: stack, log: log,
		stateFile: filepath.Join(stateDir, "rstp.json"),
		socks:     map[string]*rstpSock{}, devs: map[string]string{}, facts: map[string]rstpFact{},
		desired: map[string]int{}, queues: map[int]chan func(){}, remote: map[int]map[string]rstpFact{},
		flaps: map[string]int{}, states: map[string]int{}}
	if raw, err := hwio.ReadFile(r.stateFile); err == nil {
		var s rstpSnapMsg
		if json.Unmarshal(raw, &s) == nil && time.Since(s.At) < rstpSnapFresh {
			r.snap = &s
		}
	}
	if stack != nil {
		stack.Handle("rstp-facts", func(from int, req json.RawMessage) (any, error) {
			var msg rstpFactsMsg
			if err := json.Unmarshal(req, &msg); err != nil {
				return nil, err
			}
			r.mu.Lock()
			r.remote[from] = msg.Facts
			if r.br != nil {
				r.syncPortsLocked()
			}
			r.mu.Unlock()
			r.dispatch()
			return nil, nil
		})
		stack.Handle("rstp-rx", func(from int, req json.RawMessage) (any, error) {
			var msg rstpRxMsg
			if err := json.Unmarshal(req, &msg); err != nil {
				return nil, err
			}
			r.received(msg.Port, msg.BPDU)
			return nil, nil
		})
		stack.Handle("rstp-cmd", func(from int, req json.RawMessage) (any, error) {
			var msg rstpCmdMsg
			if err := json.Unmarshal(req, &msg); err != nil {
				return nil, err
			}
			r.mu.Lock()
			own := r.ownerLocked()
			r.mu.Unlock()
			if from != own {
				return nil, nil // an owner this member does not follow (the topology is settling)
			}
			r.command(&msg)
			return nil, nil
		})
		stack.Handle("rstp-snap", func(from int, req json.RawMessage) (any, error) {
			var msg rstpSnapMsg
			if err := json.Unmarshal(req, &msg); err != nil {
				return nil, err
			}
			r.mu.Lock()
			if from == r.ownerLocked() && r.br == nil {
				msg.At = time.Now()
				r.snap = &msg
				r.saveLocked(false)
			}
			r.mu.Unlock()
			return nil, nil
		})
		stack.Handle("rstp-status", func(from int, req json.RawMessage) (any, error) {
			return r.localStatus(), nil
		})
		stack.Handle("rstp-clear", func(from int, req json.RawMessage) (any, error) {
			var c ClearRequest
			if err := json.Unmarshal(req, &c); err != nil {
				return nil, err
			}
			return nil, r.clearLocal(c)
		})
	}
	return r
}

// setConfig takes the applied configuration.
func (r *Controller) SetConfig(cfg *Config) {
	r.mu.Lock()
	r.cfg = cfg
	r.syncModeLocked()
	if r.br != nil {
		r.br.SetConfig(r.bridgeConfigLocked())
		r.syncPortsLocked()
	}
	r.mu.Unlock()
	r.dispatch()
}

// syncModeLocked switches the bridge between user-space STP (RSTP
// configured) and no STP.
func (r *Controller) syncModeLocked() {
	on := r.cfg != nil && r.cfg.On
	if on == r.on {
		return
	}
	if err := netdev.SetBridgeSTP(names.Bridge, on); err != nil {
		// Once per distinct error (it is retried with every change), and
		// an alarm: without STP the stack forwards on every port.
		if msg := err.Error(); msg != r.lastErr {
			r.lastErr = msg
			r.log.Error("rstp: cannot switch STP on the bridge", "on", on, "err", err)
			if on && r.Alarm != nil {
				r.Alarm("ALARM: RSTP is configured but does not run: " + msg)
			}
		}
		return
	}
	if r.lastErr != "" && r.Alarm != nil && on {
		r.Alarm("RSTP runs again")
	}
	r.lastErr = ""
	r.on = on
	if on {
		r.log.Info("rstp: running (the stack is one bridge)")
		return
	}
	r.log.Info("rstp: off, every port forwards")
	r.br, r.desired = nil, map[string]int{}
	for p := range r.socks {
		r.closeSockLocked(p)
	}
}

func (r *Controller) bridgeConfigLocked() rstp.BridgeConfig {
	c := r.cfg.Bridge
	sum := sha256.Sum256([]byte("ceros rstp bridge\x00" + r.cfg.StackID))
	var mac [6]byte
	copy(mac[:], sum[:6])
	mac[0] = mac[0]&^1 | 2
	return rstp.BridgeConfig{ID: rstp.MakeBridgeID(uint16(c.BridgePriority), mac), HelloTime: c.HelloTime, MaxAge: c.MaxAge, ForwardDelay: c.ForwardDelay}
}

// rstpPort describes one RSTP port of the stack.
type rstpPort struct {
	name    string
	members []int // members with a device for it (an MC-LAG bundle: both)
	cfg     *PortConfig
}

// portsLocked lists the RSTP ports of the stack (reference 5.5): switch
// ports that are not bundle members, and aggregated interfaces.
func (r *Controller) portsLocked() map[string]rstpPort {
	out := map[string]rstpPort{}
	c := r.cfg
	if c == nil || !c.On {
		return out
	}
	for n, p := range c.Ports {
		members := slices.Clone(p.Members)
		sort.Ints(members)
		out[n] = rstpPort{name: n, members: members, cfg: p.Config}
	}
	return out
}

// rstpNumbers gives every port a stable 12-bit port number: physical ports
// of members 1-15 with card and port below 16 get (member-1)<<8 | card<<4 |
// port; the others (bundles, larger numbers) take the lowest free numbers
// from 0xf00 up, in name order.
func rstpNumbers(names []string) map[string]uint16 {
	out := map[string]uint16{}
	used := map[uint16]bool{}
	var rest []string
	for _, n := range names {
		if p, ok := schema.ParsePhysical(n); ok && p.Member >= 1 && p.Member <= 15 && p.Card < 16 && p.Port < 16 {
			v := uint16((p.Member-1)<<8 | p.Card<<4 | p.Port)
			if v == 0 {
				v = 0xeff // 1/0/0: port number 0 is not allowed
			}
			if !used[v] {
				out[n], used[v] = v, true
				continue
			}
		}
		rest = append(rest, n)
	}
	sort.Slice(rest, func(i, j int) bool { return natLess(rest[i], rest[j]) })
	next := uint16(0xf00)
	for _, n := range rest {
		for used[next] && next < 0xfff {
			next++
		}
		out[n], used[next] = next, true
	}
	return out
}

func natLess(a, b string) bool {
	na, ea := strconv.Atoi(strings.TrimPrefix(a, "ae"))
	nb, eb := strconv.Atoi(strings.TrimPrefix(b, "ae"))
	if ea == nil && eb == nil && strings.HasPrefix(a, "ae") && strings.HasPrefix(b, "ae") {
		return na < nb
	}
	return a < b
}

// ownerLocked is the RSTP owner: the lowest member id this member reaches
// (itself included), members in maintenance mode only if nothing else.
func (r *Controller) ownerLocked() int {
	cands := []int{r.member}
	var draining []int
	if r.stack != nil {
		cands = append(cands, r.stack.Reachable()...)
		draining = r.stack.Draining()
	}
	best, bestDrain := 0, 0
	for _, id := range cands {
		if slices.Contains(draining, id) {
			if bestDrain == 0 || id < bestDrain {
				bestDrain = id
			}
			continue
		}
		if best == 0 || id < best {
			best = id
		}
	}
	if best == 0 {
		return bestDrain
	}
	return best
}

// ---- member side ----

func (r *Controller) Run(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			for p := range r.socks {
				r.closeSockLocked(p)
			}
			r.mu.Unlock()
			return
		case <-t.C:
			r.step(time.Now())
		}
	}
}

func (r *Controller) step(now time.Time) {
	r.mu.Lock()
	r.syncModeLocked() // (retries when the bridge was not there yet)
	if !r.on || r.cfg == nil {
		r.mu.Unlock()
		return
	}
	ports := r.portsLocked()
	// Local devices and sockets.
	local := map[string]string{}
	for n, p := range ports {
		if !slices.Contains(p.members, r.member) {
			continue
		}
		dev := r.cfg.Ports[n].Device
		if dev == "" {
			continue // not plugged in
		}
		local[n] = dev
	}
	for n := range r.socks {
		if local[n] != r.devs[n] {
			r.closeSockLocked(n)
		}
	}
	for n, dev := range local {
		if r.socks[n] == nil {
			if err := r.openSockLocked(n, dev); err != nil {
				r.log.Debug("rstp: socket", "port", n, "err", err)
			}
		}
	}
	r.devs = local
	// Facts.
	facts := map[string]rstpFact{}
	for n, dev := range local {
		f := r.readFact(n, dev)
		prev := r.facts[n]
		f.Flaps = prev.Flaps
		// The kernel resets a port to blocking when its link returns: a
		// flap too short for this poll to see.
		if want, ok := r.desired[n]; ok && want == netdev.PortForwarding && f.Up && prev.Up {
			if st, err := netdev.PortSTPState(dev); err == nil && st != netdev.PortForwarding {
				f.Flaps++
			}
		}
		facts[n] = f
	}
	owner := r.ownerLocked()
	changed := !maps.Equal(facts, r.facts)
	r.facts = facts
	r.remote[r.member] = facts
	send := changed || now.Sub(r.sentAt) >= 2*time.Second || r.sentTo != owner
	if send {
		r.sentAt, r.sentTo = now, owner
	}
	// Kernel states: the owner's for RSTP ports, forwarding for every other
	// bridge port (stack tunnels, RSTP disabled).
	r.applyStatesLocked()
	// Ownership.
	if owner == r.member && r.br == nil {
		r.becomeOwnerLocked()
	} else if owner != r.member && r.br != nil {
		r.log.Info("rstp: member takes over the spanning tree", "owner", owner)
		r.br = nil
	}
	if owner != r.lastOwn && r.lastOwn != 0 && owner != r.member {
		r.log.Info("rstp: owner", "member", owner)
	}
	r.lastOwn = owner
	var tick, snap bool
	if r.br != nil {
		if changed {
			r.syncPortsLocked()
		}
		if now.Sub(r.lastTick) >= time.Second {
			r.lastTick = now
			tick = true
			r.br.Tick()
		}
		if r.snapChanged || now.Sub(r.lastSnap) >= time.Second {
			snap = true
			r.snapChanged = false
			r.lastSnap = now
		}
	}
	var snapMsg *rstpSnapMsg
	if snap {
		snapMsg = &rstpSnapMsg{Snap: r.br.Snapshot(), Numbers: maps.Clone(r.numbers), At: now}
		r.snap = snapMsg
		r.saveLocked(tick)
	}
	members := r.cfg.SwitchMembers
	r.mu.Unlock()

	if send {
		if owner == r.member {
			// (the owner uses its own facts directly)
		} else {
			r.enqueue(owner, func() {
				r.stack.Call(owner, "rstp-facts", rstpFactsMsg{Facts: facts}, time.Second)
			})
		}
	}
	r.dispatch()
	if snapMsg != nil && r.stack != nil {
		for _, id := range members {
			if id != r.member && slices.Contains(r.stack.Reachable(), id) {
				r.enqueue(id, func() { r.stack.Call(id, "rstp-snap", snapMsg, time.Second) })
			}
		}
	}
}

func (r *Controller) readFact(name, dev string) rstpFact {
	rd := func(f string) string {
		b, _ := hwio.ReadFile(filepath.Join("/sys/class/net", dev, f))
		return strings.TrimSpace(string(b))
	}
	f := rstpFact{Up: rd("operstate") == "up" || (rd("carrier") == "1" && rd("operstate") == "unknown")}
	p := r.cfg.Ports[name]
	if p.AE && p.LACP && r.Legs != nil {
		f.Up = f.Up && r.Legs()[name] // a held or negotiating leg carries nothing
	}
	if !f.Up {
		return f
	}
	if p.AE {
		// A bundle: the speed of its local members that are up.
		for _, l := range p.Legs {
			b, _ := hwio.ReadFile(filepath.Join("/sys/class/net", l, "carrier"))
			s, _ := hwio.ReadFile(filepath.Join("/sys/class/net", l, "speed"))
			if strings.TrimSpace(string(b)) == "1" {
				if v, err := strconv.Atoi(strings.TrimSpace(string(s))); err == nil && v > 0 {
					f.Mbps += v
				}
			}
		}
		f.FullDuplex = true
		return f
	}
	if v, err := strconv.Atoi(rd("speed")); err == nil && v > 0 {
		f.Mbps = v
	}
	f.FullDuplex = rd("duplex") != "half"
	return f
}

// applyStatesLocked puts every bridge port into the state it should have.
func (r *Controller) applyStatesLocked() {
	all, err := netdev.BridgePorts(names.Bridge)
	if err != nil {
		return
	}
	rstpDev := map[string]string{}
	for n, dev := range r.devs {
		rstpDev[dev] = n
	}
	for _, dev := range all {
		want := netdev.PortForwarding
		if n, ok := rstpDev[dev]; ok {
			w, known := r.desired[n]
			if !known {
				continue // not decided yet: the kernel keeps it blocking
			}
			want = w
		}
		if st, err := netdev.PortSTPState(dev); err == nil && st != want && st != netdev.PortDisabled {
			if err := netdev.SetPortSTPState(dev, want); err != nil {
				r.log.Debug("rstp: port state", "port", dev, "err", err)
			}
		}
	}
}

// command applies what the owner sent.
func (r *Controller) command(msg *rstpCmdMsg) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if msg.States != nil {
		for n, st := range msg.States {
			if r.desired[n] != st {
				r.log.Debug("rstp: port state", "port", n, "state", st)
			}
		}
		r.desired = msg.States
		r.applyStatesLocked()
	}
	for _, n := range msg.Flush {
		if dev, ok := r.devs[n]; ok {
			netdev.FlushLearned(dev)
		}
	}
	for _, tx := range msg.Tx {
		r.sendLocked(tx.Port, tx.BPDU)
	}
}

// received handles a BPDU from a member's port (on the owner).
func (r *Controller) received(port string, raw []byte) {
	r.mu.Lock()
	if r.br == nil {
		r.mu.Unlock()
		return
	}
	num, ok := r.numbers[port]
	if !ok {
		r.mu.Unlock()
		return
	}
	b, err := rstp.Unmarshal(raw)
	if err != nil {
		r.mu.Unlock()
		return
	}
	r.br.Receive(num, b)
	r.mu.Unlock()
	r.dispatch()
}

// ---- owner side ----

func (r *Controller) becomeOwnerLocked() {
	cfg := r.bridgeConfigLocked()
	r.pending = map[int]*rstpCmdMsg{}
	r.states = map[string]int{}
	r.br = rstp.New(cfg, rstp.Callbacks{Send: r.onSend, State: r.onState, Flush: r.onFlush})
	ports := r.portsLocked()
	r.numbers = rstpNumbers(slices.Collect(maps.Keys(ports)))
	r.byNum = map[uint16]string{}
	for n, v := range r.numbers {
		r.byNum[v] = n
	}
	if s := r.snap; s != nil && time.Since(s.At) < rstpSnapFresh && maps.Equal(s.Numbers, r.numbers) {
		cfgs := map[uint16]rstp.PortConfig{}
		for n, p := range ports {
			cfgs[r.numbers[n]] = r.portConfigLocked(p)
		}
		r.br.Restore(s.Snap, cfgs)
		r.log.Info("rstp: this member runs the spanning tree, continuing from the last copy", "age", time.Since(s.At).Round(time.Millisecond))
	} else {
		r.log.Info("rstp: this member runs the spanning tree (starting)")
	}
	r.syncPortsLocked()
}

func (r *Controller) portConfigLocked(p rstpPort) rstp.PortConfig {
	pc := rstp.PortConfig{Priority: 128, AutoEdge: true}
	fact := r.factLocked(p)
	pc.SpeedMbps = fact.Mbps
	pc.P2P = fact.FullDuplex
	if c := p.cfg; c != nil {
		pc.Priority = uint16(c.Priority)
		pc.Cost = uint32(c.Cost)
		pc.AdminEdge = c.Edge
		pc.RootGuard = c.RootGuard
		if c.PointToPnt != nil {
			pc.P2P = *c.PointToPnt
		}
	}
	return pc
}

// factLocked combines the members' facts about a port (a bundle is up if
// any leg is, with the speed of all).
func (r *Controller) factLocked(p rstpPort) rstpFact {
	var out rstpFact
	for _, m := range p.members {
		f, ok := r.remote[m][p.name]
		if !ok {
			continue
		}
		if f.Up {
			out.Up = true
			out.Mbps += f.Mbps
			out.FullDuplex = out.FullDuplex || f.FullDuplex
		}
		out.Flaps += f.Flaps
	}
	return out
}

// syncPortsLocked makes the bridge's ports match the configuration and the
// members' facts.
func (r *Controller) syncPortsLocked() {
	ports := r.portsLocked()
	nums := rstpNumbers(slices.Collect(maps.Keys(ports)))
	for n, v := range r.numbers {
		if nums[n] != v {
			r.br.RemovePort(v)
			delete(r.states, n)
		}
	}
	r.numbers = nums
	r.byNum = map[uint16]string{}
	for n, v := range nums {
		r.byNum[v] = n
	}
	for n, p := range ports {
		num := nums[n]
		pc := r.portConfigLocked(p)
		f := r.factLocked(p)
		r.br.AddPort(num, pc) // or updates the configuration
		if f.Flaps != r.flaps[n] {
			r.flaps[n] = f.Flaps
			r.br.SetEnabled(num, false)
		}
		r.br.SetEnabled(num, f.Up)
	}
	r.snapChanged = true
}

func (r *Controller) cmdFor(member int) *rstpCmdMsg {
	c := r.pending[member]
	if c == nil {
		c = &rstpCmdMsg{}
		r.pending[member] = c
	}
	return c
}

// onSend: the BPDU leaves on the port's member (a bundle: the lowest member
// whose leg is up).
func (r *Controller) onSend(num uint16, b *rstp.BPDU) {
	n := r.byNum[num]
	ports := r.portsLocked()
	p, ok := ports[n]
	if !ok {
		return
	}
	for _, m := range p.members {
		if r.remote[m][n].Up {
			c := r.cmdFor(m)
			c.Tx = append(c.Tx, rstpTx{Port: n, BPDU: b.Marshal()})
			return
		}
	}
}

func (r *Controller) onState(num uint16, learning, forwarding bool) {
	n := r.byNum[num]
	st := netdev.PortBlocking
	switch {
	case forwarding:
		st = netdev.PortForwarding
	case learning:
		st = netdev.PortLearning
	}
	if r.states[n] == st {
		return
	}
	r.states[n] = st
	r.snapChanged = true
	if p, ok := r.portsLocked()[n]; ok {
		for _, m := range p.members {
			c := r.cmdFor(m)
			c.States = map[string]int{} // filled in dispatch
		}
	}
}

func (r *Controller) onFlush(num uint16) {
	n := r.byNum[num]
	if p, ok := r.portsLocked()[n]; ok {
		for _, m := range p.members {
			c := r.cmdFor(m)
			c.Flush = append(c.Flush, n)
		}
	}
}

// dispatch sends the owner's pending outputs to the members.
func (r *Controller) dispatch() {
	r.mu.Lock()
	if r.br == nil || len(r.pending) == 0 {
		r.pending = map[int]*rstpCmdMsg{}
		r.mu.Unlock()
		return
	}
	ports := r.portsLocked()
	out := r.pending
	r.pending = map[int]*rstpCmdMsg{}
	for m, c := range out {
		if c.States != nil {
			for n, p := range ports {
				if st, ok := r.states[n]; ok && slices.Contains(p.members, m) {
					c.States[n] = st
				}
			}
		}
	}
	if c := out[r.member]; c != nil {
		delete(out, r.member)
		r.mu.Unlock()
		r.command(c)
	} else {
		r.mu.Unlock()
	}
	for m, c := range out {
		if r.stack == nil {
			continue
		}
		r.enqueue(m, func() {
			if _, err := r.stack.Call(m, "rstp-cmd", c, time.Second); err != nil {
				r.log.Debug("rstp: to member", "member", m, "err", err)
			}
		})
	}
}

// enqueue runs f in order with the other messages to member m.
func (r *Controller) enqueue(m int, f func()) {
	if r.stack == nil {
		return
	}
	r.mu.Lock()
	q := r.queues[m]
	if q == nil {
		q = make(chan func(), 1024)
		r.queues[m] = q
		go func() {
			for f := range q {
				f()
			}
		}()
	}
	r.mu.Unlock()
	select {
	case q <- f:
	default:
		r.log.Warn("rstp: queue to member full", "member", m)
	}
}

func (r *Controller) saveLocked(force bool) {
	if r.snap == nil || (!force && time.Since(r.savedAt) < time.Second) {
		return
	}
	raw, err := json.Marshal(r.snap)
	if err != nil {
		return
	}
	tmp := r.stateFile + ".tmp"
	if hwio.WriteFile(tmp, raw, 0o600) == nil && hwio.Rename(tmp, r.stateFile) == nil {
		r.savedAt = time.Now()
	}
}

// ---- BPDU sockets ----

func (r *Controller) openSockLocked(port, dev string) error {
	ifi, err := net.InterfaceByName(dev)
	if err != nil {
		return err
	}
	proto := htons(unix.ETH_P_802_2)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return err
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: proto, Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return err
	}
	tv := unix.NsecToTimeval(int64(500 * time.Millisecond))
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	s := &rstpSock{fd: fd, ifindex: ifi.Index, stop: make(chan struct{})}
	copy(s.mac[:], ifi.HardwareAddr)
	r.socks[port] = s
	go r.read(port, s)
	return nil
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func (r *Controller) closeSockLocked(port string) {
	if s := r.socks[port]; s != nil {
		close(s.stop)
		delete(r.socks, port)
	}
}

func (r *Controller) read(port string, s *rstpSock) {
	buf := make([]byte, 1600)
	for {
		select {
		case <-s.stop:
			unix.Close(s.fd)
			return
		default:
		}
		n, from, err := unix.Recvfrom(s.fd, buf, 0)
		if err != nil || n < 17 {
			continue
		}
		if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
			continue
		}
		b, err := rstp.ParseFrame(buf[:n])
		if err != nil || b == nil {
			continue
		}
		raw := b.Marshal()
		r.mu.Lock()
		owner := r.ownerLocked()
		r.mu.Unlock()
		if owner == r.member {
			r.received(port, raw)
		} else {
			r.enqueue(owner, func() { r.stack.Call(owner, "rstp-rx", rstpRxMsg{Port: port, BPDU: raw}, time.Second) })
		}
	}
}

func (r *Controller) sendLocked(port string, raw []byte) {
	s := r.socks[port]
	if s == nil {
		return
	}
	b, err := rstp.Unmarshal(raw)
	if err != nil {
		return
	}
	addr := [8]byte{}
	copy(addr[:], rstp.GroupMAC[:])
	if err := unix.Sendto(s.fd, b.Frame(s.mac), 0, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_802_2), Ifindex: s.ifindex, Halen: 6, Addr: addr}); err != nil {
		r.log.Debug("rstp: send", "port", port, "err", err)
	}
}

// ---- status ----

func (r *Controller) localStatus() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{Running: r.on, Owner: r.ownerLocked(), Error: r.lastErr}
	if r.br == nil {
		return st
	}
	st.Bridge = r.br.Config()
	root, rp, times := r.br.Root()
	st.Root, st.Times, st.Changes = root, times, r.br.TopologyChanges
	if rp != 0 {
		st.RootPort = r.byNum[rp.Number()]
	}
	for _, p := range r.br.Ports() {
		st.Ports = append(st.Ports, PortStatus{Name: r.byNum[p.Number], PortStatus: p})
	}
	return st
}

// status asks the owner.
func (r *Controller) Status() (Status, error) {
	r.mu.Lock()
	owner := r.ownerLocked()
	r.mu.Unlock()
	if owner == r.member || r.stack == nil {
		return r.localStatus(), nil
	}
	raw, err := r.stack.Call(owner, "rstp-status", nil, 2*time.Second)
	if err != nil {
		return Status{}, fmt.Errorf("RSTP owner (member %d): %w", owner, err)
	}
	var st Status
	return st, json.Unmarshal(raw, &st)
}

// Clear runs a clear command on the owner (the state machines are there).
func (r *Controller) Clear(c ClearRequest) error {
	r.mu.Lock()
	owner := r.ownerLocked()
	r.mu.Unlock()
	if owner == r.member || r.stack == nil {
		return r.clearLocal(c)
	}
	_, err := r.stack.Call(owner, "rstp-clear", c, 2*time.Second)
	return err
}

func (r *Controller) clearLocal(c ClearRequest) error {
	r.mu.Lock()
	if r.br == nil {
		r.mu.Unlock()
		return errors.New("RSTP is not running")
	}
	if !c.Migration {
		r.br.ClearStatistics()
		r.mu.Unlock()
		return nil
	}
	num := uint16(0)
	if c.Port != "" {
		n, ok := r.numbers[c.Port]
		if !ok {
			r.mu.Unlock()
			return fmt.Errorf("%s is not an RSTP port", c.Port)
		}
		num = n
	}
	r.br.Mcheck(num)
	r.mu.Unlock()
	r.dispatch() // the BPDUs it sends go out through the members
	return nil
}
