package daemon

import (
	"fmt"

	"mclag/internal/cli"
	"mclag/internal/model"
	"mclag/internal/ntp"
	"mclag/internal/syslog"
)

var severityName = []string{"emergency", "alert", "critical", "error", "warning", "notice", "info", "debug"}

// logs adapts the syslog hub to the CLI.
type logs struct{ hub *syslog.Hub }

func (l logs) Recent() []cli.LogLine {
	var out []cli.LogLine
	for _, m := range l.hub.Recent() {
		out = append(out, cli.LogLine{Time: m.Time, Facility: m.Facility, Severity: severityName[m.Severity], Text: m.Text})
	}
	return out
}

func (l logs) Forwarders() []cli.ForwarderStatus {
	var out []cli.ForwarderStatus
	for _, s := range l.hub.Stats() {
		out = append(out, cli.ForwarderStatus{
			Target:    fmt.Sprintf("%s:%d/%s", s.Host.Host, s.Host.Port, s.Host.Transport),
			Filter:    s.Host.Facility + "/" + s.Host.Severity,
			Connected: s.Connected, Sent: s.Sent, Dropped: s.Dropped, Queued: s.Queued, LastError: s.LastError,
		})
	}
	return out
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
