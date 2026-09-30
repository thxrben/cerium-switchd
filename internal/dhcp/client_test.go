package dhcp

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

type server struct {
	t       *testing.T
	addr    netip.Addr
	lease   uint32
	nak     bool
	silent  bool
	seen    []byte // message types received
	unicast []bool
}

func (s *server) handle(c *Client, outs []Out, now time.Time) {
	for _, o := range outs {
		p, err := Unmarshal(o.Pkt.Marshal())
		if err != nil {
			s.t.Fatal(err)
		}
		s.seen = append(s.seen, p.Type())
		s.unicast = append(s.unicast, o.Unicast.IsValid())
		if s.silent {
			continue
		}
		reply := &Packet{Op: 2, XID: p.XID, CHAddr: p.CHAddr, YIAddr: netip.MustParseAddr("10.1.2.50"), Options: map[byte][]byte{
			optServerID:   s.addr.AsSlice(),
			optSubnetMask: {255, 255, 255, 0},
			optRouter:     {10, 1, 2, 1},
			optDNS:        {10, 1, 2, 53, 9, 9, 9, 9},
			optLeaseTime:  binary.BigEndian.AppendUint32(nil, s.lease),
		}}
		switch p.Type() {
		case Discover:
			reply.Options[optMsgType] = []byte{Offer}
		case Request:
			reply.Options[optMsgType] = []byte{Ack}
			if s.nak {
				s.nak = false // once
				reply.Options[optMsgType] = []byte{Nak}
			}
		default:
			continue
		}
		r, _ := Unmarshal(reply.Marshal())
		s.handle(c, c.Receive(r, now), now)
	}
}

func newClient() (*Client, *[]*Lease) {
	var events []*Lease
	n := uint32(7)
	c := &Client{MAC: [6]byte{2, 0, 0, 0, 0, 1}, HostName: "sw1", Rand: func() uint32 { n = n*1103515245 + 12345; return n },
		OnLease: func(l *Lease) { events = append(events, l) }}
	return c, &events
}

func TestLeaseLifecycle(t *testing.T) {
	c, ev := newClient()
	s := &server{t: t, addr: netip.MustParseAddr("10.1.2.1"), lease: 3600}
	now := time.Unix(1000, 0)
	s.handle(c, c.Start(now), now)
	st, l := c.State()
	if st != Bound || l == nil || l.Addr != netip.MustParsePrefix("10.1.2.50/24") || l.Router != netip.MustParseAddr("10.1.2.1") || len(l.DNS) != 2 {
		t.Fatalf("after the exchange: %v %+v", st, l)
	}
	if len(*ev) != 1 || l.T1 != 1800*time.Second || l.T2 != 3150*time.Second {
		t.Fatalf("events %d, T1 %v T2 %v", len(*ev), l.T1, l.T2)
	}
	// Renewal at T1, unicast to the server; the same lease: no event.
	s.seen, s.unicast = nil, nil
	now = now.Add(1800 * time.Second)
	s.handle(c, c.Tick(now), now)
	if len(s.seen) != 1 || s.seen[0] != Request || !s.unicast[0] || len(*ev) != 1 {
		t.Errorf("renewal: %v unicast %v events %d", s.seen, s.unicast, len(*ev))
	}
	// The server goes silent: rebinding by broadcast at T2, then expiry.
	s.silent = true
	s.seen, s.unicast = nil, nil
	for now2 := now.Add(time.Second); now2.Before(now.Add(3601 * time.Second)); now2 = now2.Add(time.Second) {
		s.handle(c, c.Tick(now2), now2)
	}
	sawRebind := false
	for i, typ := range s.seen {
		if typ == Request && !s.unicast[i] {
			sawRebind = true
		}
	}
	if !sawRebind {
		t.Errorf("no rebinding broadcast: %v %v", s.seen, s.unicast)
	}
	if st, l := c.State(); st != Selecting || l != nil || len(*ev) != 2 || (*ev)[1] != nil {
		t.Errorf("after expiry: %v %v events %v", st, l, *ev)
	}
}

func TestNakRestarts(t *testing.T) {
	c, ev := newClient()
	s := &server{t: t, addr: netip.MustParseAddr("10.1.2.1"), lease: 600, nak: true}
	now := time.Unix(1000, 0)
	s.handle(c, c.Start(now)[:1], now)
	// Offer, request, NAK: the client starts over and gets the lease.
	if st, _ := c.State(); st != Bound || len(*ev) != 1 || len(s.seen) != 4 || s.seen[2] != Discover {
		t.Errorf("after a NAK: %v, messages %v", st, s.seen)
	}
}

func TestRetransmitBackoff(t *testing.T) {
	c, _ := newClient()
	now := time.Unix(1000, 0)
	c.Start(now)
	var gaps []time.Duration
	last := now
	for i := 0; i < 200; i++ {
		now = now.Add(time.Second)
		if outs := c.Tick(now); len(outs) > 0 {
			gaps = append(gaps, now.Sub(last))
			last = now
		}
	}
	if len(gaps) < 4 || gaps[0] > 6*time.Second || gaps[3] < 25*time.Second {
		t.Errorf("retransmissions %v", gaps)
	}
}

func TestRelease(t *testing.T) {
	c, ev := newClient()
	s := &server{t: t, addr: netip.MustParseAddr("10.1.2.1"), lease: 600}
	now := time.Unix(1000, 0)
	s.handle(c, c.Start(now), now)
	outs := c.Release(now)
	if len(outs) != 1 || outs[0].Pkt.Type() != Release || outs[0].Unicast != s.addr || len(*ev) != 2 {
		t.Errorf("release: %+v events %v", outs, *ev)
	}
}
