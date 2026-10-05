//go:build linux

package usbstore

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeMounter "mounts" by doing nothing (the mount point already holds the
// stick's files); it accepts only fstype ok and records the calls.
type fakeMounter struct {
	ok    string
	calls []string
}

func (m *fakeMounter) Mount(dev, dir, fstype string, ro bool) error {
	mode := "rw"
	if ro {
		mode = "ro"
	}
	m.calls = append(m.calls, "mount "+dev+" "+fstype+" "+mode)
	if fstype != m.ok {
		return unix.EINVAL
	}
	return nil
}

func (m *fakeMounter) Unmount(dir string) error {
	m.calls = append(m.calls, "unmount")
	return nil
}

func newTestStore(t *testing.T) (*Store, *fakeMounter, string) {
	f := fakeSys{t, t.TempDir()}
	f.disk("sdd", true, 48, [2]string{"sdd1", "data"})
	m := &fakeMounter{ok: "exfat"}
	mnt := filepath.Join(t.TempDir(), "usb")
	os.MkdirAll(filepath.Join(mnt, "configs"), 0o755)
	os.WriteFile(filepath.Join(mnt, "configs", "base.conf"), []byte("system { host-name a; }\n"), 0o644)
	return New(Finder{SysRoot: f.root, MountInfo: "/nonexistent"}, m, mnt), m, mnt
}

func TestReadWriteList(t *testing.T) {
	s, m, mnt := newTestStore(t)
	b, err := s.Read("configs/base.conf", MaxConfig)
	if err != nil || string(b) != "system { host-name a; }\n" {
		t.Fatalf("read %q %v", b, err)
	}
	// vfat refused, exfat taken; read-only; unmounted after.
	if want := []string{"mount /dev/sdd1 vfat ro", "mount /dev/sdd1 exfat ro", "unmount"}; !slices.Equal(m.calls, want) {
		t.Fatalf("calls %v, want %v", m.calls, want)
	}
	m.calls = nil
	if err := s.Write("/sw1.conf", []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"mount /dev/sdd1 vfat rw", "mount /dev/sdd1 exfat rw", "unmount"}; !slices.Equal(m.calls, want) {
		t.Fatalf("calls %v", m.calls)
	}
	if b, _ := os.ReadFile(filepath.Join(mnt, "sw1.conf")); string(b) != "new\n" {
		t.Fatalf("written %q", b)
	}
	// An interrupted write's temporary file is not listed.
	os.WriteFile(filepath.Join(mnt, ".x.conf.tmp-0123abcd"), nil, 0o644)
	l, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"configs", "sw1.conf"}) || !l.Entries[0].Dir || l.Entries[1].Size != 4 ||
		l.Info.Device != "/dev/sdd1" || l.Info.FSType != "exfat" {
		t.Fatalf("listing %+v", l)
	}
	if es, _ := os.ReadDir(mnt); len(es) != 3 {
		t.Fatalf("left behind: %v", es)
	}
}

// Nothing outside the stick: "..", absolute paths and symlinks resolve
// inside its root.
func TestNoEscape(t *testing.T) {
	s, m, mnt := newTestStore(t)
	secret := filepath.Join(filepath.Dir(mnt), "secret")
	os.WriteFile(secret, []byte("outside"), 0o644)
	os.Symlink(secret, filepath.Join(mnt, "abs-link"))
	os.Symlink("../secret", filepath.Join(mnt, "rel-link"))
	for _, p := range []string{"../secret", "configs/../../secret"} {
		if _, err := s.Read(p, MaxConfig); err == nil || !strings.Contains(err.Error(), "outside the stick") {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range []string{"abs-link", "rel-link"} {
		if b, err := s.Read(p, MaxConfig); err == nil {
			t.Errorf("%s read %q through a symlink out of the stick", p, b)
		}
	}
	if err := s.Write("rel-link", []byte("x")); err != nil {
		t.Fatal(err) // replaces the link itself, inside the stick
	}
	if b, _ := os.ReadFile(secret); string(b) != "outside" {
		t.Fatalf("a write went outside: %q", b)
	}
	_ = m
}

func TestLimitsAndErrors(t *testing.T) {
	s, m, mnt := newTestStore(t)
	os.WriteFile(filepath.Join(mnt, "big"), make([]byte, 100), 0o644)
	if _, err := s.Read("big", 99); err == nil {
		t.Fatal("size limit")
	}
	// A failing operation still unmounts.
	m.calls = nil
	if _, err := s.Read("missing", MaxConfig); err == nil || m.calls[len(m.calls)-1] != "unmount" {
		t.Fatalf("err %v calls %v", err, m.calls)
	}
	// No usable file system.
	m.ok = "ntfs"
	if _, err := s.Read("big", 1000); err == nil || !strings.Contains(err.Error(), "FAT, exFAT or ext4") {
		t.Fatalf("err %v", err)
	}
	// One operation at a time.
	s.Wait = 20 * time.Millisecond
	s.sem <- struct{}{}
	if _, err := s.Read("big", 1000); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	<-s.sem
}
