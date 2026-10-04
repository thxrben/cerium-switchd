package bgpd

import (
	"context"
	"net/netip"
	"slices"
	"time"

	"github.com/thxrben/cerium-switchd/internal/bfdd"
	"github.com/thxrben/cerium-switchd/pkg/bfd"
)

// BFD for BGP neighbours (reference 5.12): the master's cer-bfdd runs a
// session per neighbour with bfd-liveness-detection (client "bgp"); its
// state goes to the speaker (pkg/bgp SetBFD).

const bfdClient = "bgp"

// BFD is cer-bfdd on this member.
type BFD interface {
	Set(ctx context.Context, s bfdd.Set) error
}

type bfdRef struct {
	instance string
	nbr      netip.Addr
}

// syncBFD gives cer-bfdd the sessions of the running instances (d.mu
// held); only a changed list is sent.
func (d *Daemon) syncBFD(want map[string]Instance) {
	if d.BFD == nil {
		return
	}
	var specs []bfdd.SessionSpec
	keys := map[string]bfdRef{}
	for _, name := range sortedKeys(want) {
		if d.insts[name] == nil {
			continue
		}
		in := want[name]
		for _, n := range in.Neighbors {
			b := n.BFDCfg
			if b == nil || n.Disabled {
				continue
			}
			k := bfd.Key{Instance: in.VRF, Peer: n.Addr, Multihop: b.Multihop}
			if b.Multihop {
				k.Local = b.Local
			}
			specs = append(specs, bfdd.SessionSpec{Key: k, Interface: "bgp " + n.Addr.String(), IntervalMs: b.IntervalMs,
				Multiplier: b.Multiplier, AuthType: b.AuthType, AuthKeyID: b.AuthKeyID, AuthKey: b.AuthKey})
			keys[k.String()] = bfdRef{instance: in.Name, nbr: n.Addr}
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
func (d *Daemon) sendBFD(specs []bfdd.SessionSpec) {
	if d.bfdQ == nil {
		d.bfdQ = make(chan []bfdd.SessionSpec, 1)
		go func() {
			for ss := range d.bfdQ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := d.BFD.Set(ctx, bfdd.Set{Client: bfdClient, Sessions: ss}); err != nil {
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
	d.bfdMu.Unlock()
	if !ok || deleted {
		return
	}
	d.mu.Lock()
	in := d.insts[ref.instance]
	d.mu.Unlock()
	if in != nil {
		in.sp.SetBFD(ref.nbr, up)
	}
}
