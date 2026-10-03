package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/thxrben/cerium-switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ntp"
	"github.com/thxrben/cerium-switchd/pkg/syslog"
)

var severityName = []string{"emergency", "alert", "critical", "error", "warning", "notice", "info", "debug"}

// logs gives the CLI the log buffer and the forwarders of cer-syslogd.
type logs struct{ svc *service }

func (l logs) call(method string, resp any) error {
	if l.svc == nil {
		return errors.New("the log buffer is not available (no cer-syslogd in a dry run)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return l.svc.call(ctx, "cer-syslogd", method, nil, resp)
}

func (l logs) Recent() ([]cli.LogLine, error) {
	var msgs []syslog.Message
	if err := l.call("recent", &msgs); err != nil {
		return nil, err
	}
	var out []cli.LogLine
	for _, m := range msgs {
		sev := "unknown"
		if m.Severity >= 0 && m.Severity < len(severityName) { // relayed messages come from other members
			sev = severityName[m.Severity]
		}
		out = append(out, cli.LogLine{Time: m.Time, Facility: m.Facility, Severity: sev, Text: m.Text, Host: m.Host, App: m.App, PID: m.PID})
	}
	return out, nil
}

func (l logs) Forwarders() ([]cli.ForwarderStatus, error) {
	var stats []syslog.Stats
	if err := l.call(svc.MethodStatus, &stats); err != nil {
		return nil, err
	}
	var out []cli.ForwarderStatus
	for _, s := range stats {
		out = append(out, cli.ForwarderStatus{
			Target:    fmt.Sprintf("%s:%d/%s", s.Host.Host, s.Host.Port, s.Host.Transport),
			Filter:    s.Host.Facility + "/" + s.Host.Severity,
			Connected: s.Connected, Sent: s.Sent, Dropped: s.Dropped, Queued: s.Queued, LastError: s.LastError,
		})
	}
	return out, nil
}

// syslogHosts converts the model's syslog configuration.
func syslogHosts(cfg *model.Config) []syslog.Host {
	var out []syslog.Host
	for _, h := range cfg.System.Syslog {
		out = append(out, syslog.Host{Host: h.Host, Port: h.Port, Transport: h.Transport,
			Facility: h.Facility, Severity: h.Severity, CAFile: h.CAFile})
	}
	return out
}

// checkNTP warns when another program on the switch also sets the clock.
func checkNTP(cfg *model.Config) model.Issues {
	if len(cfg.System.NTPServers) == 0 {
		return nil
	}
	if svc := ntp.OtherService("/proc"); svc != "" {
		return model.Issues{{Severity: model.Warning, Path: "system ntp",
			Msg: "the operating system runs another time service (" + svc + "); both would set the clock. Disable it"}}
	}
	return nil
}
