//go:build linux

package ospfd

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/thxrben/cerium-switchd/lib/ospf"
	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
	"github.com/thxrben/cerium-switchd/lib/sys/nlx"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// LinuxKernel reads devices through netlink and sysfs.
type LinuxKernel struct{ SysRoot string }

func (k LinuxKernel) sys() string {
	if k.SysRoot == "" {
		return "/sys"
	}
	return k.SysRoot
}

func (k LinuxKernel) Link(dev string) (LinkInfo, bool) {
	l, err := nlx.LinkByName(dev)
	if err != nil {
		return LinkInfo{}, false
	}
	a := l.Attrs()
	li := LinkInfo{Index: a.Index, MTU: a.MTU,
		Up: a.Flags&net.FlagUp != 0 && (a.OperState == netlink.OperUp || a.OperState == netlink.OperUnknown)}
	if addrs, err := nlx.AddrList(l, unix.AF_INET6); err == nil {
		for _, ad := range addrs {
			ip, ok := netip.AddrFromSlice(ad.IP)
			if ok && ip.IsLinkLocalUnicast() && ad.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
				li.LinkLocal = ip
				break
			}
		}
	}
	li.SpeedMbps = k.speed(dev, 0)
	return li, true
}

// speed is the device's speed in Mbit/s: its own, else from the devices
// below it (a bundle: their sum; a bridge, VLAN or irb: the fastest).
func (k LinuxKernel) speed(dev string, depth int) uint64 {
	if depth > 4 {
		return 0
	}
	base := filepath.Join(k.sys(), "class/net", dev)
	if raw, err := hwio.ReadFile(filepath.Join(base, "speed")); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil && v > 0 {
			return uint64(v)
		}
	}
	var lowers []string
	if ents, err := hwio.ReadDir(filepath.Join(base, "brif")); err == nil {
		for _, e := range ents {
			lowers = append(lowers, e.Name())
		}
	} else if ents, err := hwio.ReadDir(base); err == nil {
		for _, e := range ents {
			if n, ok := strings.CutPrefix(e.Name(), "lower_"); ok {
				lowers = append(lowers, n)
			}
		}
	}
	sum := false
	if _, err := hwio.Stat(filepath.Join(base, "bonding")); err == nil {
		sum = true
	} else if l, err := nlx.LinkByName(dev); err == nil && l.Type() == "team" {
		sum = true
	}
	var total, best uint64
	for _, lo := range lowers {
		s := k.speed(lo, depth+1)
		total += s
		best = max(best, s)
	}
	if sum {
		return total
	}
	return best
}

// LinuxNet opens raw IP sockets (protocol 89), one per interface, bound
// to the device: OSPFv2 with TTL 1 to 224.0.0.5/6, OSPFv3 with hop limit 1
// to ff02::5/6 and the checksum computed by the kernel.
type LinuxNet struct{}

type rawPort struct {
	fd      int
	v       ospf.Version
	ifindex int
	closed  atomic.Bool
	wg      sync.WaitGroup
}

func setInt(fd, level, opt, v int) error { return unix.SetsockoptInt(fd, level, opt, v) }

func (LinuxNet) Open(v ospf.Version, dev string, ifindex int, rx func(src, dst netip.Addr, pkt []byte)) (Port, error) {
	fam := unix.AF_INET
	if v == ospf.V3 {
		fam = unix.AF_INET6
	}
	fd, err := unix.Socket(fam, unix.SOCK_RAW|unix.SOCK_CLOEXEC, ospf.ProtoOSPF)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (Port, error) { unix.Close(fd); return nil, err }
	if err := unix.BindToDevice(fd, dev); err != nil {
		return fail(err)
	}
	// Reads wake up every second so Close is noticed.
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 1}); err != nil {
		return fail(err)
	}
	_ = setInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)
	_ = setInt(fd, unix.SOL_SOCKET, unix.SO_PRIORITY, 6) // internetwork control
	if v == ospf.V2 {
		for _, o := range [][2]int{{unix.IP_MULTICAST_TTL, 1}, {unix.IP_TTL, 1}, {unix.IP_MULTICAST_LOOP, 0},
			{unix.IP_TOS, 0xc0}, {unix.IP_PKTINFO, 1}} {
			if err := setInt(fd, unix.IPPROTO_IP, o[0], o[1]); err != nil {
				return fail(err)
			}
		}
		if err := unix.SetsockoptIPMreqn(fd, unix.IPPROTO_IP, unix.IP_MULTICAST_IF, &unix.IPMreqn{Ifindex: int32(ifindex)}); err != nil {
			return fail(err)
		}
		for _, g := range []netip.Addr{ospf.AllSPFRouters, ospf.AllDRouters} {
			m := &unix.IPMreqn{Multiaddr: g.As4(), Ifindex: int32(ifindex)}
			if err := unix.SetsockoptIPMreqn(fd, unix.IPPROTO_IP, unix.IP_ADD_MEMBERSHIP, m); err != nil {
				return fail(err)
			}
		}
	} else {
		if err := setInt(fd, unix.IPPROTO_IPV6, unix.IPV6_CHECKSUM, ospf.IPv6ChecksumOffset()); err != nil {
			return fail(err)
		}
		for _, o := range [][2]int{{unix.IPV6_MULTICAST_HOPS, 1}, {unix.IPV6_UNICAST_HOPS, 1}, {unix.IPV6_MULTICAST_LOOP, 0},
			{unix.IPV6_TCLASS, 0xc0}, {unix.IPV6_RECVPKTINFO, 1}, {unix.IPV6_MULTICAST_IF, ifindex}} {
			if err := setInt(fd, unix.IPPROTO_IPV6, o[0], o[1]); err != nil {
				return fail(err)
			}
		}
		for _, g := range []netip.Addr{ospf.AllSPFRouters6, ospf.AllDRouters6} {
			m := &unix.IPv6Mreq{Multiaddr: g.As16(), Interface: uint32(ifindex)}
			if err := unix.SetsockoptIPv6Mreq(fd, unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, m); err != nil {
				return fail(err)
			}
		}
	}
	p := &rawPort{fd: fd, v: v, ifindex: ifindex}
	p.wg.Add(1)
	go p.read(rx)
	return p, nil
}

func (p *rawPort) read(rx func(src, dst netip.Addr, pkt []byte)) {
	defer p.wg.Done()
	buf := make([]byte, 65536)
	oob := make([]byte, 256)
	for !p.closed.Load() {
		n, oobn, _, from, err := unix.Recvmsg(p.fd, buf, oob, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			if p.closed.Load() {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		var src, dst netip.Addr
		pkt := buf[:n]
		switch sa := from.(type) {
		case *unix.SockaddrInet4:
			src = netip.AddrFrom4(sa.Addr)
		case *unix.SockaddrInet6:
			src = netip.AddrFrom16(sa.Addr)
		}
		if p.v == ospf.V2 {
			// A raw IPv4 socket delivers the IP header.
			if len(pkt) < 20 {
				continue
			}
			hl := int(pkt[0]&0x0f) * 4
			if hl < 20 || len(pkt) < hl {
				continue
			}
			dst = netip.AddrFrom4([4]byte(pkt[16:20]))
			pkt = pkt[hl:]
		} else {
			dst = pktinfoDst6(oob[:oobn])
		}
		rx(src, dst, append([]byte(nil), pkt...))
	}
}

// pktinfoDst6 reads the destination of an IPv6 packet from IPV6_PKTINFO.
func pktinfoDst6(oob []byte) netip.Addr {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return netip.Addr{}
	}
	for _, m := range msgs {
		if m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_PKTINFO && len(m.Data) >= 16 {
			return netip.AddrFrom16([16]byte(m.Data[:16]))
		}
	}
	return netip.Addr{}
}

// Send sends a packet from src (the interface's address) without waiting
// (a full socket buffer drops it; OSPF retransmits).
func (p *rawPort) Send(src, dst netip.Addr, pkt []byte) error {
	if p.closed.Load() {
		return os.ErrClosed
	}
	var oob []byte
	var to unix.Sockaddr
	if p.v == ospf.V2 {
		info := unix.Inet4Pktinfo{Ifindex: int32(p.ifindex), Spec_dst: src.As4()}
		oob = cmsg(unix.IPPROTO_IP, unix.IP_PKTINFO, unsafe.Slice((*byte)(unsafe.Pointer(&info)), unix.SizeofInet4Pktinfo))
		to = &unix.SockaddrInet4{Addr: dst.As4()}
	} else {
		info := unix.Inet6Pktinfo{Addr: src.As16(), Ifindex: uint32(p.ifindex)}
		oob = cmsg(unix.IPPROTO_IPV6, unix.IPV6_PKTINFO, unsafe.Slice((*byte)(unsafe.Pointer(&info)), unix.SizeofInet6Pktinfo))
		to = &unix.SockaddrInet6{Addr: dst.As16(), ZoneId: uint32(p.ifindex)}
	}
	return unix.Sendmsg(p.fd, pkt, oob, to, unix.MSG_DONTWAIT)
}

func cmsg(level, typ int, data []byte) []byte {
	b := make([]byte, unix.CmsgSpace(len(data)))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level, h.Type = int32(level), int32(typ)
	h.SetLen(unix.CmsgLen(len(data)))
	copy(b[unix.CmsgLen(0):], data)
	return b
}

func (p *rawPort) Close() error {
	if p.closed.Swap(true) {
		return nil
	}
	// The reader wakes up within a second (SO_RCVTIMEO); the socket closes
	// after it, without holding up the caller.
	go func() {
		p.wg.Wait()
		unix.Close(p.fd)
	}()
	return nil
}
