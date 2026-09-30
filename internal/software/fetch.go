package software

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Source is where a package comes from (reference 3.6).
type Source struct {
	Raw  string
	Kind string // "url", "usb", "file"
	URL  *url.URL
	Path string // usb: path on the stick; file: local path
}

// ParseSource checks a source: http(s)://, ftp://, sftp://user@host/path,
// usb:<file> or an absolute local path.
func ParseSource(s string) (Source, error) {
	switch {
	case strings.HasPrefix(s, "usb:"):
		p := strings.TrimPrefix(strings.TrimPrefix(s, "usb:"), "/")
		if p == "" || strings.Contains(p, "..") {
			return Source{}, fmt.Errorf("expecting usb:<file>")
		}
		return Source{Raw: s, Kind: "usb", Path: p}, nil
	case strings.HasPrefix(s, "/"):
		return Source{Raw: s, Kind: "file", Path: filepath.Clean(s)}, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return Source{}, fmt.Errorf("invalid source %q (expecting http(s)://, ftp://, sftp://, usb:<file> or a local path)", s)
	}
	switch u.Scheme {
	case "http", "https", "ftp", "sftp":
	default:
		return Source{}, fmt.Errorf("unsupported source %q", u.Scheme)
	}
	return Source{Raw: s, Kind: "url", URL: u}, nil
}

// Fetcher downloads packages on the master.
type Fetcher struct {
	// VRF is the management instance (downloads leave through it; "":
	// the default routing table).
	VRF string
	// Run runs a command (tests replace it).
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// SysRoot is "/sys" (tests: a fake tree); MountDir a scratch directory.
	SysRoot, MountDir string
}

func (f *Fetcher) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if f.Run != nil {
		return f.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Fetch stores the package at dst. password is for sftp/ftp logins ("":
// none, or the user's key).
func (f *Fetcher) Fetch(ctx context.Context, src Source, dst, password string) error {
	switch src.Kind {
	case "file":
		return copyFile(src.Path, dst)
	case "usb":
		return f.fromUSB(ctx, src.Path, dst)
	}
	return f.curl(ctx, src.URL, dst, password)
}

// FetchOptional fetches a small companion file (e.g. <package>.sha256);
// a missing one is not an error (ok false).
func (f *Fetcher) FetchOptional(ctx context.Context, src Source, suffix string) (string, bool) {
	tmp, err := os.CreateTemp("", "ceros-*"+suffix)
	if err != nil {
		return "", false
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	var s Source
	switch src.Kind {
	case "url":
		u := *src.URL
		u.Path += suffix
		s = Source{Kind: "url", URL: &u}
	default:
		s = src
		s.Path += suffix
	}
	if f.Fetch(ctx, s, tmp.Name(), "") != nil {
		return "", false
	}
	b, err := os.ReadFile(tmp.Name())
	return string(b), err == nil
}

func (f *Fetcher) curl(ctx context.Context, u *url.URL, dst, password string) error {
	args := []string{"-fsS", "--retry", "2", "--connect-timeout", "15", "-o", dst}
	if u.User != nil {
		user := u.User.Username()
		if p, ok := u.User.Password(); ok {
			password = p
		}
		clean := *u
		clean.User = nil
		if password != "" {
			args = append(args, "-u", user+":"+password)
		} else {
			args = append(args, "-u", user+":")
			for _, k := range []string{"/root/.ssh/id_ed25519", "/root/.ssh/id_rsa"} {
				if _, err := os.Stat(k); err == nil && u.Scheme == "sftp" {
					args = append(args, "--key", k)
					break
				}
			}
		}
		u = &clean
	}
	if u.Scheme == "sftp" {
		args = append(args, "--insecure") // no known_hosts on a switch; the package is verified instead
	}
	args = append(args, u.String())
	name := "curl"
	if f.VRF != "" {
		args = append([]string{"vrf", "exec", f.VRF, "curl"}, args...)
		name = "ip"
	}
	if out, err := f.run(ctx, name, args...); err != nil {
		return fmt.Errorf("download of %s failed: %s", u.Redacted(), strings.TrimSpace(string(out)))
	}
	return nil
}

// fromUSB copies path from the first USB stick (mounted read-only while
// it is read).
func (f *Fetcher) fromUSB(ctx context.Context, path, dst string) error {
	dev, err := f.usbDevice()
	if err != nil {
		return err
	}
	dir := f.MountDir
	if dir == "" {
		dir = "/run/switchd/usb"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if out, err := f.run(ctx, "mount", "-o", "ro", dev, dir); err != nil {
		return fmt.Errorf("mounting %s: %s", dev, strings.TrimSpace(string(out)))
	}
	defer f.run(context.Background(), "umount", dir)
	return copyFile(filepath.Join(dir, path), dst)
}

// usbDevice returns the first partition (or the whole disk) of the first
// removable USB disk.
func (f *Fetcher) usbDevice() (string, error) {
	root := f.SysRoot
	if root == "" {
		root = "/sys"
	}
	disks, _ := filepath.Glob(filepath.Join(root, "block", "sd*"))
	for _, d := range disks {
		rem, _ := os.ReadFile(filepath.Join(d, "removable"))
		link, _ := os.Readlink(filepath.Join(d, "device"))
		real, _ := filepath.EvalSymlinks(filepath.Join(d, "device"))
		if strings.TrimSpace(string(rem)) != "1" && !strings.Contains(link+real, "/usb") {
			continue
		}
		name := filepath.Base(d)
		parts, _ := filepath.Glob(filepath.Join(d, name+"*"))
		if len(parts) > 0 {
			return "/dev/" + filepath.Base(parts[0]), nil
		}
		return "/dev/" + name, nil
	}
	return "", errors.New("no USB stick found")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
