package software

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"golang.org/x/sys/unix"
)

// The boot state and the slots (docs/os-image.md §4).

// ---- grubenv ----

// EnvSize is the size of a GRUB environment block.
const EnvSize = 1024

const envHeader = "# GRUB Environment Block\n"

// Env is the boot state: GRUB environment variables.
type Env map[string]string

// ParseEnv reads a GRUB environment block.
func ParseEnv(raw []byte) (Env, error) {
	if !bytes.HasPrefix(raw, []byte(envHeader)) {
		return nil, errors.New("not a GRUB environment block")
	}
	env := Env{}
	for _, l := range strings.Split(string(raw[len(envHeader):]), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		env[k] = v
	}
	return env, nil
}

// Bytes formats the block (EnvSize bytes, padded with '#').
func (e Env) Bytes() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(envHeader)
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := e[k]
		if strings.ContainsAny(k, "=\n\\#") || strings.ContainsAny(v, "\n\\") || k == "" {
			return nil, fmt.Errorf("boot state: bad variable %q=%q", k, v)
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	if b.Len() > EnvSize {
		return nil, errors.New("boot state: too large for the GRUB environment block")
	}
	b.Write(bytes.Repeat([]byte{'#'}, EnvSize-b.Len()))
	return b.Bytes(), nil
}

// ReadEnv reads the boot state from path. A missing or damaged block reads
// as the defaults GRUB uses then (ORDER="A B", both slots OK).
func ReadEnv(path string) (Env, error) {
	env, _, err := ReadEnvState(path)
	return env, err
}

// ReadEnvState is ReadEnv that also says when the defaults were used
// (problem: "missing" or "damaged").
func ReadEnvState(path string) (env Env, problem string, err error) {
	raw, err := hwio.ReadFile(path)
	switch {
	case err == nil:
		if env, perr := ParseEnv(raw); perr == nil {
			return env, "", nil
		}
		problem = "damaged"
	case errors.Is(err, os.ErrNotExist):
		problem = "missing"
	default:
		return nil, "", err
	}
	return Env{"ORDER": "A B", "A_OK": "1", "B_OK": "1", "A_TRY": "0", "B_TRY": "0"}, problem, nil
}

// WriteEnv writes the boot state in place: the file keeps its blocks (GRUB
// writes it the same way), it is one 1024-byte write and is synced.
func WriteEnv(path string, e Env) error {
	raw, err := e.Bytes()
	if err != nil {
		return err
	}
	return hwio.DoErr(hwio.Resource(path), "write "+path, hwio.FileDeadline(), func() error {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		if _, err := f.WriteAt(raw, 0); err != nil {
			f.Close()
			return err
		}
		if err := f.Truncate(EnvSize); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
}

// Slots are A and B.
var Slots = []string{"A", "B"}

// OtherSlot returns the other slot.
func OtherSlot(s string) string {
	if s == "A" {
		return "B"
	}
	return "A"
}

// SlotInfo is what the boot state says about a slot.
type SlotInfo struct {
	Name string `json:"name"`
	// Version is the version the boot state recorded when the slot was
	// written.
	Version string `json:"version,omitempty"`
	// Error: the slot's device cannot be read, or does not hold a cerOS
	// image ("": it reads fine, or was not checked).
	Error string `json:"error,omitempty"`
	OK    bool   `json:"ok"`
	Tried bool   `json:"tried"` // started, not confirmed
	First bool   `json:"first"` // first in ORDER: booted next
}

// Order returns the slots in boot order (every slot exactly once).
func (e Env) Order() []string {
	var out []string
	for _, s := range strings.Fields(e["ORDER"]) {
		if slices.Contains(Slots, s) && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, s := range Slots {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// Slot returns what the boot state says about s.
func (e Env) Slot(s string) SlotInfo {
	return SlotInfo{Name: s, Version: e[s+"_VERSION"], OK: e[s+"_OK"] != "0", Tried: e[s+"_TRY"] == "1", First: e.Order()[0] == s}
}

// First puts s first in the boot order.
func (e Env) First(s string) {
	e["ORDER"] = s + " " + OtherSlot(s)
}

// Confirm records that s started and works.
func (e Env) Confirm(s string) { e[s+"_TRY"] = "0" }

// Invalidate marks s as not bootable (being written, or failed); the other
// slot goes first.
func (e Env) Invalidate(s string) {
	e[s+"_OK"] = "0"
	e.First(OtherSlot(s))
}

// Installed records a complete, verified image in s and makes it the slot
// to boot next.
func (e Env) Installed(s string, m *BundleManifest) {
	e[s+"_OK"] = "1"
	e[s+"_TRY"] = "0"
	e[s+"_ROOTHASH"] = m.RootHash
	e[s+"_HASHOFFSET"] = fmt.Sprint(m.HashOffset)
	e[s+"_VERSION"] = m.Version
	e.First(s)
}

// ---- slot devices ----

// System is this machine's disk as the image sees it.
type System struct {
	// Active is the running slot (from the kernel command line).
	Active string
	// Dev maps each slot to its block device.
	Dev map[string]string
	// ESP is the ESP's block device; EnvPath the boot state on it, relative
	// to the ESP's root.
	ESP     string
	EnvPath string
}

// EnvFile is the boot state's path on the ESP.
const EnvFile = "EFI/ceros/grubenv"

// DetectSystem finds the slots from the kernel command line (ceros.slot,
// ceros.part = the partition UUID of the running slot) and sysfs. It
// fails when the system does not run from a cerOS image.
func DetectSystem(cmdline, sysRoot, devRoot string) (*System, error) {
	args := map[string]string{}
	for _, f := range strings.Fields(cmdline) {
		if k, v, ok := strings.Cut(f, "="); ok {
			args[k] = v
		}
	}
	slot, part := args["ceros.slot"], args["ceros.part"]
	if !slices.Contains(Slots, slot) || part == "" {
		return nil, errors.New("this system does not run from a cerOS image (no ceros.slot on the kernel command line)")
	}
	dev, err := hwio.EvalSymlinks(filepath.Join(devRoot, "disk/by-partuuid", strings.ToLower(part)))
	if err != nil {
		return nil, fmt.Errorf("the running slot's partition %s: %w", part, err)
	}
	partDir, err := hwio.EvalSymlinks(filepath.Join(sysRoot, "class/block", filepath.Base(dev)))
	if err != nil {
		return nil, err
	}
	disk := filepath.Dir(partDir)
	diskName := filepath.Base(disk)
	byNum := map[string]string{}
	ents, _ := hwio.ReadDir(disk)
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), diskName) {
			continue
		}
		n, err := hwio.ReadFile(filepath.Join(disk, e.Name(), "partition"))
		if err == nil {
			byNum[strings.TrimSpace(string(n))] = filepath.Join(devRoot, e.Name())
		}
	}
	sys := &System{Active: slot, Dev: map[string]string{"A": byNum["2"], "B": byNum["3"]}, ESP: byNum["1"], EnvPath: EnvFile}
	if sys.Dev["A"] == "" || sys.Dev["B"] == "" || sys.ESP == "" {
		return nil, fmt.Errorf("the disk %s does not have the cerOS partitions", diskName)
	}
	return sys, nil
}

// slotIODeadline bounds one read, write or sync of a slot or a bundle
// (4 MiB on a slow disk or USB stick: at least ~70 KB/s).
var slotIODeadline atomic.Int64

func init() { SetSlotIODeadline(60 * time.Second) }

// SlotIODeadline is the deadline of one slot or bundle chunk (system
// timeouts slot-write).
func SlotIODeadline() time.Duration { return time.Duration(slotIODeadline.Load()) }

// SetSlotIODeadline changes it (zero: unchanged).
func SetSlotIODeadline(d time.Duration) {
	if d > 0 {
		slotIODeadline.Store(int64(d))
	}
}

// Backup is the slot that is not running.
func (s *System) Backup() string { return OtherSlot(s.Active) }

// WriteSlot writes the image into the slot's device (never the active
// slot), syncs it, reads it back and compares its SHA-256 with want.
// progress (may be nil) is called with the bytes written.
func (s *System) WriteSlot(slot string, img io.Reader, size int64, want string, progress func(int64)) error {
	if slot == s.Active {
		return errors.New("the running slot is never written")
	}
	dev := s.Dev[slot]
	raw, err := hwio.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	// Every write, seek and sync has a deadline: a disk that stops
	// answering fails the update instead of hanging it.
	f := hwio.Writer(raw, SlotIODeadline())
	if devSize, err := f.Seek(0, io.SeekEnd); err == nil && devSize > 0 && !isRegular(raw) && devSize < size {
		f.Close()
		return fmt.Errorf("the image (%d bytes) does not fit into slot %s (%d bytes)", size, slot, devSize)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	buf := make([]byte, 4<<20)
	var n int64
	for {
		k, rerr := img.Read(buf)
		if k > 0 {
			if _, err := f.Write(buf[:k]); err != nil {
				f.Close()
				return fmt.Errorf("writing slot %s: %w", slot, err)
			}
			n += int64(k)
			// Every chunk is synced: no sync has more than 4 MiB to
			// write (64 MiB took longer than the deadline on a slow
			// disk), and the other partitions of the disk (/var) are
			// not held back behind a long queue.
			if err := f.Sync(); err != nil {
				f.Close()
				return fmt.Errorf("writing slot %s: %w", slot, err)
			}
			if progress != nil {
				progress(n)
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	if n != size {
		f.Close()
		return fmt.Errorf("the image has %d bytes, %d expected", n, size)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("writing slot %s: %w", slot, err)
	}
	f.Close()
	got, err := sumDevice(dev, size)
	if err != nil {
		return fmt.Errorf("reading slot %s back: %w", slot, err)
	}
	if got != want {
		return fmt.Errorf("slot %s reads back with SHA-256 %s instead of %s: the disk may be failing", slot, got, want)
	}
	return nil
}

func isRegular(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode().IsRegular()
}

// sumDevice hashes the first size bytes of dev, past the page cache.
func sumDevice(dev string, size int64) (string, error) {
	raw, err := hwio.Open(dev)
	if err != nil {
		return "", err
	}
	f := hwio.Reader(raw, SlotIODeadline())
	defer f.Close()
	// Drop the cached pages: the read must come from the disk.
	if err := hwio.DoErr(hwio.Resource(dev), "flush cache "+dev, SlotIODeadline(), func() error {
		_ = unix.Fadvise(int(raw.Fd()), 0, 0, unix.FADV_DONTNEED)
		if !isRegular(raw) {
			_ = unix.IoctlSetInt(int(raw.Fd()), unix.BLKFLSBUF, 0)
		}
		return nil
	}); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.CopyN(h, f, size); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ProbeDeadline bounds each read of ProbeSlot.
var ProbeDeadline = 5 * time.Second

// ProbeSlot reads the slot's device directly (O_DIRECT: from the disk, not
// a cache) and checks that it holds an image: the squashfs superblock at
// its start and, if hashOffset > 0, the verity superblock of its hash tree.
// A disk that does not answer fails at ProbeDeadline.
func ProbeSlot(dev string, hashOffset int64) error {
	if dev == "" {
		return errors.New("no partition")
	}
	head, err := readDirect(dev, 0)
	if err != nil {
		return err
	}
	if string(head[:4]) != "hsqs" {
		return errors.New("no cerOS image (no file system at the start of the partition)")
	}
	if hashOffset > 0 {
		if hashOffset%4096 != 0 {
			return fmt.Errorf("bad hash offset %d", hashOffset)
		}
		sb, err := readDirect(dev, hashOffset)
		if err != nil {
			return err
		}
		if string(sb[:8]) != "verity\x00\x00" {
			return errors.New("the image's hash tree is missing or damaged")
		}
	}
	return nil
}

// readDirect reads 4 KiB at off with O_DIRECT. The buffer is page aligned
// (mmap); after a timeout it stays mapped, since the kernel may still
// write into it.
func readDirect(dev string, off int64) ([]byte, error) {
	buf, err := unix.Mmap(-1, 0, 4096, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	n, err := hwio.Do(hwio.Resource(dev), "read "+dev, ProbeDeadline, func() (int, error) {
		f, err := os.OpenFile(dev, os.O_RDONLY|unix.O_DIRECT, 0)
		if err != nil {
			// Not every file system takes O_DIRECT (tests on tmpfs).
			if f, err = os.Open(dev); err != nil {
				return 0, err
			}
		}
		defer f.Close()
		n, err := f.ReadAt(buf, off)
		if errors.Is(err, io.EOF) && n > 0 {
			err = nil
		}
		return n, err
	})
	if err != nil {
		if !errors.Is(err, hwio.ErrTimeout) {
			unix.Munmap(buf)
		}
		return nil, fmt.Errorf("unreadable: %w", err)
	}
	out := make([]byte, n)
	copy(out, buf[:n])
	unix.Munmap(buf)
	if n < 8 {
		return nil, errors.New("unreadable: the partition is too small")
	}
	return out, nil
}
