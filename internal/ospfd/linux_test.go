//go:build linux

package ospfd

import (
	"context"
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/ribd"
	"github.com/thxrben/cerium-switchd/pkg/ospf"
	"github.com/thxrben/cerium-switchd/pkg/rib"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// nsKernel is LinuxKernel inside a network namespace.
type nsKernel struct{ h *netlink.Handle }

func (k nsKernel) Link(dev string) (LinkInfo, bool) {
	l, err := k.h.LinkByName(dev)
	if err != nil {
		return LinkInfo{}, false
	}
	a := l.Attrs()
	li := LinkInfo{Index: a.Index, MTU: a.MTU, Up: a.Flags&net.FlagUp != 0, SpeedMbps: 10000}
	addrs, _ := k.h.AddrList(l, unix.AF_INET6)
	for _, ad := range addrs {
		if ip, ok := netip.AddrFromSlice(ad.IP); ok && ip.IsLinkLocalUnicast() && ad.Flags&unix.IFA_F_TENTATIVE == 0 {
			li.LinkLocal = ip
		}
	}
	return li, true
}

// nsNet opens LinuxNet sockets inside a namespace (a socket stays in the
// namespace it was created in).
type nsNet struct{ ns netns.NsHandle }

func (n nsNet) Open(v ospf.Version, dev string, ifindex int, rx func(src, dst netip.Addr, pkt []byte)) (Port, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		return nil, err
	}
	defer orig.Close()
	if err := netns.Set(n.ns); err != nil {
		return nil, err
	}
	defer netns.Set(orig)
	return LinuxNet{}.Open(v, dev, ifindex, rx)
}

func newNS(t *testing.T) (netns.NsHandle, *netlink.Handle) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Skip("no network namespaces:", err)
	}
	defer orig.Close()
	ns, err := netns.New()
	if err != nil {
		t.Skip("no network namespaces:", err)
	}
	netns.Set(orig)
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(); ns.Close() })
	return ns, h
}

func addr(t *testing.T, h *netlink.Handle, dev, a string, flags int) error {
	t.Helper()
	l, err := h.LinkByName(dev)
	if err != nil {
		t.Fatal(err)
	}
	p, err := netlink.ParseAddr(a)
	if err != nil {
		t.Fatal(err)
	}
	p.Flags = flags
	return h.AddrAdd(l, p)
}

// Two namespaces joined by a veth pair, OSPF and OSPFv3 over real raw
// sockets: the adjacencies come up and each side learns the other's
// loopback with the right gateway (IPv4 address, IPv6 link-local).
func TestLinuxSockets(t *testing.T) {
	nsA, hA := newNS(t)
	nsB, hB := newNS(t)
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "ospfta"}, PeerName: "ospftb"}); err != nil {
		t.Skip("no veth:", err)
	}
	a, _ := netlink.LinkByName("ospfta")
	b, _ := netlink.LinkByName("ospftb")
	if err := netlink.LinkSetNsFd(a, int(nsA)); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetNsFd(b, int(nsB)); err != nil {
		t.Fatal(err)
	}
	ipv6 := true
	setup := func(h *netlink.Handle, dev, v4, ll, v6, lo4, lo6 string) {
		for _, x := range [][2]string{{dev, v4}, {"lo", lo4}} {
			if err := addr(t, h, x[0], x[1], 0); err != nil {
				t.Fatalf("%s %s: %v", x[0], x[1], err)
			}
		}
		// IPv6 may be off (some sandboxes): then OSPFv2 only.
		for _, x := range [][2]string{{dev, ll}, {dev, v6}, {"lo", lo6}} {
			if err := addr(t, h, x[0], x[1], unix.IFA_F_NODAD); err != nil {
				t.Logf("no IPv6 (%s %s: %v): OSPFv2 only", x[0], x[1], err)
				ipv6 = false
				break
			}
		}
		for _, d := range []string{dev, "lo"} {
			l, _ := h.LinkByName(d)
			if err := h.LinkSetUp(l); err != nil {
				t.Fatal(err)
			}
		}
	}
	setup(hA, "ospfta", "10.99.0.1/30", "fe80::a/64", "2001:db8:99::1/64", "192.0.2.1/32", "2001:db8:ff::1/128")
	setup(hB, "ospftb", "10.99.0.2/30", "fe80::b/64", "2001:db8:99::2/64", "192.0.2.2/32", "2001:db8:ff::2/128")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mk := func(ns netns.NsHandle, h *netlink.Handle, dev string, rid ospf.ID, p4, p6, lo4, lo6 string) *fakeRIB {
		r := &fakeRIB{sets: map[rib.Protocol]ribd.SetRoutes{}}
		d := New(nsKernel{h}, nsNet{ns}, r, quiet)
		d.Settle = time.Second
		link := iface("1/0/1.0", dev, p4, p6)
		lo := iface("lo0.0", "lo", lo4, lo6)
		lo.Passive = true
		insts := []Instance{{Version: ospf.V2, RouterID: rid, ReferenceBW: 100e9, Interfaces: []Iface{link, lo}}}
		if ipv6 {
			insts = append(insts, Instance{Version: ospf.V3, RouterID: rid, ReferenceBW: 100e9, Interfaces: []Iface{link, lo}})
		}
		d.SetConfig(Config{Instances: insts})
		d.SetMaster(true)
		go d.Run(ctx)
		return r
	}
	rA := mk(nsA, hA, "ospfta", 0x01010101, "10.99.0.1/30", "2001:db8:99::1/64", "192.0.2.1/32", "2001:db8:ff::1/128")
	mk(nsB, hB, "ospftb", 0x02020202, "10.99.0.2/30", "2001:db8:99::2/64", "192.0.2.2/32", "2001:db8:ff::2/128")
	deadline := time.Now().Add(15 * time.Second)
	for {
		s4, ok4 := rA.get(rib.OSPF)
		s6, ok6 := rA.get(rib.OSPF3)
		if ok4 && len(s4.Routes) == 1 && (!ipv6 || ok6 && len(s6.Routes) == 1) {
			if rt := s4.Routes[0]; rt.Prefix != netip.MustParsePrefix("192.0.2.2/32") || rt.NextHops[0].Gateway != netip.MustParseAddr("10.99.0.2") {
				t.Fatalf("OSPF route %+v", rt)
			}
			if !ipv6 {
				return
			}
			if rt := s6.Routes[0]; rt.Prefix != netip.MustParsePrefix("2001:db8:ff::2/128") || rt.NextHops[0].Gateway != netip.MustParseAddr("fe80::b") {
				t.Fatalf("OSPFv3 route %+v", rt)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no routes over the sockets: v2 %+v\nv3 %+v", s4, s6)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
