package osconf

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mclag/internal/model"
)

func newHost(t *testing.T) (*Host, *string) {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	kernel := "debian"
	h := &Host{Root: root, StateDir: filepath.Join(root, "state"), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		SetHostname: func(n string) error { kernel = n; return nil },
		Hostname:    func() (string, error) { return kernel, nil }}
	return h, &kernel
}

func read(t *testing.T, h *Host, p string) string {
	raw, _ := os.ReadFile(h.path(p))
	return string(raw)
}

func cfgWith(host, member, domain string, ns ...string) *model.Config {
	c := &model.Config{Members: map[int]*model.Member{1: {ID: 1, HostName: member}}}
	c.System.HostName, c.System.DomainName, c.System.NameServers = host, domain, ns
	return c
}

func TestHostName(t *testing.T) {
	h, kernel := newHost(t)
	os.WriteFile(h.path("/etc/hosts"), []byte("127.0.0.1\tlocalhost\n127.0.1.1\told.example old\n\n::1 localhost ip6-localhost\n"), 0o644)
	os.WriteFile(h.path("/etc/hostname"), []byte("old\n"), 0o644)

	// Not configured: nothing changes.
	if err := h.Sync(cfgWith("", "", ""), 1); err != nil {
		t.Fatal(err)
	}
	if *kernel != "debian" || read(t, h, "/etc/hostname") != "old\n" {
		t.Error("unconfigured host name was changed")
	}
	// The member's name wins over the stack's.
	h.Sync(cfgWith("stack", "sw1", "lab.example"), 1)
	if *kernel != "sw1" || read(t, h, "/etc/hostname") != "sw1\n" {
		t.Errorf("kernel %q file %q", *kernel, read(t, h, "/etc/hostname"))
	}
	hosts := read(t, h, "/etc/hosts")
	if !strings.Contains(hosts, "127.0.1.1\tsw1.lab.example sw1\n") || strings.Contains(hosts, "old") || !strings.Contains(hosts, "::1 localhost") {
		t.Errorf("hosts:\n%s", hosts)
	}
	h.Sync(cfgWith("stack", "", ""), 1)
	if hosts := read(t, h, "/etc/hosts"); *kernel != "stack" || !strings.Contains(hosts, "127.0.1.1\tstack\n") {
		t.Errorf("kernel %q hosts:\n%s", *kernel, hosts)
	}
	// No 127.0.1.1 line yet: added after localhost.
	os.WriteFile(h.path("/etc/hosts"), []byte("127.0.0.1 localhost\n::1 localhost\n"), 0o644)
	h.Sync(cfgWith("stack", "", ""), 1)
	if hosts := read(t, h, "/etc/hosts"); hosts != "127.0.0.1 localhost\n127.0.1.1\tstack\n::1 localhost\n" {
		t.Errorf("hosts:\n%q", hosts)
	}
}

func TestResolver(t *testing.T) {
	h, _ := newHost(t)
	orig := "# from dhcp\nnameserver 10.5.150.1\n"
	os.WriteFile(h.path("/etc/resolv.conf"), []byte(orig), 0o644)
	h.Sync(cfgWith("", "", "lab.example", "1.1.1.1", "9.9.9.9", "8.8.8.8", "8.8.4.4"), 1)
	got := read(t, h, "/etc/resolv.conf")
	if got != resolvHeader+"search lab.example\nnameserver 1.1.1.1\nnameserver 9.9.9.9\nnameserver 8.8.8.8\n" {
		t.Errorf("resolv.conf:\n%s", got)
	}
	// Another program rewrites it: restored.
	os.WriteFile(h.path("/etc/resolv.conf"), []byte("nameserver 6.6.6.6\n"), 0o644)
	h.Sync(cfgWith("", "", "lab.example", "1.1.1.1"), 1)
	if got := read(t, h, "/etc/resolv.conf"); !strings.Contains(got, "nameserver 1.1.1.1") || strings.Contains(got, "6.6.6.6") {
		t.Errorf("not restored:\n%s", got)
	}
	// Removing name-server brings back the original file.
	h.Sync(cfgWith("", "", ""), 1)
	if got := read(t, h, "/etc/resolv.conf"); got != orig {
		t.Errorf("original not restored:\n%s", got)
	}
	// A symlink (systemd-resolved) comes back as the same symlink.
	os.Remove(h.path("/etc/resolv.conf"))
	os.Symlink("../run/systemd/resolve/stub-resolv.conf", h.path("/etc/resolv.conf"))
	h.Sync(cfgWith("", "", "", "1.1.1.1"), 1)
	if fi, _ := os.Lstat(h.path("/etc/resolv.conf")); fi.Mode()&os.ModeSymlink != 0 {
		t.Error("symlink not replaced")
	}
	h.Sync(cfgWith("", "", ""), 1)
	if l, err := os.Readlink(h.path("/etc/resolv.conf")); err != nil || l != "../run/systemd/resolve/stub-resolv.conf" {
		t.Errorf("symlink not restored: %q %v", l, err)
	}
	// Unmanaged and unconfigured: untouched.
	h.Sync(cfgWith("", "", ""), 1)
	if l, _ := os.Readlink(h.path("/etc/resolv.conf")); l == "" {
		t.Error("unmanaged file touched")
	}
}
