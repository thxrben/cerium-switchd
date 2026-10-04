package bgpd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/thxrben/cerium-switchd/pkg/bgp"
)

// Port is BGP's TCP port.
const Port = 179

// LinuxNet is Net on Linux: one dual-stack listener per instance bound to
// its VRF, TCP MD5 signatures (RFC 2385) per neighbour, TTL 1 for eBGP to
// a directly connected neighbour (the multihop TTL otherwise, 255 for
// iBGP).
type LinuxNet struct {
	// ListenPort overrides Port (tests).
	ListenPort int
}

func bindVRF(fd int, vrf string) error {
	if vrf == "" {
		return nil
	}
	return unix.BindToDevice(fd, vrf)
}

// md5Key sets or clears (key "") the TCP MD5 key for a neighbour on a
// socket of family v6 (IPv4 neighbours as IPv4-mapped addresses there).
func md5Key(fd int, v6 bool, peer netip.Addr, key string) error {
	if len(key) > unix.TCP_MD5SIG_MAXKEYLEN {
		return fmt.Errorf("authentication key longer than %d bytes", unix.TCP_MD5SIG_MAXKEYLEN)
	}
	sig := unix.TCPMD5Sig{Keylen: uint16(len(key))}
	copy(sig.Key[:], key)
	if v6 {
		sa := unix.RawSockaddrInet6{Family: unix.AF_INET6}
		a := peer
		if a.Is4() {
			a = netip.AddrFrom16(a.As16())
		}
		sa.Addr = a.As16()
		b := (*[unix.SizeofSockaddrInet6]byte)(unsafePointer(&sa))
		copy(sig.Addr.Data[:], b[2:])
		sig.Addr.Family = unix.AF_INET6
	} else {
		sa := unix.RawSockaddrInet4{Family: unix.AF_INET, Addr: peer.As4()}
		b := (*[unix.SizeofSockaddrInet4]byte)(unsafePointer(&sa))
		copy(sig.Addr.Data[:], b[2:])
		sig.Addr.Family = unix.AF_INET
	}
	return unix.SetsockoptTCPMD5Sig(fd, unix.IPPROTO_TCP, unix.TCP_MD5SIG, &sig)
}

func setTTL(fd int, ttl int) {
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, ttl)
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_UNICAST_HOPS, ttl)
}

// ttlOf is the TTL of a session's packets.
func ttlOf(n bgp.Neighbor) int {
	switch {
	case n.TTL > 0:
		return n.TTL
	case n.Internal:
		return 255
	}
	return 1
}

type linuxListener struct {
	l    *net.TCPListener
	mu   sync.Mutex
	keys map[netip.Addr]string
}

// Listen opens [::]:179 in the VRF.
func (ln LinuxNet) Listen(vrf string, keys map[netip.Addr]string) (Listener, error) {
	port := ln.ListenPort
	if port == 0 {
		port = Port
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			serr = bindVRF(int(fd), vrf)
		})
		return errors.Join(err, serr)
	}}
	// Plain TCP: Go may open listeners as Multipath TCP, which has no
	// TCP MD5 (and BGP neighbours speak TCP).
	lc.SetMultipathTCP(false)
	l, err := lc.Listen(context.Background(), "tcp", fmt.Sprintf("[::]:%d", port))
	if err != nil {
		return nil, fmt.Errorf("bgp: listen in %q: %w", vrf, err)
	}
	ll := &linuxListener{l: l.(*net.TCPListener), keys: map[netip.Addr]string{}}
	return ll, ll.SetKeys(keys)
}

func (l *linuxListener) SetKeys(keys map[netip.Addr]string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rc, err := l.l.SyscallConn()
	if err != nil {
		return err
	}
	var errs []error
	err = rc.Control(func(fd uintptr) {
		for a := range l.keys {
			if _, ok := keys[a]; !ok {
				errs = append(errs, md5Key(int(fd), true, a, ""))
			}
		}
		for a, k := range keys {
			if l.keys[a] != k {
				errs = append(errs, md5Key(int(fd), true, a, k))
			}
		}
	})
	l.keys = keys
	return errors.Join(append(errs, err)...)
}

func (l *linuxListener) Accept() (net.Conn, error) {
	c, err := l.l.AcceptTCP()
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (l *linuxListener) Close() error { return l.l.Close() }

// Dial connects to a neighbour from its local address in the VRF.
func (ln LinuxNet) Dial(ctx context.Context, vrf string, n bgp.Neighbor) (net.Conn, error) {
	port := ln.ListenPort
	if port == 0 {
		port = Port
	}
	v6 := n.Addr.Is6()
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			if serr = bindVRF(int(fd), vrf); serr != nil {
				return
			}
			if n.AuthKey != "" {
				if serr = md5Key(int(fd), v6, n.Addr, n.AuthKey); serr != nil {
					return
				}
			}
			setTTL(int(fd), ttlOf(n))
		})
		return errors.Join(err, serr)
	}}
	d.SetMultipathTCP(false)
	if n.LocalAddress.IsValid() {
		d.LocalAddr = &net.TCPAddr{IP: n.LocalAddress.AsSlice()}
	}
	return d.DialContext(ctx, "tcp", netip.AddrPortFrom(n.Addr, uint16(port)).String())
}

// SetAcceptedTTL sets the TTL of an accepted session (its neighbour's).
func SetAcceptedTTL(c net.Conn, n bgp.Neighbor) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	if rc, err := tc.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) { setTTL(int(fd), ttlOf(n)) })
	}
}

func unsafePointer[T any](p *T) unsafe.Pointer { return unsafe.Pointer(p) }
