//go:build linux

package stp

import (
	"errors"
	"net"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// LinuxGuardIO listens with a packet socket per port; a classic BPF
// filter in the kernel lets only frames to the BPDU addresses through
// (tagged PVST+ BPDUs included).
type LinuxGuardIO struct{}

// bpduFilter accepts frames to 01:80:c2:00:00:00 and 01:00:0c:cc:cc:cd.
var bpduFilter = []unix.SockFilter{
	/* 0 */ {Code: 0x20, K: 0}, // ld  [0]           (destination, bytes 0-3)
	/* 1 */ {Code: 0x15, Jt: 0, Jf: 2, K: 0x0180c200}, // jeq IEEE ? 2 : 4
	/* 2 */ {Code: 0x28, K: 4}, // ldh [4]           (bytes 4-5)
	/* 3 */ {Code: 0x15, Jt: 3, Jf: 4, K: 0x0000}, // jeq 0000 ? accept : drop
	/* 4 */ {Code: 0x15, Jt: 0, Jf: 3, K: 0x01000ccc}, // jeq Cisco ? 5 : drop
	/* 5 */ {Code: 0x28, K: 4}, // ldh [4]
	/* 6 */ {Code: 0x15, Jt: 0, Jf: 1, K: 0xcccd}, // jeq cccd ? accept : drop
	/* 7 */ {Code: 0x06, K: 0x40000}, // ret accept
	/* 8 */ {Code: 0x06, K: 0}, // ret drop
}

func (LinuxGuardIO) Listen(dev string, rx func(frame []byte)) (func(), error) {
	ifi, err := net.InterfaceByName(dev)
	if err != nil {
		return nil, err
	}
	proto := htons(unix.ETH_P_ALL)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return nil, err
	}
	prog := unix.SockFprog{Len: uint16(len(bpduFilter)), Filter: (*unix.SockFilter)(unsafe.Pointer(&bpduFilter[0]))}
	if err := unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &prog); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: proto, Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	tv := unix.NsecToTimeval(int64(500 * time.Millisecond))
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	var stopped atomic.Bool
	go func() {
		defer unix.Close(fd)
		buf := make([]byte, 2048)
		for !stopped.Load() {
			n, from, err := unix.Recvfrom(fd, buf, 0)
			if err != nil {
				if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
					continue
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			// Only frames that arrived on the port (not the ones sent).
			if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
				continue
			}
			rx(append([]byte(nil), buf[:n]...))
		}
	}()
	return func() { stopped.Store(true) }, nil
}
