// Package packaging holds the files switchd installs on the operating
// system itself.
package packaging

import _ "embed"

// Unit is switchd's systemd unit. ExecStart names /usr/local/sbin/switchd;
// switchd writes its own program path there when it installs the unit.
//
//go:embed switchd.service
var Unit string

// UpdateUnit is the update daemon's unit (switchd-update, reference 3.6).
//
//go:embed switchd-update.service
var UpdateUnit string
