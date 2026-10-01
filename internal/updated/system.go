package updated

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"mclag/internal/software"
)

// Paths of the image (docs/os-image.md §3).
const (
	KeysDir     = "/usr/share/ceros/keys"
	ReleaseFile = "/etc/ceros-release"
	ConfigRoot  = "/config"
	espMount    = "/run/switchd-update/esp"
)

// Release reads /etc/ceros-release.
func Release(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			out[k] = strings.Trim(v, `"`)
		}
	}
	return out
}

// System is the Platform of a machine running from a cerOS image.
type System struct {
	Sys     *software.System
	Release map[string]string
	// Check runs "<program> check-config <file>" (the new image's program).
	Check func(prog, cfgFile string) (string, error)
	// ActiveConfig returns the active configuration as JSON (from switchd).
	ActiveConfig func() ([]byte, error)

	mu sync.Mutex // the ESP mount
}

// NewSystem detects the image's slots.
func NewSystem() (*System, error) {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, err
	}
	sys, err := software.DetectSystem(string(cmdline), "/sys", "/dev")
	if err != nil {
		return nil, err
	}
	return &System{Sys: sys, Release: Release(ReleaseFile)}, nil
}

func (s *System) Active() string  { return s.Sys.Active }
func (s *System) Version() string { return s.Release["VERSION"] }

func (s *System) Keys() ([]software.PublicKey, error) { return software.LoadKeys(KeysDir) }

// withESP mounts the ESP for f (it is not mounted otherwise).
func (s *System) withESP(rw bool, f func(dir string) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(espMount, 0o700); err != nil {
		return err
	}
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
	if !rw {
		flags |= syscall.MS_RDONLY
	}
	if err := syscall.Mount(s.Sys.ESP, espMount, "vfat", flags, "flush"); err != nil {
		return fmt.Errorf("mounting the ESP: %w", err)
	}
	ferr := f(espMount)
	// Only the ESP: a global sync would wait for a slot being written.
	if d, err := os.Open(espMount); err == nil {
		unix.Syncfs(int(d.Fd()))
		d.Close()
	}
	if err := syscall.Unmount(espMount, 0); err != nil && ferr == nil {
		ferr = fmt.Errorf("unmounting the ESP: %w", err)
	}
	return ferr
}

func (s *System) ReadEnv() (software.Env, error) {
	var env software.Env
	err := s.withESP(false, func(dir string) error {
		var err error
		env, err = software.ReadEnv(filepath.Join(dir, s.Sys.EnvPath))
		return err
	})
	return env, err
}

func (s *System) WriteEnv(env software.Env) error {
	return s.withESP(true, func(dir string) error {
		return software.WriteEnv(filepath.Join(dir, s.Sys.EnvPath), env)
	})
}

func (s *System) WriteSlot(slot string, img io.Reader, size int64, sha string) error {
	return s.Sys.WriteSlot(slot, img, size, sha, nil)
}

// CheckConfig opens the new slot with dm-verity, mounts it read-only and
// runs its switchd's configuration check.
func (s *System) CheckConfig(slot string, m *software.BundleManifest) (string, error) {
	name := "ceros-check-" + slot
	exec.Command("veritysetup", "close", name).Run()
	if out, err := exec.Command("veritysetup", "open", s.Sys.Dev[slot], name, s.Sys.Dev[slot], m.RootHash,
		fmt.Sprintf("--hash-offset=%d", m.HashOffset)).CombinedOutput(); err != nil {
		return "", fmt.Errorf("the new slot does not match its verity root hash: %v %s", err, strings.TrimSpace(string(out)))
	}
	defer exec.Command("veritysetup", "close", name).Run()
	dir, err := os.MkdirTemp("/run/switchd-update", "slot-")
	if err != nil {
		return "", err
	}
	defer os.Remove(dir)
	if err := syscall.Mount("/dev/mapper/"+name, dir, "squashfs", syscall.MS_RDONLY|syscall.MS_NODEV|syscall.MS_NOSUID, ""); err != nil {
		return "", fmt.Errorf("mounting the new slot: %w", err)
	}
	defer syscall.Unmount(dir, 0)
	if rel := Release(filepath.Join(dir, "etc/ceros-release")); rel["VERSION"] != m.Version {
		return "", fmt.Errorf("the new slot says version %q, the bundle %q", rel["VERSION"], m.Version)
	}
	if s.ActiveConfig == nil || s.Check == nil {
		return "", nil
	}
	cfg, err := s.ActiveConfig()
	if err != nil {
		return "", fmt.Errorf("reading the active configuration: %w", err)
	}
	f, err := os.CreateTemp("/run/switchd-update", "config-*.json")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	f.Write(cfg)
	f.Close()
	return s.Check(filepath.Join(dir, "usr/local/sbin/switchd"), f.Name())
}

func (s *System) Reboot() error { return exec.Command("systemctl", "reboot").Run() }

// RunCheck runs a switchd program's configuration check: exit 0 accepts
// (output = warnings), 1 rejects.
func RunCheck(prog, cfgFile string) (string, error) {
	out, err := exec.Command(prog, "check-config", cfgFile).CombinedOutput()
	text := strings.TrimSpace(string(out))
	var ee *exec.ExitError
	switch {
	case err == nil:
		return text, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return "", errors.New(text)
	default:
		return "", fmt.Errorf("the new program cannot check the configuration: %v %s", err, text)
	}
}

// ConfigFromStore returns the newest committed revision's configuration
// from switchd's store (<switchdDir>/config/rev/*.json).
func ConfigFromStore(switchdDir string) ([]byte, error) {
	name := newestRevision(switchdDir)
	if name == "" {
		return []byte("{}"), nil
	}
	raw, err := os.ReadFile(filepath.Join(switchdDir, "config", "rev", name))
	if err != nil {
		return nil, err
	}
	var rev struct {
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(raw, &rev); err != nil || len(rev.Config) == 0 {
		return nil, fmt.Errorf("revision %s: unreadable", name)
	}
	return rev.Config, nil
}
