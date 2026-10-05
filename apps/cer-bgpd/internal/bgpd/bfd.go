package bgpd

import (
	"context"
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/thxrben/cerium-switchd/lib/bfd"
	"github.com/thxrben/cerium-switchd/lib/platform/api/bfdapi"
)

// BFD for BGP neighbours (reference 5.12): a session per neighbour with
// bfd-liveness-detection (client "bgp") in the cer-bfdd of the member that
// reaches it: the master for its own interfaces, irb and MC-LAG; the owner
// of a routed port of another member (relay.go) for the neighbours there.
// The state goes to the speaker (pkg/bgp SetBFD); an owner reports it to
// the master in order, with a count of failures (a lost report is
// repaired by the next one).

const bfdClient = "bgp"

// BFD is cer-bfdd on this member.
type BFD interface {
	Set(ctx context.Context, s bfdapi.Set) error
}

type bfdRef struct {
	instance string
	nbr      netip.Addr
	relayed  bool // run for the master (on an owner)
}

// StackBFDState is an owner's report of a relayed neighbour's BFD state.
const StackBFDState = "bgp-bfd-state"

// RelayBFDState is that report.
type RelayBFDState struct {
	Instance string     `json:"instance"`
	Neighbor netip.Addr `json:"neighbor"`
	Up       bool       `json:"up"`
	Downs    uint64     `json:"downs"` // up -> down changes so far
}

// syncBFD gives cer-bfdd the sessions of the running instances (d.mu
// held); only a changed list is sent.
func (d *Daemon) syncBFD(want map[string]Instance) {
	if d.BFD == nil {
		return
	}
	var specs []bfdapi.SessionSpec
	keys := map[string]bfdRef{}
	// The master: the neighbours it reaches itself. An owner: those on its
	// routed ports.
	insts := map[string]Instance{}
	if d.master {
		for _, name := range slices.Sorted(maps.Keys(want)) {
			if d.insts[name] != nil {
				insts[name] = want[name]
			}
		}
	} else if d.masterID != 0 {
		for _, in := range d.cfg.Instances {
			insts[in.Name] = in
		}
	}
	for _, name := range slices.Sorted(maps.Keys(insts)) {
		in := insts[name]
		for _, n := range in.Neighbors {
			b := n.BFDCfg
			if b == nil || n.Disabled {
				continue
			}
			ownedElsewhere := n.Owner != 0 && n.Owner != d.Member
			if (d.master && ownedElsewhere) || (!d.master && n.Owner != d.Member) {
				continue // another member runs it
			}
			k := bfd.Key{Instance: in.VRF, Peer: n.Addr, Multihop: b.Multihop}
			if b.Multihop {
				k.Local = b.Local
			}
			specs = append(specs, bfdapi.SessionSpec{Key: k, IntervalMs: b.IntervalMs,
				Multiplier: b.Multiplier, AuthType: b.AuthType, AuthKeyID: b.AuthKeyID, AuthKey: b.AuthKey})
			keys[k.String()] = bfdRef{instance: in.Name, nbr: n.Addr, relayed: !d.master}
		}
	}
	d.bfdMu.Lock()
	defer d.bfdMu.Unlock()
	d.bfdKeys = keys
	if d.bfdSent && slices.Equal(specs, d.bfdSpecs) {
		return
	}
	d.bfdSpecs, d.bfdSent = specs, true
	d.sendBFD(specs)
}

// sendBFD hands a list to the sender (d.bfdMu held): the newest wins.
func (d *Daemon) sendBFD(specs []bfdapi.SessionSpec) {
	if d.bfdQ == nil {
		d.bfdQ = make(chan []bfdapi.SessionSpec, 1)
		go func() {
			for ss := range d.bfdQ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := d.BFD.Set(ctx, bfdapi.Set{Client: bfdClient, Sessions: ss}); err != nil {
					d.Log.Warn("bgp: BFD sessions not set", "err", err)
				}
				cancel()
			}
		}()
	}
	select {
	case <-d.bfdQ: // drop the older list not sent yet
	default:
	}
	d.bfdQ <- specs
}

// ResendBFD gives cer-bfdd the sessions again (it restarted).
func (d *Daemon) ResendBFD() {
	d.bfdMu.Lock()
	defer d.bfdMu.Unlock()
	if d.BFD != nil && d.bfdSent {
		d.sendBFD(d.bfdSpecs)
	}
}

// BFDChanged is a state on cer-bfdd's topic (key: the session key's
// string).
func (d *Daemon) BFDChanged(key string, up, deleted bool) {
	d.bfdMu.Lock()
	ref, ok := d.bfdKeys[key]
	if !ok || deleted {
		d.bfdMu.Unlock()
		return
	}
	if ref.relayed {
		if d.bfdUp == nil {
			d.bfdUp, d.bfdDowns = map[string]bool{}, map[string]uint64{}
		}
		if d.bfdUp[key] && !up {
			d.bfdDowns[key]++
		}
		d.bfdUp[key] = up
		st := RelayBFDState{Instance: ref.instance, Neighbor: ref.nbr, Up: up, Downs: d.bfdDowns[key]}
		d.bfdMu.Unlock()
		d.reportBFD(st)
		return
	}
	d.bfdMu.Unlock()
	d.mu.Lock()
	in := d.insts[ref.instance]
	d.mu.Unlock()
	if in != nil {
		in.sp.SetBFD(ref.nbr, up)
	}
}

// reportBFD sends an owner's state to the master, in order.
func (d *Daemon) reportBFD(st RelayBFDState) {
	d.mu.Lock()
	master := d.masterID
	d.mu.Unlock()
	if master == 0 || d.StackCall == nil {
		return
	}
	d.bfdMu.Lock()
	if d.bfdReports == nil {
		d.bfdReports = make(chan func(), 1024)
		go func() {
			for f := range d.bfdReports {
				f()
			}
		}()
	}
	q := d.bfdReports
	d.bfdMu.Unlock()
	q <- func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.StackCall(ctx, master, StackBFDState, st, nil); err != nil {
			d.Log.Debug("bgp: BFD state to the master", "err", err)
		}
	}
}

// ResendBFDStates reports every relayed session's state again (a new
// master).
func (d *Daemon) resendBFDStates() {
	d.bfdMu.Lock()
	var sts []RelayBFDState
	for k, ref := range d.bfdKeys {
		if up, ok := d.bfdUp[k]; ok && ref.relayed {
			sts = append(sts, RelayBFDState{Instance: ref.instance, Neighbor: ref.nbr, Up: up, Downs: d.bfdDowns[k]})
		}
	}
	d.bfdMu.Unlock()
	for _, st := range sts {
		d.reportBFD(st)
	}
}

// RelayedBFDState is StackBFDState on the master. The first report of a
// neighbour sets the count; a higher count later is a failure, even if
// the "up" before it was missed.
func (d *Daemon) RelayedBFDState(st RelayBFDState) {
	d.mu.Lock()
	in := d.insts[st.Instance]
	d.mu.Unlock()
	if in == nil {
		return
	}
	k := st.Instance + "|" + st.Neighbor.String()
	d.bfdMu.Lock()
	if d.bfdSeen == nil {
		d.bfdSeen = map[string]uint64{}
	}
	known, seen := d.bfdSeen[k]
	d.bfdSeen[k] = st.Downs
	d.bfdMu.Unlock()
	if seen && st.Downs > known && !st.Up {
		in.sp.SetBFD(st.Neighbor, true) // (it was up meanwhile)
	}
	in.sp.SetBFD(st.Neighbor, st.Up)
}
