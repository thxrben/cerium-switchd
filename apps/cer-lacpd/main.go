// Command cer-lacpd runs LACP for the switch's bundles (reference 5.3.2,
// 1.9): it decides which member ports of each team carry traffic, the only
// thing it changes in the kernel. A restart is hitless: the ports keep
// their state and the negotiated state is restored from the state file.
//
// MC-LAG (switchd or cer-mclagd, named in the configuration) holds legs
// out of their bundles through a topic, and is called before a leg carries
// traffic and before a held leg's last port leaves.
package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/lib/lacp"
	"github.com/thxrben/cerium-switchd/lib/platform/daemonkit"
	"github.com/thxrben/cerium-switchd/lib/platform/ipc"
	"github.com/thxrben/cerium-switchd/lib/platform/svc"
	"github.com/thxrben/cerium-switchd/lib/sys/netdev"
)

func main() { daemonkit.Main("cer-lacpd", setup) }

// hookTimeout bounds a call to MC-LAG before a leg joins (its peer filters
// first, reference 5.6) or leaves.
const hookTimeout = 300 * time.Millisecond

type teamKernel struct{}

func (teamKernel) SetPort(bundle, port string, on bool) error {
	return netdev.SetTeamPort(bundle, port, on)
}
func (teamKernel) PortsEnabled(bundle string) (map[string]bool, error) {
	return netdev.TeamPortsEnabled(bundle)
}

type daemon struct {
	k  *daemonkit.Kit
	rt *lacp.Runtime

	mu      sync.Mutex
	hooks   string // program serving the MC-LAG side
	hookCli *ipc.Client
	control map[string]svc.LACPControl
	synced  chan struct{} // closed when the control topic arrived (or timed out)
	started bool
	changed chan struct{}
}

func setup(k *daemonkit.Kit) error {
	d := &daemon{k: k, control: map[string]svc.LACPControl{}, synced: make(chan struct{}), changed: make(chan struct{}, 1)}
	d.rt = &lacp.Runtime{Kernel: teamKernel{}, StateFile: filepath.Join(k.StateDir, "lacp.json"), Log: k.Log,
		BeforeJoin:  func(b string) { d.hook(svc.MethodBeforeJoin, b) },
		BeforeLeave: func(b string) { d.hook(svc.MethodBeforeLeave, b) },
		OnChange: func() {
			select {
			case d.changed <- struct{}{}:
			default:
			}
		}}
	k.OnConfig(func(raw json.RawMessage) {
		var c svc.LACPConfig
		if err := json.Unmarshal(raw, &c); err != nil {
			k.Log.Error("configuration", "err", err)
			return
		}
		d.setHooks(c.Hooks)
		d.rt.Sync(c.Bundles)
		d.start()
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return d.rt.Status(), nil
	})
	go d.publish()
	return nil
}

// setHooks follows the MC-LAG program's control topic.
func (d *daemon) setHooks(name string) {
	if name == "" {
		name = svc.Switchd
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if name == d.hooks {
		return
	}
	d.hooks = name
	d.hookCli = nil
	sub := func(ev ipc.Event) { d.controlEvent(ev) }
	if name == svc.Switchd {
		d.k.Subscribe(svc.TopicLACPControl, "", sub)
		return
	}
	dir := d.k.SocketDir
	c := d.k.Endpoint.Dial(d.k.Ctx, svc.Socket(dir, name))
	c.Subscribe(svc.TopicLACPControl, "", sub)
	d.hookCli = c
}

func (d *daemon) controlEvent(ev ipc.Event) {
	if ev.Sync {
		d.mu.Lock()
		select {
		case <-d.synced:
		default:
			close(d.synced)
		}
		d.mu.Unlock()
		return
	}
	var c svc.LACPControl
	if !ev.Deleted {
		json.Unmarshal(ev.Value, &c)
	}
	d.mu.Lock()
	old := d.control[ev.Key]
	d.control[ev.Key] = c
	d.mu.Unlock()
	if old.Hold != c.Hold {
		d.rt.SetHold(ev.Key, c.Hold)
	}
	if old.PeerReady != c.PeerReady {
		d.rt.SetPeerReady(ev.Key, c.PeerReady)
	}
}

// start runs the state machines once the configuration is there and the
// holds are known (a held leg must not rejoin while they are unknown), or
// after 5 s without an answer from the MC-LAG program.
func (d *daemon) start() {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return
	}
	d.started = true
	d.mu.Unlock()
	go func() {
		select {
		case <-d.synced:
		case <-time.After(5 * time.Second):
			d.k.Log.Warn("MC-LAG holds unknown; LACP runs without them")
		case <-d.k.Ctx.Done():
			return
		}
		d.rt.Run(d.k.Ctx)
	}()
}

// hook calls the MC-LAG program (bounded: a leg joins anyway when it does
// not answer, as with a peer of an earlier version).
func (d *daemon) hook(method, bundle string) {
	d.mu.Lock()
	cli := d.hookCli
	d.mu.Unlock()
	if cli == nil {
		cli = d.k.Switchd
	}
	ctx, cancel := context.WithTimeout(d.k.Ctx, hookTimeout)
	defer cancel()
	if err := cli.Call(ctx, method, bundle, nil); err != nil {
		d.k.Log.Debug("MC-LAG did not answer", "call", method, "bundle", bundle, "err", err)
	}
}

// publish reports the legs, ready ports and port states: at once when a
// port changed, and every 100 ms (ready counts change without one; the
// topics send only what differs).
func (d *daemon) publish() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-d.k.Ctx.Done():
			return
		case <-d.changed:
		case <-t.C:
		}
		legs := map[string]any{}
		for b, v := range d.rt.Legs() {
			legs[b] = v
		}
		ready := map[string]any{}
		for b, v := range d.rt.Ready() {
			ready[b] = v
		}
		d.k.Endpoint.Replace(svc.TopicLACPLegs, legs)
		d.k.Endpoint.Replace(svc.TopicLACPReady, ready)
		d.k.Endpoint.Publish(svc.TopicLACPPorts, "", d.rt.EnabledPorts())
	}
}
