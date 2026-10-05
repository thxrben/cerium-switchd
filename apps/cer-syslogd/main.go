// Command cer-syslogd forwards the switch's logs (reference 5.1, 1.9): it
// reads the system journal (switchd, the cer- daemons, the kernel and the
// operating system's services), keeps the recent messages for show log
// and sends them to the configured syslog servers. On a member that is not
// the master it hands its messages to the master's cer-syslogd, which
// sends those of all members (reference 1.8).
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/lib/platform/daemonkit"
	"github.com/thxrben/cerium-switchd/lib/platform/ipc"
	"github.com/thxrben/cerium-switchd/lib/platform/svc"
	"github.com/thxrben/cerium-switchd/lib/sys/journal"
	"github.com/thxrben/cerium-switchd/lib/syslog"
)

func main() { daemonkit.Main("cer-syslogd", setup) }

// relayQueue bounds the messages waiting for the master (oldest dropped).
const relayQueue = 1000

type daemon struct {
	k   *daemonkit.Kit
	hub *syslog.Hub

	mu     sync.Mutex
	cfg    syslog.Config
	role   svc.Role
	relayQ chan syslog.Message
}

func setup(k *daemonkit.Kit) error {
	d := &daemon{k: k, hub: syslog.NewHub(slog.DiscardHandler, 5000), relayQ: make(chan syslog.Message, relayQueue)}
	k.OnConfig(func(raw json.RawMessage) {
		var c syslog.Config
		if err := json.Unmarshal(raw, &c); err != nil {
			k.Log.Error("configuration", "err", err)
			return
		}
		d.mu.Lock()
		d.cfg = c
		d.mu.Unlock()
		d.apply()
	})
	k.OnRole(func(r svc.Role) {
		d.mu.Lock()
		changed := r.Master != d.role.Master || r.MasterID != d.role.MasterID
		d.role = r
		d.mu.Unlock()
		if changed {
			d.apply()
		}
	})
	// Messages of the other members (on the master).
	k.HandleStack("log", func(_ context.Context, _ int, raw json.RawMessage) (any, error) {
		var msgs []syslog.Message
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, err
		}
		for _, m := range msgs {
			m.Severity = min(max(m.Severity, 0), 7) // a valid syslog PRI whatever the sender
			d.hub.Log(m)
		}
		return nil, nil
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return d.hub.Stats(), nil
	})
	k.Endpoint.Handle("recent", func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return d.hub.Recent(), nil
	})
	go d.relayLoop()
	go func() {
		cursor := filepath.Join(k.SocketDir, "cer-syslogd.cursor")
		if k.SocketDir == "" {
			cursor = filepath.Join(svc.SocketDir, "cer-syslogd.cursor")
		}
		err := journal.Follow(k.Ctx, cursor, 1000, func(e journal.Entry) {
			d.hub.Log(syslog.Message{Time: e.Time, Facility: e.Facility, Severity: e.Severity, Text: e.Message, App: e.App, PID: e.PID})
		})
		if err != nil {
			k.Log.Error("journal", "err", err)
		}
	}()
	// The queued messages (the shutdown's own among them) go out first.
	k.OnShutdown(func(ctx context.Context) {
		d.hub.Drain(ctx)
		d.hub.Close()
	})
	return nil
}

// apply converges the forwarders: the master sends, the others relay to it.
func (d *daemon) apply() {
	d.mu.Lock()
	c, r := d.cfg, d.role
	d.mu.Unlock()
	master := r.Master || r.MasterID == 0 || r.MasterID == r.Member
	hosts := c.Hosts
	if !master {
		hosts = nil
	}
	d.hub.SetVRF(c.VRF)
	d.hub.Configure(hosts, func() string { return c.HostName }, c.BufSize)
	if master {
		d.hub.SetRelay(nil)
	} else {
		d.hub.SetRelay(d.relay)
	}
}

// relay queues a local message for the master (never blocks).
func (d *daemon) relay(m syslog.Message) {
	d.mu.Lock()
	m.Host = d.cfg.HostName
	d.mu.Unlock()
	for {
		select {
		case d.relayQ <- m:
			return
		default:
			select {
			case <-d.relayQ: // drop the oldest
			default:
			}
		}
	}
}

// relayLoop sends queued messages to the master in batches.
func (d *daemon) relayLoop() {
	ctx := d.k.Ctx
	var batch []syslog.Message
	wait := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
			return true
		}
	}
	for {
		if len(batch) == 0 {
			select {
			case <-ctx.Done():
				return
			case m := <-d.relayQ:
				batch = append(batch, m)
			}
		}
	fill:
		for len(batch) < 100 {
			select {
			case m := <-d.relayQ:
				batch = append(batch, m)
			default:
				break fill
			}
		}
		d.mu.Lock()
		r := d.role
		d.mu.Unlock()
		switch {
		case r.MasterID == r.Member && r.Member != 0:
			batch = nil // this member became master: its own forwarders have them
			continue
		case r.MasterID == 0:
			if !wait() {
				return
			}
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := d.k.StackCall(cctx, r.MasterID, "log", batch, nil)
		cancel()
		if err != nil {
			if !wait() {
				return
			}
			if len(batch) > relayQueue {
				batch = batch[len(batch)-relayQueue:]
			}
			continue
		}
		batch = nil
	}
}
