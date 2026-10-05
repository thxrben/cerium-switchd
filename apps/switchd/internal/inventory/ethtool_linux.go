//go:build linux

package inventory

import (
	"bytes"
	"encoding/binary"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
	"github.com/thxrben/cerium-switchd/lib/sys/nlx"
	"golang.org/x/sys/unix"
)

// Caps are the hardware facts of a port that commit checks and "show
// system offload" use (reference 1.7). Zero values mean unknown.
type Caps struct {
	MaxSpeedMbps int
	Pause        Tristate // pause frames (802.3x)
	// Features are the driver's feature flags that are active ("on") or
	// available but off ("off"), e.g. "hw-tc-offload": "on".
	Features  map[string]string
	Switchdev bool // the port belongs to a hardware switch (switchdev)
}

// Tristate is yes/no/unknown.
type Tristate int

const (
	Unknown Tristate = iota
	Yes
	No
)

const (
	ethtoolGSSetInfo      = 0x37
	ethtoolGStrings       = 0x1b
	ethtoolGFeatures      = 0x3a
	ethtoolGLinkSettings  = 0x4c
	ethSSFeatures         = 4
	ethGStringLen         = 32
	ethtoolGPauseParam    = 0x12
	maxLinkModeWords      = 8
	linkSettingsHeaderLen = 48
)

// linkModeSpeed maps ethtool link mode bits to their speed in Mbit/s.
var linkModeSpeed = map[int]int{
	0: 10, 1: 10, 2: 100, 3: 100, 4: 1000, 5: 1000, 12: 10000, 15: 2500, 17: 1000, 18: 10000, 19: 10000,
	21: 20000, 22: 20000, 23: 40000, 24: 40000, 25: 40000, 26: 40000, 27: 56000, 28: 56000, 29: 56000, 30: 56000,
	31: 25000, 32: 25000, 33: 25000, 34: 50000, 35: 50000, 36: 100000, 37: 100000, 38: 100000, 39: 100000,
	40: 50000, 41: 1000, 42: 10000, 43: 10000, 44: 10000, 45: 10000, 46: 10000, 47: 2500, 48: 5000,
	52: 50000, 53: 50000, 54: 50000, 55: 50000, 56: 50000, 57: 100000, 58: 100000, 59: 100000, 60: 100000,
	61: 100000, 62: 200000, 63: 200000, 64: 200000, 65: 200000, 66: 200000, 67: 100, 68: 1000,
	69: 400000, 70: 400000, 71: 400000, 72: 400000, 73: 400000, 75: 100000, 76: 100000, 77: 100000,
	78: 100000, 79: 100000, 80: 200000, 81: 200000, 82: 200000, 83: 200000, 84: 200000, 85: 400000,
	86: 400000, 87: 400000, 88: 400000, 89: 400000, 90: 100, 91: 10, 92: 10, 93: 10,
}

type ifreq struct {
	Name [unix.IFNAMSIZ]byte
	Data uintptr
	_    [16]byte
}

// ethtool runs an ethtool ioctl with the kernel deadline. It works on a
// copy of buf, copied back when it succeeds: a call that timed out may
// still write into its buffer later.
func ethtool(name string, buf []byte) error {
	tmp := slices.Clone(buf)
	err := hwio.DoErr(nlx.Resource, "ethtool "+name, 0, func() error { return ethtoolIoctl(name, tmp) })
	if err == nil {
		copy(buf, tmp)
	}
	return err
}

func ethtoolIoctl(name string, buf []byte) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var ifr ifreq
	copy(ifr.Name[:], name)
	ifr.Data = uintptr(unsafe.Pointer(&buf[0]))
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCETHTOOL, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return errno
	}
	return nil
}

// ReadCaps reads the capabilities of a kernel interface.
func ReadCaps(sysRoot, linux string) Caps {
	var c Caps
	c.MaxSpeedMbps = linkModes(linux)
	c.Pause = pauseSupport(linux)
	c.Features = features(linux)
	if raw, err := hwio.ReadFile(filepath.Join(sysRoot, "class", "net", linux, "phys_switch_id")); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		c.Switchdev = true
	}
	return c
}

// pauseSupport reports whether the driver has pause-frame settings
// (ETHTOOL_GPAUSEPARAM). The link modes' "Pause" bit is not used: it only
// describes autonegotiation advertisement.
func pauseSupport(linux string) Tristate {
	buf := make([]byte, 16)
	binary.LittleEndian.PutUint32(buf, ethtoolGPauseParam)
	switch err := ethtool(linux, buf); {
	case err == nil:
		return Yes
	case errors.Is(err, unix.EOPNOTSUPP):
		return No
	}
	return Unknown
}

// linkModes returns the highest supported speed (ETHTOOL_GLINKSETTINGS with
// the size handshake); 0 = unknown.
func linkModes(linux string) int {
	le := binary.LittleEndian
	buf := make([]byte, linkSettingsHeaderLen+3*4*maxLinkModeWords)
	le.PutUint32(buf, ethtoolGLinkSettings)
	if ethtool(linux, buf) != nil {
		return 0
	}
	// The kernel answers the handshake with -nwords in
	// link_mode_masks_nwords (offset 15).
	nwords := -int(int8(buf[15]))
	if nwords <= 0 || nwords > maxLinkModeWords {
		return 0
	}
	for i := range buf {
		buf[i] = 0
	}
	le.PutUint32(buf, ethtoolGLinkSettings)
	buf[15] = byte(nwords)
	if ethtool(linux, buf) != nil {
		return 0
	}
	supported := buf[linkSettingsHeaderLen : linkSettingsHeaderLen+4*nwords]
	bit := func(n int) bool {
		w := n / 32
		return w < nwords && le.Uint32(supported[4*w:])&(1<<(n%32)) != 0
	}
	max := 0 // stays 0 when nothing is reported (e.g. virtio)
	for n := 0; n < 32*nwords; n++ {
		if s := linkModeSpeed[n]; bit(n) && s > max {
			max = s
		}
	}
	return max
}

// features reads the driver's feature flags (ETHTOOL_GFEATURES).
func features(linux string) map[string]string {
	le := binary.LittleEndian
	info := make([]byte, 16+4)
	le.PutUint32(info, ethtoolGSSetInfo)
	le.PutUint64(info[8:], 1<<ethSSFeatures)
	if ethtool(linux, info) != nil {
		return nil
	}
	n := int(le.Uint32(info[16:]))
	if n <= 0 || n > 1024 {
		return nil
	}
	strs := make([]byte, 12+n*ethGStringLen)
	le.PutUint32(strs, ethtoolGStrings)
	le.PutUint32(strs[4:], ethSSFeatures)
	le.PutUint32(strs[8:], uint32(n))
	if ethtool(linux, strs) != nil {
		return nil
	}
	blocks := (n + 31) / 32
	feat := make([]byte, 8+blocks*16)
	le.PutUint32(feat, ethtoolGFeatures)
	le.PutUint32(feat[4:], uint32(blocks))
	if ethtool(linux, feat) != nil {
		return nil
	}
	out := map[string]string{}
	for i := 0; i < n; i++ {
		name := strings.TrimRight(string(strs[12+i*ethGStringLen:12+(i+1)*ethGStringLen]), "\x00")
		blk := feat[8+(i/32)*16:]
		mask := uint32(1) << (i % 32)
		available, active := le.Uint32(blk[0:])&mask != 0, le.Uint32(blk[8:])&mask != 0
		switch {
		case active:
			out[name] = "on"
		case available:
			out[name] = "off"
		}
	}
	return out
}

// Rings are a NIC's receive and transmit ring sizes (ETHTOOL_GRINGPARAM).
type Rings struct {
	RX, RXMax, TX, TXMax int
}

// ReadRings reads a port's ring sizes; ok is false when the driver does
// not report them.
func ReadRings(linux string) (Rings, bool) {
	const ethtoolGRingParam = 0x10
	buf := make([]byte, 9*4)
	le := binary.LittleEndian
	le.PutUint32(buf, ethtoolGRingParam)
	if err := ethtool(linux, buf); err != nil {
		return Rings{}, false
	}
	u := func(i int) int { return int(le.Uint32(buf[4*i:])) }
	return Rings{RXMax: u(1), TXMax: u(4), RX: u(5), TX: u(8)}, true
}

const (
	ethtoolGModuleInfo   = 0x42
	ethtoolGModuleEEPROM = 0x43
)

// ReadOptics reads a port's transceiver (ErrNoModule: none, or the driver
// cannot read it).
func ReadOptics(linux string) (Optics, error) {
	info := make([]byte, 44) // cmd, type, eeprom_len, reserved[8]
	binary.LittleEndian.PutUint32(info, ethtoolGModuleInfo)
	if err := ethtool(linux, info); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EIO) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.EINVAL) {
			return Optics{}, ErrNoModule
		}
		return Optics{}, err
	}
	typ := int(binary.LittleEndian.Uint32(info[4:]))
	n := int(binary.LittleEndian.Uint32(info[8:]))
	if n <= 0 || n > 640 {
		return Optics{}, ErrNoModule
	}
	buf := make([]byte, 16+n) // cmd, magic, offset, len, data
	binary.LittleEndian.PutUint32(buf, ethtoolGModuleEEPROM)
	binary.LittleEndian.PutUint32(buf[12:], uint32(n))
	if err := ethtool(linux, buf); err != nil {
		return Optics{}, ErrNoModule
	}
	return ParseModule(typ, buf[16:])
}
