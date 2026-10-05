//go:build linux

package usbstore

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unsafe"

	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"golang.org/x/sys/unix"
)

// MaxConfig is the largest configuration file read from a stick.
const MaxConfig = 16 << 20

// Mounter mounts and unmounts (tests replace it).
type Mounter interface {
	Mount(dev, dir, fstype string, readOnly bool) error
	Unmount(dir string) error
}

// fsTypes are tried in this order.
var fsTypes = []string{"vfat", "exfat", "ext4"}

// Store runs one operation on the first stick at a time.
type Store struct {
	Finder  Finder
	Mounter Mounter // nil: the kernel
	Dir     string  // mount point, "/run/switchd/usb"
	// Wait is how long an operation waits for another one (default 5 s).
	Wait time.Duration

	sem chan struct{}
}

// ErrBusy: another operation holds the stick.
var ErrBusy = errors.New("the USB stick is busy with another operation")

func (s *Store) lock() error {
	if s.sem == nil {
		panic("usbstore: use New")
	}
	wait := s.Wait
	if wait == 0 {
		wait = 5 * time.Second
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-t.C:
		return ErrBusy
	}
}

func (s *Store) unlock() { <-s.sem }

// New returns a store mounting at dir ("" = /run/switchd/usb).
func New(f Finder, m Mounter, dir string) *Store {
	if dir == "" {
		dir = "/run/switchd/usb"
	}
	if m == nil {
		m = kernelMounter{}
	}
	return &Store{Finder: f, Mounter: m, Dir: dir, sem: make(chan struct{}, 1)}
}

// with mounts the first stick, runs fn with the root directory open, and
// unmounts (also when fn fails).
func (s *Store) with(readOnly bool, fn func(root *os.File, info Info) error) error {
	if err := s.lock(); err != nil {
		return err
	}
	defer s.unlock()
	st, err := s.Finder.First()
	if err != nil {
		return err
	}
	if err := hwio.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	info, err := s.mount(st, readOnly)
	if err != nil {
		return err
	}
	defer s.unmount(info.Device)
	root, err := s.open(info.Device, s.Dir, unix.O_DIRECTORY|unix.O_RDONLY, 0, unix.AT_FDCWD)
	if err != nil {
		return err
	}
	defer root.Close()
	info.Label = label(root)
	if err := fn(root, info); err != nil {
		return err
	}
	if !readOnly {
		// Everything on the stick before it is unmounted.
		return hwio.DoErr(res(info.Device), "sync "+info.Device, hwio.FileDeadline(), func() error {
			return unix.Syncfs(int(root.Fd()))
		})
	}
	return nil
}

func res(dev string) string { return "USB stick " + dev }

func (s *Store) mount(st Stick, readOnly bool) (Info, error) {
	var last error
	for _, dev := range st.Devices {
		for _, t := range fsTypes {
			err := hwio.DoErr(res(dev), "mount "+dev, hwio.FileDeadline(), func() error {
				return s.Mounter.Mount(dev, s.Dir, t, readOnly)
			})
			if err == nil {
				return Info{Stick: st, Device: dev, FSType: t}, nil
			}
			if errors.Is(err, unix.EBUSY) {
				// Still mounted from an operation that hung: free it.
				s.Mounter.Unmount(s.Dir)
			}
			last = err
		}
	}
	return Info{}, fmt.Errorf("the USB stick %s has no file system cerOS can use (FAT, exFAT or ext4): %v", st.Disk, last)
}

func (s *Store) unmount(dev string) {
	hwio.DoErr(res(dev), "unmount "+dev, hwio.FileDeadline(), func() error { return s.Mounter.Unmount(s.Dir) })
}

// open opens rel below the directory dirfd (no "..", absolute or symlink
// escapes); with unix.AT_FDCWD, rel is an ordinary path.
func (s *Store) open(dev, rel string, flags int, mode uint32, dirfd int) (*os.File, error) {
	how := &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC), Mode: uint64(mode)}
	if dirfd != unix.AT_FDCWD {
		how.Resolve = unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS
	}
	fd, err := hwio.Do(res(dev), "open "+rel, hwio.FileDeadline(), func() (int, error) { return unix.Openat2(dirfd, rel, how) })
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: "usb:" + strings.TrimPrefix(rel, "/"), Err: err}
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// clean checks a path given by the user ("dir/file", relative to the
// stick's root).
func clean(p string) (string, error) {
	p = strings.TrimPrefix(p, "/")
	c := path.Clean(p)
	if c == "." && p != "" && p != "." {
		return "", fmt.Errorf("invalid path %q", p)
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("usb:%s is outside the stick", p)
	}
	return c, nil
}

// ErrTooLarge: the file exceeds the limit given.
var ErrTooLarge = errors.New("too large")

// Read returns a file of at most max bytes.
func (s *Store) Read(p string, max int64) ([]byte, error) {
	var b bytes.Buffer
	if _, err := s.CopyTo(p, &b, max); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// CopyTo copies a file of at most max bytes to w (a bundle into memory);
// a file whose size exceeds max fails before anything is copied.
func (s *Store) CopyTo(p string, w io.Writer, max int64) (int64, error) {
	rel, err := clean(p)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.with(true, func(root *os.File, info Info) error {
		f, err := s.open(info.Device, rel, unix.O_RDONLY, 0, int(root.Fd()))
		if err != nil {
			return err
		}
		defer f.Close()
		if fi, err := f.Stat(); err == nil && fi.Size() > max {
			return fmt.Errorf("usb:%s: %w (%d bytes, at most %d)", rel, ErrTooLarge, fi.Size(), max)
		}
		n, err = io.Copy(w, io.LimitReader(hwio.Reader(f, hwio.FileDeadline()), max+1))
		if err != nil {
			return err
		}
		if n > max {
			return fmt.Errorf("usb:%s: %w (more than %d bytes)", rel, ErrTooLarge, max)
		}
		return nil
	})
	return n, err
}

// Write stores data at p: written under a temporary name in the same
// directory, synced and renamed over p.
func (s *Store) Write(p string, data []byte) error {
	rel, err := clean(p)
	if err != nil {
		return err
	}
	if rel == "." {
		return errors.New("expecting usb:<file>")
	}
	return s.with(false, func(root *os.File, info Info) error {
		dir, base := path.Split(rel)
		if dir == "" {
			dir = "."
		}
		d, err := s.open(info.Device, dir, unix.O_DIRECTORY|unix.O_RDONLY, 0, int(root.Fd()))
		if err != nil {
			return err
		}
		defer d.Close()
		var rnd [4]byte
		rand.Read(rnd[:])
		tmp := "." + base + ".tmp-" + hex.EncodeToString(rnd[:])
		f, err := s.open(info.Device, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o644, int(d.Fd()))
		if err != nil {
			return err
		}
		ok := false
		defer func() {
			if !ok {
				unix.Unlinkat(int(d.Fd()), tmp, 0)
			}
		}()
		if _, err := hwio.Writer(f, hwio.FileDeadline()).Write(data); err != nil {
			f.Close()
			return err
		}
		if err := hwio.Sync(f); err != nil {
			f.Close()
			return err
		}
		f.Close()
		if err := hwio.DoErr(res(info.Device), "rename usb:"+rel, hwio.FileDeadline(), func() error {
			return unix.Renameat(int(d.Fd()), tmp, int(d.Fd()), base)
		}); err != nil {
			return fmt.Errorf("usb:%s: %w", rel, err)
		}
		ok = true
		return nil
	})
}

// List lists a directory ("" = the root); hidden temporary files of
// interrupted writes are left out.
func (s *Store) List(p string) (Listing, error) {
	rel, err := clean(p)
	if err != nil {
		return Listing{}, err
	}
	var out Listing
	err = s.with(true, func(root *os.File, info Info) error {
		out.Info, out.Dir = info, rel
		d, err := s.open(info.Device, rel, unix.O_DIRECTORY|unix.O_RDONLY, 0, int(root.Fd()))
		if err != nil {
			return err
		}
		defer d.Close()
		es, err := hwio.Do(res(info.Device), "list usb:"+rel, hwio.FileDeadline(), func() ([]os.DirEntry, error) { return d.ReadDir(-1) })
		if err != nil {
			return err
		}
		for _, e := range es {
			if tmpName.MatchString(e.Name()) {
				continue
			}
			// Relative to the open directory (DirEntry.Info would look the
			// name up from the working directory).
			var st unix.Stat_t
			if err := unix.Fstatat(int(d.Fd()), e.Name(), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				continue
			}
			out.Entries = append(out.Entries, Entry{Name: e.Name(), Dir: e.IsDir(), Size: st.Size,
				ModTime: time.Unix(st.Mtim.Unix())})
		}
		slices.SortFunc(out.Entries, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
		return nil
	})
	return out, err
}

var tmpName = regexp.MustCompile(`^\..+\.tmp-[0-9a-f]{8}$`)

// Eject unmounts the stick (should a hung operation have left it), removes
// the disk from the kernel and powers its USB port off.
func (s *Store) Eject() (Stick, error) {
	if err := s.lock(); err != nil {
		return Stick{}, err
	}
	defer s.unlock()
	st, err := s.Finder.First()
	if err != nil {
		return st, err
	}
	s.Mounter.Unmount(s.Dir) // normally not mounted
	disk := filepath.Join(s.Finder.sys(), "block", st.Disk)
	real, err := hwio.EvalSymlinks(filepath.Join(disk, "device"))
	if err != nil {
		return st, err
	}
	// The disk first (the kernel flushes its cache), then the USB device.
	if err := hwio.WriteFile(filepath.Join(disk, "device", "delete"), []byte("1"), 0o200); err != nil {
		return st, fmt.Errorf("removing %s: %w", st.Disk, err)
	}
	if usb := usbDevice(real); usb != "" {
		if err := hwio.WriteFile(filepath.Join(usb, "remove"), []byte("1"), 0o200); err != nil {
			return st, fmt.Errorf("powering the USB port off: %w", err)
		}
	}
	return st, nil
}

// usbPort matches a USB device's sysfs name ("2-1", "1-1.4").
var usbPort = regexp.MustCompile(`^\d+-\d+(\.\d+)*$`)

// usbDevice returns the USB device directory above a SCSI device path.
func usbDevice(real string) string {
	parts := strings.Split(real, "/")
	for i := len(parts) - 1; i > 0; i-- {
		if usbPort.MatchString(parts[i]) {
			return strings.Join(parts[:i+1], "/")
		}
	}
	return ""
}

// fsIocGetFSLabel is FS_IOC_GETFSLABEL (_IOR(0x94, 49, char[256])).
const fsIocGetFSLabel = 0x81009431

// label reads the file system's label where the kernel offers it (ext4,
// exFAT on newer kernels; FAT has no such call: none shown).
func label(root *os.File) string {
	var b [256]byte
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, root.Fd(), fsIocGetFSLabel, uintptr(unsafe.Pointer(&b[0]))); e != 0 {
		return ""
	}
	n := bytes.IndexByte(b[:], 0)
	if n < 0 {
		n = len(b)
	}
	return strings.TrimSpace(string(b[:n]))
}
