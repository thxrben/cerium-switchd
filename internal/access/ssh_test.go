package access

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mclag/internal/model"
)

func TestCLISSH(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "net"), 0o755)
	// A listener on port 22 (0x0016), e.g. the OS sshd.
	os.WriteFile(filepath.Join(dir, "net", "tcp"), []byte("  sl  local_address rem_address   st\n   0: 00000000:0016 00000000:0000 0A\n"), 0o644)
	var calls []string
	failCheck := false
	s := &SSH{Dir: filepath.Join(dir, "etc"), UnitPath: filepath.Join(dir, "unit"), ProcNet: filepath.Join(dir, "net"),
		LegacyDropIn: filepath.Join(dir, "legacy.conf"),
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Run: func(n string, a ...string) error {
			calls = append(calls, n+" "+strings.Join(a, " "))
			if n == "sshd" && failCheck {
				return errors.New("bad")
			}
			return nil
		}}
	cfg := &model.Config{}
	if err := s.Sync(cfg); err != nil || len(calls) != 0 {
		t.Fatalf("unconfigured: %v %v", err, calls)
	}
	cfg.System.SSH = model.SSHService{Configured: true, Port: 22, RootLogin: "deny"}
	if is := s.Check(cfg); !strings.Contains(is.String(), "port 22 is already used") {
		t.Errorf("port conflict not reported: %v", is)
	}
	cfg.System.SSH.Port = 2222
	cfg.System.Banner = "Authorized access only"
	if is := s.Check(cfg); len(is) != 0 {
		t.Errorf("free port reported: %v", is)
	}
	os.WriteFile(s.LegacyDropIn, []byte("Port 2222\n"), 0o644)
	if err := s.Sync(cfg); err != nil {
		t.Fatal(err)
	}
	conf, _ := os.ReadFile(s.confPath())
	for _, want := range []string{"Port 2222\n", "AllowGroups switchd-cli\n", "PermitRootLogin no\n", "Banner " + s.bannerPath(), "ForceCommand " + Shell, "AllowTcpForwarding no"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("config lacks %q:\n%s", want, conf)
		}
	}
	if fileExists(s.LegacyDropIn) {
		t.Error("legacy OS drop-in not removed")
	}
	for _, want := range []string{"systemctl reload ssh", "sshd -t -f " + s.confPath() + ".new", "systemctl enable switchd-sshd.service", "systemctl restart switchd-sshd.service"} {
		if !strings.Contains(strings.Join(calls, "\n"), want) {
			t.Errorf("missing call %q in %v", want, calls)
		}
	}
	if u, _ := os.ReadFile(s.UnitPath); !strings.Contains(string(u), "ExecStart=/usr/sbin/sshd -D") {
		t.Errorf("without a management instance sshd runs in the default VRF:\n%s", u)
	}
	// Our own running port is not a conflict.
	os.WriteFile(filepath.Join(dir, "net", "tcp"), []byte("hdr\n   0: 00000000:08AE 00000000:0000 0A\n"), 0o644)
	if is := s.Check(cfg); len(is) != 0 {
		t.Errorf("own port reported as conflict: %v", is)
	}
	calls = nil
	s.Sync(cfg)
	if len(calls) != 0 {
		t.Errorf("unchanged config: %v", calls)
	}
	// A rejected configuration keeps the previous one.
	failCheck = true
	cfg.System.SSH.RootLogin = "allow"
	if err := s.Sync(cfg); err == nil {
		t.Fatal("rejected config accepted")
	}
	if now, _ := os.ReadFile(s.confPath()); string(now) != string(conf) {
		t.Error("previous config not kept")
	}
	failCheck = false
	s.Sync(cfg)
	if now, _ := os.ReadFile(s.confPath()); !strings.Contains(string(now), "AllowGroups switchd-cli root\nPermitRootLogin yes") {
		t.Errorf("root-login allow:\n%s", now)
	}
	// Removal.
	cfg.System.SSH.Configured = false
	calls = nil
	s.Sync(cfg)
	if fileExists(s.UnitPath) || fileExists(s.confPath()) || !strings.Contains(strings.Join(calls, "\n"), "disable --now switchd-sshd.service") {
		t.Errorf("not removed: %v", calls)
	}
}

func TestCLISSHInManagementVRF(t *testing.T) {
	u := unitText(model.MgmtInstance)
	if !strings.Contains(u, "ExecStart=/usr/sbin/ip vrf exec mgmt_ceros /usr/sbin/sshd -D -f /etc/switchd/sshd_config") {
		t.Errorf("unit does not run sshd in the management VRF:\n%s", u)
	}
	if strings.Contains(unitText(""), "vrf exec") || strings.Contains(unitText(""), "@EXEC@") {
		t.Errorf("unit without a management instance:\n%s", unitText(""))
	}
}
