//go:build linux

package stp

import (
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The real listener on a veth: a BPDU sent at the far end is seen, an LLDP
// frame is not (the kernel filter).
func TestLinuxGuardIO(t *testing.T) {
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "bpg0"}, PeerName: "bpg1"}); err != nil {
		t.Skip("no veth:", err)
	}
	defer func() {
		if l, err := netlink.LinkByName("bpg0"); err == nil {
			netlink.LinkDel(l)
		}
	}()
	for _, n := range []string{"bpg0", "bpg1"} {
		l, _ := netlink.LinkByName(n)
		netlink.LinkSetUp(l)
	}
	got := make(chan []byte, 10)
	stop, err := LinuxGuardIO{}.Listen("bpg0", func(f []byte) { got <- f })
	if err != nil {
		t.Skip("no packet sockets:", err)
	}
	defer stop()
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		t.Skip(err)
	}
	defer unix.Close(fd)
	peer, _ := netlink.LinkByName("bpg1")
	send := func(dst []byte) {
		f := append(append([]byte{}, dst...), 0x02, 0xaa, 0, 0, 0, 1, 0, 0x26)
		f = append(f, make([]byte, 46)...)
		unix.Sendto(fd, f, 0, &unix.SockaddrLinklayer{Ifindex: peer.Attrs().Index, Halen: 6})
	}
	time.Sleep(100 * time.Millisecond)
	send([]byte{0x01, 0x80, 0xc2, 0, 0, 0x0e}) // LLDP
	send([]byte{0x01, 0x80, 0xc2, 0, 0, 0})    // BPDU
	select {
	case f := <-got:
		if !IsBPDU(f) {
			t.Fatalf("not a BPDU: % x", f[:6])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BPDU not seen")
	}
	select {
	case f := <-got:
		t.Fatalf("a second frame came through the filter: % x", f[:6])
	case <-time.After(300 * time.Millisecond):
	}
}
