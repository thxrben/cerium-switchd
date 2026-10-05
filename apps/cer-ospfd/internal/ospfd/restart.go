package ospfd

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"time"

	"github.com/thxrben/cerium-switchd/lib/ospf"
	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
)

// Graceful restart of cer-ospfd (reference 5.13, PLAN 9c): the fully
// adjacent neighbours of every instance are kept in a file in /run (gone
// after a reboot, when the kernel's routes are gone too). A restarted
// cer-ospfd with a fresh file restarts gracefully; a planned restart (the
// supervisor's mark) announces it to the neighbours before it stops.

// RestartFile is where the neighbours are kept.
const RestartFile = "/run/ceros/ospf-restart.json"

// restartAlive: the file's time is refreshed this often (the grace period
// counts from it).
const restartAlive = 10 * time.Second

type restartData struct {
	Alive     time.Time                       `json:"alive"`
	Planned   bool                            `json:"planned,omitempty"`
	Instances map[string]map[string][]ospf.ID `json:"instances"` // instance key -> interface -> neighbours
}

// loadRestart reads the file of the previous run (once, before the first
// instance starts).
func (d *Daemon) loadRestart() {
	if d.restartRead || d.RestartFile == "" {
		return
	}
	d.restartRead = true
	b, err := hwio.ReadFile(d.RestartFile)
	if err != nil {
		return
	}
	var rd restartData
	if json.Unmarshal(b, &rd) != nil {
		return
	}
	d.restartFrom = &rd
	// Until the instances relearned their neighbours, these stay the ones
	// kept (another crash meanwhile restarts gracefully as well).
	d.lastSaved = restartData{Alive: rd.Alive, Planned: rd.Planned, Instances: maps.Clone(rd.Instances)}
}

// startRestart puts a new instance into the restarting role when the
// previous run left its neighbours (StartRestart ignores a period that is
// over).
func (d *Daemon) startRestart(key string, in *instance) {
	d.loadRestart()
	rd := d.restartFrom
	if rd == nil || len(rd.Instances[key]) == 0 {
		return
	}
	reason := uint8(ospf.ReasonUnknown)
	if rd.Planned {
		reason = ospf.ReasonSoftware
	}
	in.r.StartRestart(rd.Instances[key], rd.Alive, reason)
	delete(rd.Instances, key) // once
}

// saveRestart keeps the neighbours (on a change, and every restartAlive).
func (d *Daemon) saveRestart(now time.Time, planned bool) {
	if d.RestartFile == "" {
		return
	}
	d.loadRestart() // (the previous run's neighbours are not overwritten unseen)
	insts := map[string]map[string][]ospf.ID{}
	for _, k := range d.sortedKeys() {
		in := d.insts[k]
		if !in.cfg.GracefulRestart {
			continue
		}
		if in.r.Restarting() {
			// Still relearning: the neighbours of before stay the truth.
			if prev := d.lastSaved.Instances[k]; prev != nil {
				insts[k] = prev
			}
			continue
		}
		if nb := in.r.AdjacentNeighbors(); len(nb) > 0 {
			insts[k] = nb
		}
	}
	same := len(insts) == len(d.lastSaved.Instances)
	for k, v := range insts {
		same = same && maps.EqualFunc(v, d.lastSaved.Instances[k], func(a, b []ospf.ID) bool { return slicesEqual(a, b) })
	}
	if same && !planned && now.Sub(d.lastSaved.Alive) < restartAlive {
		return
	}
	if len(insts) == 0 {
		if d.lastSaved.Instances != nil {
			_ = hwio.Remove(d.RestartFile)
		}
		d.lastSaved = restartData{Alive: now}
		return
	}
	rd := restartData{Alive: now, Planned: planned, Instances: insts}
	b, _ := json.Marshal(rd)
	_ = hwio.MkdirAll(filepath.Dir(d.RestartFile), 0o755)
	if err := hwio.WriteFileAtomic(d.RestartFile, b, 0o600); err != nil {
		d.Log.Warn("ospf: neighbours for a graceful restart not kept", "err", err)
		return
	}
	d.lastSaved = rd
}

func slicesEqual(a, b []ospf.ID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
