//go:build linux

package dataplane

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ethtoolGPauseParam = 0x12
	ethtoolSPauseParam = 0x13
)

type ethtoolPause struct {
	Cmd, Autoneg, RxPause, TxPause uint32
}

type ifreqData struct {
	Name [unix.IFNAMSIZ]byte
	Data uintptr
	_    [16]byte // pad to sizeof(struct ifreq)
}

func ethtoolPauseIoctl(name string, p *ethtoolPause) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var ifr ifreqData
	copy(ifr.Name[:], name)
	ifr.Data = uintptr(unsafe.Pointer(p))
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCETHTOOL, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return errno
	}
	return nil
}

// flowControl caches ports whose driver does not support pause settings,
// so they are reported as configured and not retried on every reconcile.
type flowControl struct {
	mu          sync.Mutex
	unsupported map[string]bool // port -> requested value was not applicable
	requested   map[string]bool
}

var pause = &flowControl{unsupported: map[string]bool{}, requested: map[string]bool{}}

// readFlowControl returns the pause setting (rx and tx both on), or nil if
// unknown.
func readFlowControl(name string) *bool {
	pause.mu.Lock()
	if pause.unsupported[name] {
		v := pause.requested[name]
		pause.mu.Unlock()
		return &v
	}
	pause.mu.Unlock()
	p := ethtoolPause{Cmd: ethtoolGPauseParam}
	if err := ethtoolPauseIoctl(name, &p); err != nil {
		return nil
	}
	v := p.RxPause != 0 && p.TxPause != 0
	return &v
}

func setFlowControl(name string, on bool) error {
	v := uint32(0)
	if on {
		v = 1
	}
	p := ethtoolPause{Cmd: ethtoolSPauseParam, RxPause: v, TxPause: v}
	err := ethtoolPauseIoctl(name, &p)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
		pause.mu.Lock()
		pause.unsupported[name], pause.requested[name] = true, on
		pause.mu.Unlock()
		return fmt.Errorf("%s: the driver does not support flow-control settings: %w", name, ErrUnsupported)
	}
	return err
}
