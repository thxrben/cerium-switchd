package updated

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thxrben/cerium-switchd/internal/software"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"github.com/thxrben/cerium-switchd/pkg/sysexec"
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
	f, err := hwio.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(hwio.Reader(f, 0))
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
	cmdline, err := hwio.ReadFile("/proc/cmdline")
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
	if err := hwio.MkdirAll(espMount, 0o700); err != nil {
		return err
	}
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
	if !rw {
		flags |= syscall.MS_RDONLY
	}
	res := hwio.Resource(s.Sys.ESP)
	if err := hwio.DoErr(res, "mount "+s.Sys.ESP, hwio.FileDeadline, func() error {
		return syscall.Mount(s.Sys.ESP, espMount, "vfat", flags, "flush")
	}); err != nil {
		return fmt.Errorf("mounting the ESP: %w", err)
	}
	ferr := f(espMount)
	// Only the ESP: a global sync would wait for a slot being written.
	if d, err := hwio.Open(espMount); err == nil {
		hwio.DoErr(res, "sync "+s.Sys.ESP, hwio.FileDeadline, func() error { return unix.Syncfs(int(d.Fd())) })
		d.Close()
	}
	if err := hwio.DoErr(res, "unmount "+s.Sys.ESP, hwio.FileDeadline, func() error { return syscall.Unmount(espMount, 0) }); err != nil && ferr == nil {
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

// SlotStatus reads the boot state from the ESP (mounted for it, so it is
// read from the disk) and probes both slot devices in parallel.
func (s *System) SlotStatus() ([]software.SlotInfo, string, error) {
	var env software.Env
	var problem string
	err := s.withESP(false, func(dir string) error {
		var err error
		env, problem, err = software.ReadEnvState(filepath.Join(dir, s.Sys.EnvPath))
		return err
	})
	if err != nil {
		env = software.Env{}
	}
	out := make([]software.SlotInfo, len(software.Slots))
	var wg sync.WaitGroup
	for i, sl := range software.Slots {
		out[i] = env.Slot(sl)
		off, _ := strconv.ParseInt(env[sl+"_HASHOFFSET"], 10, 64)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if perr := software.ProbeSlot(s.Sys.Dev[sl], off); perr != nil {
				out[i].Error = perr.Error()
			}
		}()
	}
	wg.Wait()
	return out, problem, err
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
	veritysetup := func(args ...string) error {
		_, err := sysexec.Command("veritysetup", args...).WithTimeout(time.Minute).CombinedOutput(context.Background())
		return err
	}
	veritysetup("close", name)
	if err := veritysetup("open", s.Sys.Dev[slot], name, s.Sys.Dev[slot], m.RootHash,
		fmt.Sprintf("--hash-offset=%d", m.HashOffset)); err != nil {
		return "", fmt.Errorf("the new slot does not match its verity root hash: %w", err)
	}
	defer veritysetup("close", name)
	dir, err := hwio.MkdirTemp("/run/switchd-update", "slot-")
	if err != nil {
		return "", err
	}
	defer hwio.Remove(dir)
	if err := hwio.DoErr("/dev/mapper/"+name, "mount", 30*time.Second, func() error {
		return syscall.Mount("/dev/mapper/"+name, dir, "squashfs", syscall.MS_RDONLY|syscall.MS_NODEV|syscall.MS_NOSUID, "")
	}); err != nil {
		return "", fmt.Errorf("mounting the new slot: %w", err)
	}
	defer hwio.DoErr("/dev/mapper/"+name, "unmount", 30*time.Second, func() error { return syscall.Unmount(dir, 0) })
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
	f, err := hwio.CreateTemp("/run/switchd-update", "config-*.json")
	if err != nil {
		return "", err
	}
	defer hwio.Remove(f.Name())
	f.Write(cfg)
	f.Close()
	return s.Check(filepath.Join(dir, "usr/local/sbin/switchd"), f.Name())
}

func (s *System) Reboot() error { _, err := sysexec.CombinedOutput("systemctl", "reboot"); return err }

// RunCheck runs a switchd program's configuration check: exit 0 accepts
// (output = warnings), 1 rejects.
func RunCheck(prog, cfgFile string) (string, error) {
	out, err := sysexec.Command(prog, "check-config", cfgFile).WithTimeout(2 * time.Minute).CombinedOutput(context.Background())
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
	raw, err := hwio.ReadFile(filepath.Join(switchdDir, "config", "rev", name))
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
