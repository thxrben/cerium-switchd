// Package usbstore reads and writes files on a USB stick of this switch
// (reference 3.4): the stick is found in sysfs, mounted through the kernel
// API only for one operation, and unmounted right after. The disk the
// system runs from is never used, even when it is a USB stick.
package usbstore

import (
	"bufio"
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
)

// ErrNoStick: no USB stick is plugged in.
var ErrNoStick = errors.New("no USB stick found")

// Stick is one USB disk that is not the system disk.
type Stick struct {
	Disk   string // kernel name, "sdb"
	Vendor string
	Model  string
	Size   int64 // bytes
	// Devices are the candidates to mount, in order: the partitions, or
	// the whole disk when it has none ("/dev/sdb1", ...).
	Devices []string
}

// Finder locates sticks; the paths are replaced in tests.
type Finder struct {
	SysRoot   string // "/sys"
	MountInfo string // "/proc/self/mountinfo"
}

func (f Finder) sys() string {
	if f.SysRoot == "" {
		return "/sys"
	}
	return f.SysRoot
}

// Sticks lists the USB disks in sysfs order, without the system disks.
func (f Finder) Sticks() ([]Stick, error) {
	sys := f.sys()
	system := f.systemDisks()
	disks, err := hwio.Glob(filepath.Join(sys, "block", "sd*"))
	if err != nil {
		return nil, err
	}
	slices.Sort(disks)
	var out []Stick
	for _, d := range disks {
		name := filepath.Base(d)
		if system[name] || !f.usb(d) {
			continue
		}
		s := Stick{Disk: name, Vendor: readTrim(filepath.Join(d, "device", "vendor")),
			Model: readTrim(filepath.Join(d, "device", "model"))}
		if n, err := strconv.ParseInt(readTrim(filepath.Join(d, "size")), 10, 64); err == nil {
			s.Size = n * 512
		}
		parts, _ := hwio.Glob(filepath.Join(d, name+"*"))
		slices.SortFunc(parts, func(a, b string) int { return naturalCmp(filepath.Base(a), filepath.Base(b)) })
		for _, p := range parts {
			if _, err := hwio.Stat(filepath.Join(p, "partition")); err == nil {
				s.Devices = append(s.Devices, "/dev/"+filepath.Base(p))
			}
		}
		if len(s.Devices) == 0 {
			s.Devices = []string{"/dev/" + name}
		}
		if s.Size == 0 {
			continue // no medium (a card reader without a card)
		}
		out = append(out, s)
	}
	return out, nil
}

// First returns the first stick.
func (f Finder) First() (Stick, error) {
	ss, err := f.Sticks()
	if err != nil {
		return Stick{}, err
	}
	if len(ss) == 0 {
		return Stick{}, ErrNoStick
	}
	return ss[0], nil
}

// usb reports whether a disk hangs on a USB bus or is removable.
func (f Finder) usb(disk string) bool {
	if readTrim(filepath.Join(disk, "removable")) == "1" {
		return true
	}
	real, err := hwio.EvalSymlinks(filepath.Join(disk, "device"))
	return err == nil && strings.Contains(real, "/usb")
}

// systemDisks are the disks the running system uses: those under a
// mounted file system (through device-mapper layers such as dm-verity)
// and those with a cerOS partition ("ceros-*" GPT names).
func (f Finder) systemDisks() map[string]bool {
	sys := f.sys()
	out := map[string]bool{}
	mi := f.MountInfo
	if mi == "" {
		mi = "/proc/self/mountinfo"
	}
	if b, err := hwio.ReadFile(mi); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) < 3 || strings.HasPrefix(fs[2], "0:") {
				continue // virtual file systems
			}
			for _, d := range f.disksOf(filepath.Join(sys, "dev", "block", fs[2]), 0) {
				out[d] = true
			}
		}
	}
	disks, _ := hwio.Glob(filepath.Join(sys, "block", "*"))
	for _, d := range disks {
		name := filepath.Base(d)
		parts, _ := hwio.Glob(filepath.Join(d, name+"*", "uevent"))
		for _, u := range parts {
			b, _ := hwio.ReadFile(u)
			for l := range strings.Lines(string(b)) {
				if strings.HasPrefix(strings.TrimSpace(l), "PARTNAME=ceros-") {
					out[name] = true
				}
			}
		}
	}
	return out
}

// disksOf resolves a block device (a sysfs path) to the disks below it:
// a partition to its disk, a device-mapper device to its slaves' disks.
func (f Finder) disksOf(dev string, depth int) []string {
	if depth > 8 {
		return nil
	}
	real, err := hwio.EvalSymlinks(dev)
	if err != nil {
		return nil
	}
	if slaves, _ := hwio.Glob(filepath.Join(real, "slaves", "*")); len(slaves) > 0 {
		var out []string
		for _, s := range slaves {
			out = append(out, f.disksOf(s, depth+1)...)
		}
		return out
	}
	// .../block/<disk>[/<partition>]
	parts := strings.Split(real, "/")
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] == "block" {
			return []string{parts[i+1]}
		}
	}
	return nil
}

func readTrim(p string) string {
	b, _ := hwio.ReadFile(p)
	return strings.TrimSpace(string(b))
}

// naturalCmp orders "sdb2" before "sdb10".
func naturalCmp(a, b string) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(a, b)
}

// Info describes the mounted stick.
type Info struct {
	Stick
	Device string `json:"device"` // the one mounted
	FSType string `json:"fs_type"`
	Label  string `json:"label,omitempty"`
}

// Entry is one file of a listing.
type Entry struct {
	Name    string    `json:"name"`
	Dir     bool      `json:"dir,omitempty"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// Listing is a directory of the stick.
type Listing struct {
	Info    Info    `json:"info"`
	Dir     string  `json:"dir"`
	Entries []Entry `json:"entries"`
}
