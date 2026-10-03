package daemon

import (
	"log/slog"
	"sync"

	"github.com/thxrben/cerium-switchd/internal/access"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/pkg/ntp"
	"github.com/thxrben/cerium-switchd/pkg/syslog"
)

// mgmtCtl runs the management services where they belong (reference 1.8):
// the CLI SSH server on the master, and the configuration of cer-syslogd
// and cer-ntpd (reference 1.9), which forward logs and query NTP servers on
// the master and relay to it on the other members.
type mgmtCtl struct {
	member int
	log    *slog.Logger
	sshd   *access.SSH
	// ctl is the stack control (nil: standalone, always master).
	ctl *stackCtl
	// dryRun: services outside switchd are left alone.
	dryRun bool
	// publish hands a daemon its configuration (reference 1.9).
	publish func(daemon string, v any)

	mu  sync.Mutex
	cfg *model.Config
}

func (m *mgmtCtl) master() bool { return m.ctl == nil || m.ctl.node.IsMaster() }

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
	// cer-ntpd queries the servers on the master and takes the master's
	// time elsewhere (by its role).
	if m.publish != nil {
		var servers []ntp.Server
		for _, s := range cfg.System.NTPServers {
			servers = append(servers, ntp.Server{Host: s.Host, Prefer: s.Prefer})
		}
		m.publish("cer-ntpd", ntp.Config{Servers: servers, VRF: mi})
	}
	if err := m.sshd.Sync(cfg, master); err != nil {
		m.log.Error("ssh", "err", err)
	}
}
