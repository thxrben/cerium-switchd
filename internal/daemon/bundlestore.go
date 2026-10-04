package daemon

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thxrben/cerium-switchd/internal/software"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// bundleStore keeps software bundles in memory only (reference 3.6): a
// tmpfs of its own, sized to the room a transfer may take before each
// one, so no bundle is written to a disk and none pushes the running
// system out of memory.
type bundleStore struct {
	dir string
	log *slog.Logger
	// mount mounts the tmpfs at dir with size bytes, or changes the size
	// of the mounted one (tests: a plain directory).
	mount func(dir string, size uint64) error
	// available is the memory available now (MemAvailable); slot is the
	// update slot of system memory (0: no slots).
	available func() (uint64, error)
	slot      func() uint64

	mu   sync.Mutex
	used map[string]time.Time // bundle file -> last use
}

// bundleDir is the store's mount point.
const bundleDir = "/run/ceros-software"

// keepFree is what a bundle leaves of the available memory without slots.
const keepFree = 256 << 20

// unusedFor is how long a bundle nobody uses stays.
const unusedFor = time.Hour

func newBundleStore(log *slog.Logger) *bundleStore {
	return &bundleStore{dir: bundleDir, log: log, mount: mountTmpfs, available: memAvailable,
		slot: func() uint64 { return 0 }}
}

// path is the file of version v ("upload" is the uploaded bundle).
func (s *bundleStore) path(v string) string {
	if v == "upload" {
		return filepath.Join(s.dir, "upload.bundle")
	}
	return filepath.Join(s.dir, "ceros-"+v+"-"+software.Arch()+".bundle")
}

// room is how many bytes a new bundle may take: the update slot, or the
// available memory less keepFree. Bundles already held count as free:
// a new one replaces them (except keep).
func (s *bundleStore) room(keep ...string) (uint64, error) {
	held := s.kept(keep)
	if sl := s.slot(); sl > 0 {
		return sl - min(sl, held), nil
	}
	avail, err := s.available()
	if err != nil {
		return 0, err
	}
	avail += s.size(nil) - held // the replaced bundles' memory comes back
	if avail <= keepFree {
		return 0, nil
	}
	return avail - keepFree, nil
}

// size is the bytes of the files in the store (only those in only,
// unless it is nil).
func (s *bundleStore) size(only []string) uint64 {
	files, _ := hwio.Glob(filepath.Join(s.dir, "*"))
	var n uint64
	for _, f := range files {
		if only != nil && !containsPath(only, f) {
			continue
		}
		if fi, err := hwio.Stat(f); err == nil {
			n += uint64(fi.Size())
		}
	}
	return n
}

// kept is the bytes of the bundles in keep.
func (s *bundleStore) kept(keep []string) uint64 {
	if len(keep) == 0 {
		return 0
	}
	return s.size(keep)
}

func containsPath(list []string, p string) bool {
	for _, x := range list {
		if x == p {
			return true
		}
	}
	return false
}

// prepare makes room for a new bundle of size bytes (0: unknown) and
// returns the room: every bundle but keep is removed, and the tmpfs is
// sized so that it holds keep and the room, no more.
func (s *bundleStore) prepare(size uint64, keep ...string) (uint64, error) {
	room, err := s.room(keep...)
	if err != nil {
		return 0, err
	}
	if size > 0 && size > room {
		return room, fmt.Errorf("%w: it has %s, the room is %s", software.ErrTooLarge, mib(size), mib(room))
	}
	files, _ := hwio.Glob(filepath.Join(s.dir, "*"))
	s.mu.Lock()
	for _, f := range files {
		if !containsPath(keep, f) {
			hwio.Remove(f)
			delete(s.used, f)
		}
	}
	s.mu.Unlock()
	// The tmpfs never takes more than the room: a transfer of unknown size
	// ends with "no space" instead of pushing the system out of memory.
	if err := s.mount(s.dir, s.kept(keep)+room+(1<<20)); err != nil {
		return 0, fmt.Errorf("memory for the bundle: %w", err)
	}
	return room, nil
}

// touch marks a bundle as used now.
func (s *bundleStore) touch(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used == nil {
		s.used = map[string]time.Time{}
	}
	s.used[path] = time.Now()
}

// sweep removes bundles unused for an hour, except while busy (an update
// uses them).
func (s *bundleStore) sweep(busy bool) {
	if busy {
		return
	}
	files, _ := hwio.Glob(filepath.Join(s.dir, "*"))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range files {
		t, ok := s.used[f]
		if !ok {
			if fi, err := hwio.Stat(f); err == nil {
				t = fi.ModTime()
			}
		}
		if time.Since(t) > unusedFor {
			s.log.Info("software: bundle removed from memory (unused for an hour)", "bundle", filepath.Base(f))
			hwio.Remove(f)
			delete(s.used, f)
		}
	}
}

func mib(n uint64) string { return fmt.Sprintf("%d MB", (n+(1<<20)-1)>>20) }

// mountTmpfs mounts a tmpfs of size bytes at dir, or resizes the one
// mounted there.
func mountTmpfs(dir string, size uint64) error {
	if err := hwio.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	opts := "mode=0700,size=" + strconv.FormatUint((size+4095)/4096*4096, 10)
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if mounted(dir) {
		flags |= unix.MS_REMOUNT
	}
	return unix.Mount("tmpfs", dir, "tmpfs", flags, opts)
}

// mounted reports whether a file system is mounted at dir.
func mounted(dir string) bool {
	b, err := hwio.ReadFile("/proc/self/mounts")
	if err != nil {
		return false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) > 1 && f[1] == dir {
			return true
		}
	}
	return false
}

// memAvailable reads MemAvailable from /proc/meminfo.
func memAvailable() (uint64, error) {
	b, err := hwio.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, err := strconv.ParseUint(f[1], 10, 64)
			return kb << 10, err
		}
	}
	return 0, fmt.Errorf("no MemAvailable in /proc/meminfo")
}
