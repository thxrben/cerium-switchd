package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/access"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/ntp"
	"github.com/thxrben/cerium-switchd/internal/syslog"
)

// mgmtCtl runs the management services where they belong (reference 1.8):
// on the master the CLI SSH server, NTP and syslog forwarding through the
// management instance; on the other members none of them. Those hand
// their log messages to the master and take the time from it, over the
// stacking protocol.
type mgmtCtl struct {
	member int
	log    *slog.Logger
	hub    *syslog.Hub
	ntp    *ntp.Client
	sshd   *access.SSH
	clock  ntp.Clock
	// ctl is the stack control (nil: standalone, always master).
	ctl *stackCtl
	// dryRun: services outside switchd are left alone.
	dryRun bool

	mu     sync.Mutex
	cfg    *model.Config
	relayQ chan syslog.Message
}

// Relayed messages wait here while the master cannot be reached; the
// oldest are dropped first.
const relayQueue = 1000

// timeInterval is how often a member takes the time from the master.
const timeInterval = 16 * time.Second

func (m *mgmtCtl) master() bool { return m.ctl == nil || m.ctl.node.IsMaster() }

// start registers the stacking protocol handlers and runs the relay and
// time loops until ctx is done.
func (m *mgmtCtl) start(ctx context.Context) {
	m.relayQ = make(chan syslog.Message, relayQueue)
	if m.ctl == nil {
		return
	}
	m.ctl.node.Handle("log", func(_ int, req json.RawMessage) (any, error) {
		var msgs []syslog.Message
		if err := json.Unmarshal(req, &msgs); err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			msg.Severity = min(max(msg.Severity, 0), 7) // a valid syslog PRI whatever the sender
			m.hub.Log(msg)
		}
		return nil, nil
	})
	m.ctl.node.Handle("time", func(int, json.RawMessage) (any, error) {
		return time.Now().UnixNano(), nil
	})
	go m.relayLoop(ctx)
	go m.timeLoop(ctx)
}

// sync converges the services for cfg (nil: the last one) and the current
// role; it runs after every commit and whenever mastership changes.
func (m *mgmtCtl) sync(cfg *model.Config) {
	m.mu.Lock()
	if cfg != nil {
		m.cfg = cfg
	}
	cfg = m.cfg
	m.mu.Unlock()
	if cfg == nil {
		return
	}
	master := m.master()
	mi := cfg.System.MgmtInstance
	hosts := syslogHosts(cfg)
	if !master {
		hosts = nil
	}
	m.hub.SetVRF(mi)
	m.hub.Configure(hosts, nil, cfg.System.LogBuffer)
	if master || m.ctl == nil {
		m.hub.SetRelay(nil)
	} else {
		m.hub.SetRelay(m.relay)
	}
	if m.dryRun {
		return
	}
	m.ntp.SetVRF(mi)
	var servers []ntp.Server
	if master {
		for _, s := range cfg.System.NTPServers {
			servers = append(servers, ntp.Server{Host: s.Host, Prefer: s.Prefer})
		}
	}
	m.ntp.Configure(servers)
	if err := m.sshd.Sync(cfg, master); err != nil {
		m.log.Error("ssh", "err", err)
	}
}

// relay queues a local message for the master (never blocks).
func (m *mgmtCtl) relay(msg syslog.Message) {
	msg.Host = m.hostName()
	for {
		select {
		case m.relayQ <- msg:
			return
		default:
			select {
			case <-m.relayQ: // drop the oldest
			default:
			}
		}
	}
}

func (m *mgmtCtl) hostName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg == nil {
		return ""
	}
	return m.cfg.MemberHostName(m.member)
}

// relayLoop sends queued messages to the master in batches.
func (m *mgmtCtl) relayLoop(ctx context.Context) {
	var batch []syslog.Message
	for {
		if len(batch) == 0 {
			select {
			case <-ctx.Done():
				return
			case msg := <-m.relayQ:
				batch = append(batch, msg)
			}
		}
	fill:
		for len(batch) < 100 {
			select {
			case msg := <-m.relayQ:
				batch = append(batch, msg)
			default:
				break fill
			}
		}
		to := m.ctl.node.Master()
		if to == 0 || to == m.member {
			if to == m.member {
				batch = nil // this member became master: its own forwarders have them
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if _, err := m.ctl.node.Call(to, "log", batch, 5*time.Second); err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			if len(batch) > relayQueue {
				batch = batch[len(batch)-relayQueue:]
			}
			continue
		}
		batch = nil
	}
}

// timeLoop sets this member's clock to the master's (reference 1.8):
// offset = master time + half the round trip - local time; stepped above
// ntp.StepLimit, slewed below.
func (m *mgmtCtl) timeLoop(ctx context.Context) {
	t := time.NewTicker(timeInterval)
	defer t.Stop()
	first := time.After(2 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-first:
		case <-t.C:
		}
		to := m.ctl.node.Master()
		if m.dryRun || to == 0 || to == m.member {
			continue
		}
		t1 := time.Now()
		raw, err := m.ctl.node.Call(to, "time", nil, 2*time.Second)
		t4 := time.Now()
		var ns int64
		if err != nil || json.Unmarshal(raw, &ns) != nil {
			continue
		}
		rtt := t4.Sub(t1)
		if rtt > time.Second {
			continue // too slow to be useful
		}
		off := time.Unix(0, ns).Add(rtt / 2).Sub(t4)
		switch {
		case off > ntp.StepLimit || off < -ntp.StepLimit:
			if err := m.clock.Step(off); err == nil {
				m.log.Warn("clock stepped to the master's time", "offset", off.String(), "master", to)
			}
		case off > time.Millisecond || off < -time.Millisecond:
			_ = m.clock.Slew(off)
		}
	}
}
