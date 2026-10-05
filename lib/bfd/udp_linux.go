//go:build linux

package bfd

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// Ports (RFC 5881, 5883). Source ports are 49152-65535.
const (
	PortSingle   = 3784
	PortMultihop = 4784
	srcPortBase  = 49152
)

// UDP is the Linux transport: one listening socket per (instance, family,
// multihop), bound to the instance's VRF device, and one sending socket
// per session (fixed source port, TTL 255).
type UDP struct {
	// Input receives packets (the server's Input).
	Input func(Input)

	mu    sync.Mutex
	lis   map[string][]net.PacketConn // key: instance/multihop
	send  map[Key]*net.UDPConn
	nextP int
}

func (u *UDP) key(inst string, mh bool) string { return fmt.Sprintf("%s/%v", inst, mh) }

// control binds a socket to the VRF device and sets TTL 255 and receiving
// of the TTL.
func control(vrf string, v6 bool) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			if vrf != "" {
				if serr = unix.BindToDevice(int(fd), vrf); serr != nil {
					return
				}
			}
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			if v6 {
				_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_UNICAST_HOPS, 255)
				_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_RECVHOPLIMIT, 1)
			} else {
				_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TTL, 255)
				_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_RECVTTL, 1)
			}
		})
		if err != nil {
			return err
		}
		return serr
	}
}

// Open listens for the instance (IPv4 and IPv6).
func (u *UDP) Open(inst string, mh bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.lis == nil {
		u.lis, u.send = map[string][]net.PacketConn{}, map[Key]*net.UDPConn{}
	}
	k := u.key(inst, mh)
	if u.lis[k] != nil {
		return nil
	}
	port := PortSingle
	if mh {
		port = PortMultihop
	}
	var cs []net.PacketConn
	for _, fam := range []struct {
		net, addr string
		v6        bool
	}{{"udp4", "0.0.0.0", false}, {"udp6", "::", true}} {
		lc := net.ListenConfig{Control: control(inst, fam.v6)}
		c, err := lc.ListenPacket(context.Background(), fam.net, fmt.Sprintf("[%s]:%d", fam.addr, port))
		if err != nil {
			for _, o := range cs {
				o.Close()
			}
			return fmt.Errorf("listening on UDP %d (%s): %w", port, fam.net, err)
		}
		cs = append(cs, c)
		go u.read(c, inst, mh, fam.v6)
	}
	u.lis[k] = cs
	return nil
}

func (u *UDP) read(c net.PacketConn, inst string, mh, v6 bool) {
	buf := make([]byte, 1500)
	if v6 {
		pc := ipv6.NewPacketConn(c)
		_ = pc.SetControlMessage(ipv6.FlagHopLimit|ipv6.FlagDst, true)
		for {
			n, cm, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			in := Input{Instance: inst, Multihop: mh, Raw: append([]byte(nil), buf[:n]...)}
			if ua, ok := from.(*net.UDPAddr); ok {
				in.From = ua.AddrPort().Addr().Unmap()
			}
			if cm != nil {
				in.TTL = cm.HopLimit
				in.To, _ = netip.AddrFromSlice(cm.Dst)
			}
			u.Input(in)
		}
	}
	pc := ipv4.NewPacketConn(c)
	_ = pc.SetControlMessage(ipv4.FlagTTL|ipv4.FlagDst, true)
	for {
		n, cm, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		in := Input{Instance: inst, Multihop: mh, Raw: append([]byte(nil), buf[:n]...)}
		if ua, ok := from.(*net.UDPAddr); ok {
			in.From = ua.AddrPort().Addr().Unmap()
		}
		if cm != nil {
			in.TTL = cm.TTL
			if d, ok := netip.AddrFromSlice(cm.Dst); ok {
				in.To = d.Unmap()
			}
		}
		u.Input(in)
	}
}

// Close stops listening for the instance.
func (u *UDP) Close(inst string, mh bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	k := u.key(inst, mh)
	for _, c := range u.lis[k] {
		c.Close()
	}
	delete(u.lis, k)
	for sk, c := range u.send {
		if sk.Instance == inst && sk.Multihop == mh {
			c.Close()
			delete(u.send, sk)
		}
	}
}

// Send sends a packet for session k from a per-session source port.
func (u *UDP) Send(k Key, b []byte) error {
	u.mu.Lock()
	c := u.send[k]
	if c == nil {
		port := PortSingle
		if k.Multihop {
			port = PortMultihop
		}
		v6 := k.Peer.Is6()
		network := "udp4"
		if v6 {
			network = "udp6"
		}
		var err error
		for range 64 {
			u.nextP = (u.nextP + 1) % 16384
			src := &net.UDPAddr{Port: srcPortBase + u.nextP}
			if k.Local.IsValid() {
				src.IP = k.Local.AsSlice()
			}
			d := net.Dialer{LocalAddr: src, Control: control(k.Instance, v6)}
			var nc net.Conn
			nc, err = d.Dial(network, netip.AddrPortFrom(k.Peer, uint16(port)).String())
			if err == nil {
				c = nc.(*net.UDPConn)
				break
			}
		}
		if c == nil {
			u.mu.Unlock()
			return err
		}
		if u.send == nil {
			u.send = map[Key]*net.UDPConn{}
		}
		u.send[k] = c
	}
	u.mu.Unlock()
	_, err := c.Write(b)
	return err
}
