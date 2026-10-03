package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thxrben/cerium-switchd/internal/mclag"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/stack/control"
	"github.com/thxrben/cerium-switchd/internal/stack/mesh"
)

// maintDrainWait bounds how long entering maintenance mode waits for the
// member to be drained.
const maintDrainWait = 30 * time.Second

// maintCtl runs maintenance mode (reference 5.2): the member announces it
// in its stack topology announcements (no transit, never master), hands
// mastership on and holds its MC-LAG legs out of their bundles.
type maintCtl struct {
	file  string // present: in maintenance mode (survives a reboot)
	mesh  *mesh.Mesh
	node  *control.Node // nil: no stack control
	mclag mclagAPI
	cfg   func() *model.Config
	self  int
	log   *slog.Logger

	mu sync.Mutex
	on bool
}

func newMaint(stateDir string, self int, m *mesh.Mesh, node *control.Node, mc mclagAPI, cfg func() *model.Config, log *slog.Logger) *maintCtl {
	x := &maintCtl{file: filepath.Join(stateDir, "maintenance"), mesh: m, node: node, mclag: mc, cfg: cfg, self: self, log: log}
	if node != nil {
		node.Handle("drain", func(from int, req json.RawMessage) (any, error) {
			var why string
			json.Unmarshal(req, &why)
			x.drainForShutdown(why)
			return nil, nil
		})
	}
	if _, err := os.Stat(x.file); err == nil {
		log.Warn("maintenance mode: this member stays drained until 'request system maintenance-mode exit'")
		x.set(true)
	}
	return x
}

func (x *maintCtl) set(on bool) {
	x.mu.Lock()
	x.on = on
	x.mu.Unlock()
	if x.mesh != nil {
		x.mesh.SetDraining(on)
	}
	if x.mclag != nil {
		x.mclag.SetMaintenance(on, time.Now())
	}
}

func (x *maintCtl) active() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.on
}

// enter drains this member. force skips the checks; persist keeps the mode
// over a reboot (false: the member is about to go away anyway).
func (x *maintCtl) enter(force, persist bool, user string) (string, error) {
	if !force {
		var blockers []string
		if x.mclag != nil {
			blockers = x.mclag.DrainBlockers(x.draining())
		}
		if len(blockers) > 0 {
			return "", errors.New(strings.Join(blockers, "; ") + " ('force' enters anyway)")
		}
	}
	if persist {
		if err := os.WriteFile(x.file, []byte(time.Now().UTC().Format(time.RFC3339)+" "+user+"\n"), 0o644); err != nil {
			return "", err
		}
		x.log.Warn("maintenance mode entered", "facility", "change-log", "user", user, "force", force)
	}
	x.set(true)
	var out strings.Builder
	if x.node != nil && x.node.IsMaster() {
		if err := x.node.Transfer(0); err != nil {
			fmt.Fprintf(&out, "mastership stays here: %v\n", err)
		} else {
			out.WriteString("mastership handed on\n")
		}
	}
	deadline := time.Now().Add(maintDrainWait)
	var up []string
	for {
		up = nil
		if x.mclag != nil {
			up = x.mclag.LegsUp()
		}
		if len(up) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	drained := len(up) == 0
	if !drained {
		fmt.Fprintf(&out, "still carrying traffic: MC-LAG legs %s\n", strings.Join(up, ", "))
	}
	if t := x.transit(); len(t) > 0 {
		fmt.Fprintf(&out, "still carries stack transit (no other path): %s\n", strings.Join(t, ", "))
		drained = false
	}
	if s := x.singleHomed(); len(s) > 0 {
		fmt.Fprintf(&out, "not drained (only this member serves them): %s\n", strings.Join(s, ", "))
	}
	if drained {
		out.WriteString("drained\n")
	}
	return out.String(), nil
}

func (x *maintCtl) exit(user string) (string, error) {
	if !x.active() {
		return "", errors.New("this member is not in maintenance mode")
	}
	if err := os.Remove(x.file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	x.set(false)
	x.log.Warn("maintenance mode exited", "facility", "change-log", "user", user)
	return fmt.Sprintf("maintenance mode exited; MC-LAG legs rejoin in %s\n", mclag.RejoinAfter), nil
}

// drainForShutdown drains before a reboot, halt or power-off (best effort).
func (x *maintCtl) drainForShutdown(why string) {
	if x == nil || x.active() {
		return
	}
	x.log.Info("draining before " + why)
	text, err := x.enter(true, false, "")
	if err != nil {
		x.log.Warn("drain before "+why, "err", err)
		return
	}
	x.log.Info("drain before " + why + ": " + strings.TrimSpace(strings.ReplaceAll(text, "\n", "; ")))
}

func (x *maintCtl) draining() []int {
	if x.mesh == nil {
		return nil
	}
	return x.mesh.Draining()
}

// transit lists members that reach each other only through this one.
func (x *maintCtl) transit() []string {
	if x.mesh == nil {
		return nil
	}
	return cutPairs(x.mesh.Topology(), x.self, x.mesh.Reachable())
}

// cutPairs returns "a-b" for reachable members that are connected only
// through self (edges count when both ends announce them).
func cutPairs(topo map[int][]int, self int, reach []int) []string {
	adj := func(a int) []int {
		var out []int
		for _, b := range topo[a] {
			if b != self && slices.Contains(topo[b], a) {
				out = append(out, b)
			}
		}
		return out
	}
	comp := map[int]int{}
	for _, start := range reach {
		if _, ok := comp[start]; ok {
			continue
		}
		comp[start] = start
		q := []int{start}
		for len(q) > 0 {
			a := q[0]
			q = q[1:]
			for _, b := range adj(a) {
				if _, ok := comp[b]; !ok && slices.Contains(reach, b) {
					comp[b] = start
					q = append(q, b)
				}
			}
		}
	}
	var out []string
	for i, a := range reach {
		for _, b := range reach[i+1:] {
			if comp[a] != comp[b] {
				out = append(out, fmt.Sprintf("%d-%d", a, b))
			}
		}
	}
	return out
}

// singleHomed lists what only this member forwards: its switching and
// routed ports outside MC-LAG bundles, and bundles without mclag.
func (x *maintCtl) singleHomed() []string {
	cfg := x.cfg()
	if cfg == nil {
		return nil
	}
	var out []string
	for n, i := range cfg.Interfaces {
		switch {
		case i.Disabled:
		case i.AE && !i.MCLAG && slices.Contains(i.MemberIDs, x.self) && (i.Switching || cfg.L3[n+".0"] != nil):
			out = append(out, n)
		case !i.AE && i.Member == x.self && i.Parent == "" && i.Switching:
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// systemStopping: the machine is shutting down (not just switchd).
func systemStopping() bool {
	out, _ := exec.Command("systemctl", "is-system-running").Output()
	return strings.TrimSpace(string(out)) == "stopping"
}

// mclagAPI is what maintenance mode needs from MC-LAG (cer-mclagd).
type mclagAPI interface {
	SetMaintenance(on bool, now time.Time)
	DrainBlockers(draining []int) []string
	LegsUp() []string
}
