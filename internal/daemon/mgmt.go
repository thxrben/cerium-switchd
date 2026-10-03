package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/access"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/pkg/ntp"
	"github.com/thxrben/cerium-switchd/pkg/syslog"
)

// mgmtCtl runs the management services where they belong (reference 1.8):
// on the master the CLI SSH server, NTP and syslog forwarding through the
// management instance; on the other members none of them. Those hand
// their log messages to the master and take the time from it, over the
// stacking protocol.
type mgmtCtl struct {
	member int
	log    *slog.Logger
	ntp    *ntp.Client
	sshd   *access.SSH
	clock  ntp.Clock
	// ctl is the stack control (nil: standalone, always master).
	ctl *stackCtl
	// dryRun: services outside switchd are left alone.
	dryRun bool
	// publish hands a daemon its configuration (reference 1.9).
	publish func(daemon string, v any)

	mu  sync.Mutex
	cfg *model.Config
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
	if m.ctl == nil {
		return
	}
	m.ctl.node.Handle("time", func(int, json.RawMessage) (any, error) {
		return time.Now().UnixNano(), nil
	})
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
	// cer-syslogd sends on the master and relays to it elsewhere (by its
	// role); it gets the whole configuration on every member.
	if m.publish != nil {
		m.publish("cer-syslogd", syslog.Config{Hosts: syslogHosts(cfg), BufSize: cfg.System.LogBuffer, VRF: mi,
			HostName: cfg.MemberHostName(m.member)})
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
