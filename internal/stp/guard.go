package stp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// BPDU protection (protocols layer2-control bpdu-block, reference 5.5): a
// listed port that receives any BPDU (STP, RSTP, MSTP, or Cisco PVST+) is
// shut down at once, with or without RSTP. The guard only decides: it
// publishes the blocked interfaces (TopicBPDUBlocked) and switchd, which
// owns the ports, keeps them down until "clear error bpdu interface" or
// the disable-timeout. The blocked set survives restarts of cer-rstpd and
// of switchd.

// The destinations of BPDUs: IEEE STP/RSTP/MSTP and Cisco PVST+.
var (
	bpduDst  = []byte{0x01, 0x80, 0xc2, 0x00, 0x00, 0x00}
	pvstDst  = []byte{0x01, 0x00, 0x0c, 0xcc, 0xcc, 0xcd}
	guardMAC = [][]byte{bpduDst, pvstDst}
)

// IsBPDU reports whether a frame (from its destination MAC on) is a BPDU.
func IsBPDU(frame []byte) bool {
	if len(frame) < 6 {
		return false
	}
	for _, m := range guardMAC {
		if bytes.Equal(frame[:6], m) {
			return true
		}
	}
	return false
}

// GuardIO opens a port's BPDU listener (the Linux one in guard_linux.go).
type GuardIO interface {
	Listen(dev string, rx func(frame []byte)) (stop func(), err error)
}

// Guard watches the bpdu-block ports of this member.
type Guard struct {
	IO        GuardIO
	Log       *slog.Logger
	StateFile string
	// Publish replaces the topic; Alarm tells the operators.
	Publish func(map[string]Blocked)
	Alarm   func(text string)

	mu      sync.Mutex
	ports   map[string]string // interface -> device on this member
	timeout time.Duration
	stops   map[string]func()
	blocked map[string]Blocked
	now     func() time.Time
}

// NewGuard returns a guard with the blocked ports of the state file.
func NewGuard(io GuardIO, stateFile string, log *slog.Logger) *Guard {
	g := &Guard{IO: io, Log: log, StateFile: stateFile, ports: map[string]string{}, stops: map[string]func(){},
		blocked: map[string]Blocked{}, now: time.Now}
	if raw, err := hwio.ReadFile(stateFile); err == nil {
		json.Unmarshal(raw, &g.blocked)
	}
	return g
}

// SetConfig takes the bpdu-block ports (interface -> local device) and the
// disable-timeout (0: until cleared).
func (g *Guard) SetConfig(ports map[string]string, timeout time.Duration) {
	g.mu.Lock()
	g.ports, g.timeout = ports, timeout
	// No longer protected: no longer blocked.
	for n := range g.blocked {
		if _, ok := ports[n]; !ok {
			delete(g.blocked, n)
		}
	}
	for n, stop := range g.stops {
		if _, ok := ports[n]; !ok {
			stop()
			delete(g.stops, n)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(ports)) {
		if g.stops[n] != nil || ports[n] == "" {
			continue
		}
		name := n
		stop, err := g.IO.Listen(ports[n], func(frame []byte) { g.bpdu(name, frame) })
		if err != nil {
			g.Log.Error("bpdu-block: cannot watch the port", "interface", n, "err", err)
			continue
		}
		g.stops[n] = stop
	}
	g.saveLocked()
	g.mu.Unlock()
	g.publish()
}

func (g *Guard) bpdu(name string, frame []byte) {
	if !IsBPDU(frame) {
		return
	}
	g.mu.Lock()
	if _, ok := g.blocked[name]; ok {
		g.mu.Unlock()
		return
	}
	now := g.now()
	b := Blocked{Since: now}
	if len(frame) >= 12 {
		b.From = macString(frame[6:12])
	}
	if g.timeout > 0 {
		b.Until = now.Add(g.timeout)
	}
	g.blocked[name] = b
	g.saveLocked()
	g.mu.Unlock()
	g.Log.Error("bpdu-block: BPDU received, port shut down", "interface", name, "from", b.From)
	if g.Alarm != nil {
		until := "until 'clear error bpdu interface " + name + "'"
		if !b.Until.IsZero() {
			until = "until " + b.Until.Format("15:04:05")
		}
		g.Alarm("ALARM: " + name + " received a BPDU from " + b.From + " and is shut down (bpdu-block) " + until)
	}
	g.publish()
}

// Clear re-enables a blocked port ("": all); it returns how many.
func (g *Guard) Clear(name string) int {
	g.mu.Lock()
	n := 0
	for k := range g.blocked {
		if name == "" || k == name {
			delete(g.blocked, k)
			n++
		}
	}
	g.saveLocked()
	g.mu.Unlock()
	if n > 0 {
		g.publish()
	}
	return n
}

// Tick ends blocks whose disable-timeout passed.
func (g *Guard) Tick() {
	g.mu.Lock()
	now := g.now()
	var done []string
	for k, b := range g.blocked {
		if !b.Until.IsZero() && !now.Before(b.Until) {
			delete(g.blocked, k)
			done = append(done, k)
		}
	}
	if len(done) > 0 {
		g.saveLocked()
	}
	g.mu.Unlock()
	for _, k := range done {
		g.Log.Info("bpdu-block: disable-timeout passed, port enabled again", "interface", k)
	}
	if len(done) > 0 {
		g.publish()
	}
}

// Blocked returns the blocked ports.
func (g *Guard) Blocked() map[string]Blocked {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.blocked)
}

func (g *Guard) publish() {
	if g.Publish != nil {
		g.Publish(g.Blocked())
	}
}

func (g *Guard) saveLocked() {
	if g.StateFile == "" {
		return
	}
	raw, _ := json.Marshal(g.blocked)
	if err := hwio.WriteFileAtomic(g.StateFile, raw, 0o600); err != nil {
		g.Log.Warn("bpdu-block: state file", "err", err)
	}
}

func macString(b []byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, 17)
	for i, v := range b {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexd[v>>4], hexd[v&15])
	}
	return string(out)
}

// GuardStateFile is the guard's state in dir.
func GuardStateFile(dir string) string { return filepath.Join(dir, "bpdu-blocked.json") }
