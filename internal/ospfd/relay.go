package ospfd

import (
	"context"
	"net/netip"
	"slices"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/ospf"
)

// Routed interfaces that live on other members (reference 5.8): OSPF runs
// on the master, but a routed port of member 2, or a routed MC-LAG bundle
// with a leg on member 2, receives its packets on member 2. That member's
// cer-ospfd receives them on its own socket and hands them to the master
// over the stacking protocol (StackRx), unchanged; the master sends through
// its own device when it has one, otherwise through an owner (StackTx).
// The owner also reports the device's state (carrier, MTU, the OSPFv3
// link-local address) to the master (StackLink). irb interfaces are not
// relayed: their frames reach the master in their VLAN (the stack tunnel).

// Stacking-protocol methods of the relay.
const (
	StackRx   = "ospf-rx"
	StackTx   = "ospf-tx"
	StackLink = "ospf-link"
)

// RelayPacket is a packet relayed between an owner and the master.
type RelayPacket struct {
	Key  string     `json:"key"` // instance key (name/vN)
	Unit string     `json:"unit"`
	Src  netip.Addr `json:"src"`
	Dst  netip.Addr `json:"dst"`
	Pkt  []byte     `json:"pkt"`
}

// RelayLink is an owner's report of a relayed interface's device.
type RelayLink struct {
	Key    string   `json:"key"`
	Unit   string   `json:"unit"`
	Member int      `json:"member"`
	Info   LinkInfo `json:"info"`
	OK     bool     `json:"ok"`
}

// relayed reports whether interface ic is relayed and whether this member
// (me) relays it: a routed interface (not irb) that the master does not
// own, or that has legs on several members (MC-LAG).
func relayed(ic Iface, me, master int) (relayedUnit, here bool) {
	if ic.IRB || len(ic.Owners) == 0 {
		return false, false
	}
	need := !slices.Contains(ic.Owners, master) || len(ic.Owners) > 1
	return need, need && me != master && slices.Contains(ic.Owners, me) && ic.Device != ""
}

// relayState is a non-master's relay sockets and what it reported.
type relayState struct {
	ports    map[string]Port     // key+"|"+unit
	reported map[string]LinkInfo // last reported link info
	reportAt time.Time
}

// relay converges the relay sockets on a non-master member.
func (d *Daemon) relay(cfg Config, me, master int) {
	if d.rel.ports == nil {
		d.rel = relayState{ports: map[string]Port{}, reported: map[string]LinkInfo{}}
	}
	want := map[string]bool{}
	if master != 0 && me != master && d.StackCall != nil {
		for _, in := range cfg.Instances {
			for _, ic := range in.Interfaces {
				if _, here := relayed(ic, me, master); !here || ic.Passive {
					continue
				}
				k := in.key() + "|" + ic.Unit
				li, ok := d.Kernel.Link(ic.Device)
				up := ok && li.Up && li.Index > 0 && (in.Version == ospf.V2 || li.LinkLocal.IsValid())
				if !up {
					continue
				}
				want[k] = true
				if d.rel.ports[k] != nil {
					continue
				}
				key, unit := in.key(), ic.Unit
				p, err := d.Net.Open(in.Version, ic.Device, li.Index, func(src, dst netip.Addr, pkt []byte) {
					d.toMaster(StackRx, RelayPacket{Key: key, Unit: unit, Src: src, Dst: dst, Pkt: pkt})
				})
				if err != nil {
					d.Log.Warn("ospf relay: cannot open the interface", "unit", ic.Unit, "err", err)
					continue
				}
				d.rel.ports[k] = p
			}
		}
	}
	for k, p := range d.rel.ports {
		if !want[k] {
			p.Close()
			delete(d.rel.ports, k)
		}
	}
	d.reportLinks(cfg, me, master)
}

// reportLinks tells the master the state of the relayed devices (when it
// changed, and every 10 s).
func (d *Daemon) reportLinks(cfg Config, me, master int) {
	if master == 0 || me == master || d.StackCall == nil {
		return
	}
	now := time.Now()
	all := now.Sub(d.rel.reportAt) >= 10*time.Second
	if all {
		d.rel.reportAt = now
	}
	for _, in := range cfg.Instances {
		for _, ic := range in.Interfaces {
			if _, here := relayed(ic, me, master); !here {
				continue
			}
			k := in.key() + "|" + ic.Unit
			li, ok := d.Kernel.Link(ic.Device)
			if !all && ok && d.rel.reported[k] == li {
				continue
			}
			d.rel.reported[k] = li
			d.toMaster(StackLink, RelayLink{Key: in.key(), Unit: ic.Unit, Member: me, Info: li, OK: ok})
		}
	}
}

// toMaster sends a relay message without holding up the event loop.
func (d *Daemon) toMaster(method string, v any) {
	d.mu.Lock()
	master := d.masterID
	d.mu.Unlock()
	if master == 0 || d.StackCall == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := d.StackCall(ctx, master, method, v, nil); err != nil {
			d.Log.Debug("ospf relay to the master", "method", method, "err", err)
		}
	}()
}

// ReceiveRelayed is StackRx on the master: a packet an owner received.
func (d *Daemon) ReceiveRelayed(p RelayPacket) {
	d.do(func() {
		if in := d.insts[p.Key]; in != nil {
			in.r.Receive(p.Unit, p.Src, p.Dst, p.Pkt, time.Now())
		}
	})
}

// LinkReported is StackLink on the master: an owner's device state.
func (d *Daemon) LinkReported(l RelayLink) {
	d.do(func() {
		k := l.Key + "|" + l.Unit
		if d.remote == nil {
			d.remote = map[string]map[int]RelayLink{}
		}
		if d.remote[k] == nil {
			d.remote[k] = map[int]RelayLink{}
		}
		old := d.remote[k][l.Member]
		d.remote[k][l.Member] = l
		if old != l {
			if in := d.insts[l.Key]; in != nil {
				in.configure(time.Now())
			}
		}
	})
}

// SendRelayed is StackTx on an owner: send a packet for the master.
func (d *Daemon) SendRelayed(p RelayPacket) {
	d.do(func() {
		if port := d.rel.ports[p.Key+"|"+p.Unit]; port != nil {
			if err := port.Send(p.Src, p.Dst, p.Pkt); err != nil {
				d.Log.Debug("ospf relay: send", "unit", p.Unit, "err", err)
			}
		}
	})
}

// remoteLink is the master's view of a relayed interface's device: the
// first owner (lowest id) that reports it up.
func (d *Daemon) remoteLink(key string, ic Iface) (LinkInfo, int, bool) {
	reps := d.remote[key+"|"+ic.Unit]
	for _, m := range slices.Sorted(func(yield func(int) bool) {
		for m := range reps {
			if !yield(m) {
				return
			}
		}
	}) {
		r := reps[m]
		if r.OK && r.Info.Up && slices.Contains(ic.Owners, m) {
			return r.Info, m, true
		}
	}
	return LinkInfo{}, 0, false
}

// relayPort sends the master's packets through an owner.
type relayPort struct {
	d      *Daemon
	key    string
	unit   string
	member int
}

func (p *relayPort) Send(src, dst netip.Addr, pkt []byte) error {
	d := p.d
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := d.StackCall(ctx, p.member, StackTx, RelayPacket{Key: p.key, Unit: p.unit, Src: src, Dst: dst, Pkt: pkt}, nil); err != nil {
			d.Log.Debug("ospf relay to an owner", "member", p.member, "err", err)
		}
	}()
	return nil
}

func (p *relayPort) Close() error { return nil }
