package access

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// CLIGroup is the group of managed users; only it (and root, by policy)
// may log in through the CLI SSH server.
const CLIGroup = "switchd-cli"

// SSH runs switchd's own SSH server instance for the CLI (reference 5.1
// system services ssh). The OS SSH server is never modified.
type SSH struct {
	Dir      string // /etc/switchd
	UnitPath string // /etc/systemd/system/switchd-sshd.service
	// LegacyDropIn is an OS sshd drop-in written by earlier versions; it is
	// removed.
	LegacyDropIn string
	ProcNet      string // /proc/net (listening sockets)
	Log          *slog.Logger
	Run          func(name string, args ...string) error
}

const sshUnit = "switchd-sshd.service"

var rootLogin = map[string]string{"deny": "no", "allow": "yes", "key-only": "prohibit-password"}

func (s *SSH) confPath() string   { return filepath.Join(s.Dir, "sshd_config") }
func (s *SSH) bannerPath() string { return filepath.Join(s.Dir, "banner") }

func (s *SSH) config(cfg *model.Config) string {
	var b strings.Builder
	b.WriteString("# Managed by switchd (system services ssh). Changes are overwritten.\n")
	fmt.Fprintf(&b, "Port %d\n", cfg.System.SSH.Port)
	b.WriteString("PidFile /run/switchd-sshd.pid\n")
	for _, k := range []string{"ed25519", "ecdsa", "rsa"} {
		if p := "/etc/ssh/ssh_host_" + k + "_key"; fileExists(p) {
			fmt.Fprintf(&b, "HostKey %s\n", p)
		}
	}
	groups := CLIGroup
	if cfg.System.SSH.RootLogin != "deny" {
		groups += " root"
	}
	fmt.Fprintf(&b, "AllowGroups %s\n", groups)
	fmt.Fprintf(&b, "PermitRootLogin %s\n", rootLogin[cfg.System.SSH.RootLogin])
	b.WriteString("PubkeyAuthentication yes\nPasswordAuthentication yes\nKbdInteractiveAuthentication no\nUsePAM yes\n")
	b.WriteString("AuthorizedKeysFile .ssh/authorized_keys\nPrintMotd no\n")
	b.WriteString("X11Forwarding no\nAllowTcpForwarding no\nAllowAgentForwarding no\nAllowStreamLocalForwarding no\nPermitTunnel no\n")
	if cfg.System.Banner != "" {
		fmt.Fprintf(&b, "Banner %s\n", s.bannerPath())
	}
	// root's shell is bash; on this port it lands in the CLI as well.
	fmt.Fprintf(&b, "Match User root\n    ForceCommand %s\n", Shell)
	return b.String()
}

// unitText is the systemd unit. With a management instance the server runs
// inside its VRF (ip vrf exec binds the sockets to it): it is then reachable
// through the management interfaces only, never through data interfaces or
// the operating system's own NICs. The VRF appears with the data plane, so
// a start before that simply retries.
func unitText(vrf string) string {
	exec := ""
	if vrf != "" {
		exec = "/usr/sbin/ip vrf exec " + vrf + " "
	}
	return strings.NewReplacer("@EXEC@", exec).Replace(unitTemplate)
}

const unitTemplate = `[Unit]
Description=cerOS CLI SSH server (managed by switchd)
After=network.target

[Service]
ExecStartPre=/usr/sbin/sshd -t -f /etc/switchd/sshd_config
ExecStart=@EXEC@/usr/sbin/sshd -D -f /etc/switchd/sshd_config
ExecReload=/usr/sbin/sshd -t -f /etc/switchd/sshd_config
ExecReload=/bin/kill -HUP $MAINPID
KillMode=process
Restart=on-failure
RestartSec=2
RestartPreventExitStatus=255
RuntimeDirectory=sshd
RuntimeDirectoryMode=0755
RuntimeDirectoryPreserve=yes
`

func fileExists(p string) bool { _, err := hwio.Stat(p); return err == nil }

// currentPort returns the port of the running managed instance (0 = none).
func (s *SSH) currentPort() int {
	raw, err := hwio.ReadFile(s.confPath())
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if p, ok := strings.CutPrefix(l, "Port "); ok {
			n, _ := strconv.Atoi(p)
			return n
		}
	}
	return 0
}

// listening reports whether any TCP socket listens on port.
func (s *SSH) listening(port int) bool {
	want := fmt.Sprintf(":%04X", port)
	for _, f := range []string{"tcp", "tcp6"} {
		raw, err := hwio.ReadFile(filepath.Join(s.ProcNet, f))
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(raw), "\n")[1:] {
			fs := strings.Fields(l)
			if len(fs) > 3 && fs[3] == "0A" && strings.HasSuffix(fs[1], want) {
				return true
			}
		}
	}
	return false
}

// Check rejects a port that another program already uses.
func (s *SSH) Check(cfg *model.Config) model.Issues {
	ssh := cfg.System.SSH
	if !ssh.Configured || ssh.Port == s.currentPort() || !s.listening(ssh.Port) {
		return nil
	}
	return model.Issues{{Severity: model.Error, Path: "system services ssh port",
		Msg: fmt.Sprintf("port %d is already used by another program (e.g. the OS SSH server); choose another port", ssh.Port)}}
}

func writeIfChanged(path, content string, mode os.FileMode) (bool, error) {
	if old, err := hwio.ReadFile(path); err == nil && string(old) == content {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := hwio.WriteFile(tmp, []byte(content), mode); err != nil {
		return false, err
	}
	return true, hwio.Rename(tmp, path)
}

// Sync converges the CLI SSH server. It runs only on the master, inside
// the management instance (reference 1.8, 5.1 system services ssh).
func (s *SSH) Sync(cfg *model.Config, master bool) error {
	if s.LegacyDropIn != "" && fileExists(s.LegacyDropIn) {
		if err := hwio.Remove(s.LegacyDropIn); err == nil {
			_ = s.Run("systemctl", "reload", "ssh")
		}
	}
	if !cfg.System.SSH.Configured || cfg.System.MgmtInstance == "" || !master {
		if !fileExists(s.UnitPath) {
			return nil
		}
		_ = s.Run("systemctl", "disable", "--now", sshUnit)
		for _, p := range []string{s.UnitPath, s.confPath(), s.bannerPath()} {
			_ = hwio.Remove(p)
		}
		s.Log.Info("ssh: CLI SSH server stopped (not configured, no management instance, or not the master)")
		return s.Run("systemctl", "daemon-reload")
	}
	if err := hwio.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	if _, err := writeIfChanged(s.bannerPath(), cfg.System.Banner+"\n", 0o644); err != nil {
		return err
	}
	conf := s.config(cfg)
	old, _ := hwio.ReadFile(s.confPath())
	confChanged := string(old) != conf
	if confChanged {
		tmp := s.confPath() + ".new"
		if err := hwio.WriteFile(tmp, []byte(conf), 0o600); err != nil {
			return err
		}
		if err := s.Run("sshd", "-t", "-f", tmp); err != nil {
			hwio.Remove(tmp)
			return fmt.Errorf("ssh: configuration rejected by sshd, previous one kept: %w", err)
		}
		if err := hwio.Rename(tmp, s.confPath()); err != nil {
			return err
		}
	}
	vrf := cfg.System.MgmtInstance
	unitChanged, err := writeIfChanged(s.UnitPath, unitText(vrf), 0o644)
	if err != nil {
		return err
	}
	if unitChanged {
		if err := s.Run("systemctl", "daemon-reload"); err != nil {
			return err
		}
		// switchd starts the server itself, once the management instance
		// exists: started by systemd at boot, it would fail before the
		// instance's VRF is there.
		_ = s.Run("systemctl", "disable", sshUnit)
	}
	if confChanged || unitChanged {
		// A changed unit (e.g. now inside the management VRF) needs a real
		// restart: a reload keeps the old process and its sockets. Open
		// sessions survive (KillMode=process).
		verb := "reload-or-restart"
		if unitChanged {
			verb = "restart"
		}
		if err := s.Run("systemctl", verb, sshUnit); err != nil {
			return errors.Join(errors.New("ssh: CLI SSH server did not start"), err)
		}
		s.Log.Info("ssh: CLI SSH server configured", "port", cfg.System.SSH.Port, "root_login", cfg.System.SSH.RootLogin)
		return nil
	}
	// Unchanged, but not running (after a boot, or it failed): start it.
	if s.Run("systemctl", "is-active", "--quiet", sshUnit) != nil {
		if err := s.Run("systemctl", "restart", sshUnit); err != nil {
			return errors.Join(errors.New("ssh: CLI SSH server did not start"), err)
		}
		s.Log.Warn("ssh: CLI SSH server was not running; started")
	}
	return nil
}
