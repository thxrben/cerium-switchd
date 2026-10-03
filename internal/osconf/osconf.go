// Package osconf applies the parts of the configuration that live in the
// operating system's own files: host name (/etc/hostname, /etc/hosts) and
// resolver (/etc/resolv.conf). Reference 5.1 system host-name, name-server.
package osconf

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// Host keeps the OS files in line with the configuration.
type Host struct {
	Root     string // "" = /
	StateDir string // where the replaced resolv.conf is kept
	Log      *slog.Logger
	// SetHostname sets the kernel host name (replaceable in tests).
	SetHostname func(name string) error
	// Hostname returns the kernel host name.
	Hostname func() (string, error)
}

func (h *Host) path(p string) string { return filepath.Join(h.Root, p) }

// Sync applies the host name and resolver of member.
func (h *Host) Sync(cfg *model.Config, member int) error {
	return errors.Join(h.syncHostName(cfg, member), h.syncResolver(cfg))
}

// HostName returns the configured host name of member ("" = none).
func HostName(cfg *model.Config, member int) string { return cfg.MemberHostName(member) }

func writeIfChanged(path, content string, mode os.FileMode) (bool, error) {
	if old, err := hwio.ReadFile(path); err == nil && string(old) == content {
		return false, nil
	}
	tmp := path + ".switchd-tmp"
	if err := hwio.WriteFile(tmp, []byte(content), mode); err != nil {
		return false, err
	}
	return true, hwio.Rename(tmp, path)
}

func (h *Host) syncHostName(cfg *model.Config, member int) error {
	name := HostName(cfg, member)
	if name == "" {
		return nil // not configured: the OS keeps its name
	}
	var errs []error
	if cur, err := h.Hostname(); err != nil || cur != name {
		if err := h.SetHostname(name); err != nil {
			errs = append(errs, fmt.Errorf("host name: %w", err))
		} else {
			h.Log.Info("host name set", "name", name, "was", cur)
		}
	}
	if _, err := writeIfChanged(h.path("/etc/hostname"), name+"\n", 0o644); err != nil {
		errs = append(errs, err)
	}
	line := "127.0.1.1\t" + name
	if d := cfg.System.DomainName; d != "" {
		line = "127.0.1.1\t" + name + "." + d + " " + name
	}
	hosts := h.path("/etc/hosts")
	raw, err := hwio.ReadFile(hosts)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(append(errs, err)...)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(raw) == 0 {
		lines = []string{"127.0.0.1\tlocalhost"}
	}
	found := false
	for i, l := range lines {
		if f := strings.Fields(l); len(f) > 0 && f[0] == "127.0.1.1" {
			if found {
				lines[i] = "" // one line only
				continue
			}
			lines[i], found = line, true
		}
	}
	if !found {
		// After the localhost line, like the Debian installer.
		pos := 0
		for i, l := range lines {
			if strings.HasPrefix(l, "127.0.0.1") {
				pos = i + 1
				break
			}
		}
		lines = append(lines[:pos], append([]string{line}, lines[pos:]...)...)
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l != "" || len(out) == 0 || out[len(out)-1] != "" {
			out = append(out, l)
		}
	}
	if _, err := writeIfChanged(hosts, strings.Join(out, "\n")+"\n", 0o644); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// resolvBackup records the resolv.conf that switchd replaced.
type resolvBackup struct {
	Absent  bool   `json:"absent,omitempty"`
	Symlink string `json:"symlink,omitempty"` // it was a symlink to this
	Content string `json:"content,omitempty"`
}

const resolvHeader = "# Managed by switchd (system name-server). Changes are overwritten.\n"

func (h *Host) syncResolver(cfg *model.Config) error {
	path := h.path("/etc/resolv.conf")
	backupPath := filepath.Join(h.StateDir, "resolv.conf.orig.json")
	servers := cfg.System.NameServers
	if len(servers) > 3 {
		servers = servers[:3]
	}
	if len(servers) == 0 {
		raw, err := hwio.ReadFile(backupPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil // never managed
		}
		if err != nil {
			return err
		}
		var b resolvBackup
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		switch {
		case b.Absent:
			err = hwio.Remove(path)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		case b.Symlink != "":
			tmp := path + ".switchd-tmp"
			hwio.Remove(tmp)
			if err = hwio.Symlink(b.Symlink, tmp); err == nil {
				err = hwio.Rename(tmp, path)
			}
		default:
			_, err = writeIfChanged(path, b.Content, 0o644)
		}
		if err != nil {
			return fmt.Errorf("restoring resolv.conf: %w", err)
		}
		h.Log.Info("resolver configuration restored (name-server removed)")
		return hwio.Remove(backupPath)
	}

	var b strings.Builder
	b.WriteString(resolvHeader)
	if d := cfg.System.DomainName; d != "" {
		fmt.Fprintf(&b, "search %s\n", d)
	}
	for _, s := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", s)
	}
	want := b.String()

	_, err := hwio.Stat(backupPath)
	managed := err == nil
	if !managed {
		var bk resolvBackup
		if fi, err := hwio.Lstat(path); errors.Is(err, os.ErrNotExist) {
			bk.Absent = true
		} else if err != nil {
			return err
		} else if fi.Mode()&os.ModeSymlink != 0 {
			if bk.Symlink, err = hwio.Readlink(path); err != nil {
				return err
			}
		} else {
			raw, err := hwio.ReadFile(path)
			if err != nil {
				return err
			}
			bk.Content = string(raw)
		}
		raw, _ := json.Marshal(bk)
		if err := hwio.MkdirAll(h.StateDir, 0o700); err != nil {
			return err
		}
		if err := hwio.WriteFile(backupPath, raw, 0o600); err != nil {
			return err
		}
	}
	// A symlink (e.g. to systemd-resolved's stub) is replaced by a file.
	if fi, err := hwio.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		hwio.Remove(path)
	}
	old, _ := hwio.ReadFile(path)
	changed, err := writeIfChanged(path, want, 0o644)
	if err != nil {
		return err
	}
	if changed && managed && strings.HasPrefix(string(old), resolvHeader) {
		h.Log.Info("resolver configuration updated")
	} else if changed && managed {
		h.Log.Warn("resolv.conf was changed by another program (a DHCP client?); switchd restored it. Disable that program's resolver handling.")
	} else if changed {
		h.Log.Info("resolver configuration taken over; the previous file is kept for when name-server is removed")
	}
	return nil
}
