//go:build linux

package dataplane

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func vethPair(t *testing.T, a, b string) {
	t.Helper()
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: a, MTU: 1500}, PeerName: b}); err != nil {
		t.Skip("no veth:", err)
	}
	t.Cleanup(func() {
		if l, err := netlink.LinkByName(a); err == nil {
			netlink.LinkDel(l)
		}
	})
	for _, n := range []string{a, b} {
		l, _ := netlink.LinkByName(n)
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatal(err)
		}
	}
}

func packetSock(t *testing.T, dev string) int {
	t.Helper()
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		t.Skip("no AF_PACKET:", err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	l, _ := netlink.LinkByName(dev)
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: l.Attrs().Index}); err != nil {
		t.Fatal(err)
	}
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 200000})
	return fd
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// frame builds an Ethernet frame (vid > 0: 802.1Q tagged) with an IPv4
// packet of protocol proto and, for UDP/TCP, a destination port.
func frame(dst, src net.HardwareAddr, vid int, proto uint8, dport uint16) []byte {
	b := append([]byte{}, dst...)
	b = append(b, src...)
	if vid > 0 {
		b = binary.BigEndian.AppendUint16(b, 0x8100)
		b = binary.BigEndian.AppendUint16(b, uint16(vid))
	}
	b = binary.BigEndian.AppendUint16(b, 0x0800)
	ipb := []byte{0x45, 0xc0, 0, 48, 0, 1, 0, 0, 1, proto, 0, 0, 10, 0, 10, 2, 10, 0, 10, 1}
	b = append(b, ipb...)
	l4 := make([]byte, 28)
	binary.BigEndian.PutUint16(l4[0:], 49152)
	binary.BigEndian.PutUint16(l4[2:], dport)
	return append(b, l4...)
}

// The frames that arrive at the tunnel: exactly the client's, with the
// port VLAN's tag added to an untagged one; nothing else leaves that way.
func TestPuntFrames(t *testing.T) {
	vethPair(t, "puc", "pup") // client -> port
	vethPair(t, "put", "pum") // tunnel -> the master's end
	k := &Netlink{}
	gw := net.HardwareAddr{0x02, 0xce, 0, 0, 0, 1}
	client := net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}
	p := &Punt{Tunnel: "put", GatewayMAC: gw, Ports: []PuntPort{{Name: "pup", PVID: 10, VLANs: []int{20}}}}
	if _, err := k.SyncPunt(p); err != nil {
		t.Skip("tc flower/mirred/vlan not available:", err)
	}
	t.Cleanup(func() { k.SyncPunt(nil) })
	tx := packetSock(t, "puc")
	rx := packetSock(t, "pum")
	lc, _ := netlink.LinkByName("puc")
	send := func(f []byte) {
		if err := unix.Sendto(tx, f, 0, &unix.SockaddrLinklayer{Ifindex: lc.Attrs().Index, Halen: 6}); err != nil {
			t.Fatal(err)
		}
	}
	recvAll := func() [][]byte {
		var out [][]byte
		buf := make([]byte, 4096)
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			n, _, err := unix.Recvfrom(rx, buf, 0)
			if err != nil {
				continue
			}
			if n >= 14 && bytes.Equal(buf[6:12], client) {
				out = append(out, append([]byte(nil), buf[:n]...))
			}
		}
		return out
	}
	recvAll() // drain (IPv6 noise of new links)
	cases := []struct {
		name string
		in   []byte
		want []byte // nil: not redirected
	}{
		{"untagged OSPF: the port VLAN's tag added", frame(gw, client, 0, 89, 0), frame(gw, client, 10, 89, 0)},
		{"tagged BFD in an irb VLAN: unchanged", frame(gw, client, 20, 17, 3784), frame(gw, client, 20, 17, 3784)},
		{"untagged BGP: tag added", frame(gw, client, 0, 6, 179), frame(gw, client, 10, 6, 179)},
		{"a VLAN the port does not pass: not redirected", frame(gw, client, 30, 89, 0), nil},
		{"ICMP to the gateway: stays local", frame(gw, client, 0, 1, 0), nil},
		{"OSPF to another MAC: bridged as usual", frame(net.HardwareAddr{0x02, 1, 2, 3, 4, 5}, client, 0, 89, 0), nil},
	}
	for _, c := range cases {
		send(c.in)
		got := recvAll()
		switch {
		case c.want == nil && len(got) > 0:
			t.Errorf("%s: redirected % x", c.name, got[0])
		case c.want != nil && len(got) != 1:
			t.Errorf("%s: %d frames at the tunnel", c.name, len(got))
		case c.want != nil && !bytes.Equal(got[0], c.want):
			t.Errorf("%s:\n got % x\nwant % x", c.name, got[0], c.want)
		}
	}
	// Removed: nothing is redirected any more.
	if _, err := k.SyncPunt(nil); err != nil {
		t.Fatal(err)
	}
	send(frame(gw, client, 0, 89, 0))
	if got := recvAll(); len(got) > 0 {
		t.Errorf("still redirected after the removal")
	}
}
