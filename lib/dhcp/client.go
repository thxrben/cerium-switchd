package dhcp

import (
	"encoding/binary"
	"net/netip"
	"slices"
	"time"
)

// State is the client state (RFC 2131 figure 5).
type State int

const (
	Init State = iota
	Selecting
	Requesting
	Bound
	Renewing
	Rebinding
)

func (s State) String() string {
	return [...]string{"init", "selecting", "requesting", "bound", "renewing", "rebinding"}[s]
}

// Lease is an address the client holds.
type Lease struct {
	Addr     netip.Prefix
	Router   netip.Addr // invalid: none
	DNS      []netip.Addr
	Domain   string
	Server   netip.Addr
	Time     time.Duration
	T1, T2   time.Duration
	Acquired time.Time
}

// Expires returns the end of the lease.
func (l *Lease) Expires() time.Time { return l.Acquired.Add(l.Time) }

func (l *Lease) same(o *Lease) bool {
	return o != nil && l.Addr == o.Addr && l.Router == o.Router && slices.Equal(l.DNS, o.DNS) && l.Domain == o.Domain
}

// Out is a message to send; Unicast is the server for renewals (invalid:
// broadcast).
type Out struct {
	Pkt     *Packet
	Unicast netip.Addr
}

// Retransmission (RFC 2131 4.1): 4 s doubling up to 64 s.
const (
	firstRetry = 4 * time.Second
	maxRetry   = 64 * time.Second
	// requestTries: REQUESTs after an OFFER before starting over.
	requestTries = 4
)

// Client is one interface's DHCP client, as pure logic: the caller sends
// what the methods return and calls Tick at least every second.
type Client struct {
	MAC      [6]byte
	HostName string
	// Rand returns random numbers (transaction ids, jitter).
	Rand func() uint32
	// OnLease is called when a lease is obtained or changes (nil: the
	// lease is gone).
	OnLease func(*Lease)

	state   State
	xid     uint32
	started time.Time // first message of this exchange (secs field)
	next    time.Time // retransmission
	retry   time.Duration
	tries   int
	offer   *Packet
	lease   *Lease
}

// State reports the state and the lease.
func (c *Client) State() (State, *Lease) { return c.state, c.lease }

// Next reports when Tick has something to do.
func (c *Client) Next() time.Time { return c.next }

func (c *Client) base(t byte, now time.Time) *Packet {
	secs := uint16(0)
	if !c.started.IsZero() {
		secs = uint16(min(now.Sub(c.started)/time.Second, 65535))
	}
	cid := append([]byte{1}, c.MAC[:]...)
	p := &Packet{Op: 1, XID: c.xid, Secs: secs, CHAddr: c.MAC, Options: map[byte][]byte{
		optMsgType:  {t},
		optClientID: cid,
		optParams:   {optSubnetMask, optRouter, optDNS, optDomainName, optLeaseTime, optRenewal, optRebinding},
	}}
	if c.HostName != "" {
		p.Options[optHostName] = []byte(c.HostName)
	}
	return p
}

func (c *Client) newXID(now time.Time) {
	c.xid = c.Rand()
	c.started = now
	c.retry = firstRetry
	c.tries = 0
}

func (c *Client) schedule(now time.Time) {
	j := time.Duration(c.Rand()%2000) * time.Millisecond // ±1 s
	c.next = now.Add(c.retry + j - time.Second)
	c.retry = min(c.retry*2, maxRetry)
}

// Resume continues with a lease held before (the daemon restarted): bound
// until T1, then renewing as usual. An expired lease is not resumed
// (false: call Start).
func (c *Client) Resume(l *Lease, now time.Time) bool {
	if l == nil || !now.Before(l.Expires()) {
		return false
	}
	cp := *l
	c.lease, c.state, c.offer = &cp, Bound, nil
	c.next = l.Acquired.Add(l.T1)
	if c.next.Before(now) {
		c.next = now
	}
	return true
}

// Start begins (or restarts) with a DISCOVER.
func (c *Client) Start(now time.Time) []Out {
	c.newXID(now)
	c.state, c.offer = Selecting, nil
	return c.discover(now)
}

func (c *Client) discover(now time.Time) []Out {
	p := c.base(Discover, now)
	p.Broadcast = true
	if c.lease != nil {
		p.Options[optRequested] = c.lease.Addr.Addr().AsSlice()
	}
	c.schedule(now)
	return []Out{{Pkt: p}}
}

func (c *Client) request(now time.Time) []Out {
	p := c.base(Request, now)
	switch c.state {
	case Requesting:
		p.Broadcast = true
		p.Options[optRequested] = c.offer.YIAddr.AsSlice()
		p.Options[optServerID] = c.offer.Options[optServerID]
		c.schedule(now)
		return []Out{{Pkt: p}}
	case Renewing, Rebinding:
		p.CIAddr = c.lease.Addr.Addr()
		// Retransmit at half the time left to T2 (renewing) or expiry
		// (rebinding), at least 60 s (RFC 2131 4.4.5).
		end := c.lease.Acquired.Add(c.lease.T2)
		if c.state == Rebinding {
			end = c.lease.Expires()
		}
		c.next = now.Add(max(end.Sub(now)/2, 60*time.Second))
		if c.next.After(end) {
			c.next = end
		}
		if c.state == Renewing {
			return []Out{{Pkt: p, Unicast: c.lease.Server}}
		}
		return []Out{{Pkt: p}}
	}
	return nil
}

// Receive handles a message from a server.
func (c *Client) Receive(p *Packet, now time.Time) []Out {
	if p.Op != 2 || p.XID != c.xid || p.CHAddr != c.MAC {
		return nil
	}
	switch t := p.Type(); {
	case c.state == Selecting && t == Offer && p.YIAddr.IsValid() && p.addrOpt(optServerID).IsValid():
		c.offer, c.state = p, Requesting
		c.retry, c.tries = firstRetry, 1
		return c.request(now)
	case (c.state == Requesting || c.state == Renewing || c.state == Rebinding) && t == Ack:
		l := c.parse(p, now)
		if l == nil {
			return nil
		}
		changed := !l.same(c.lease)
		c.lease, c.state, c.offer = l, Bound, nil
		c.next = now.Add(l.T1)
		if changed && c.OnLease != nil {
			c.OnLease(l)
		}
		return nil
	case (c.state == Requesting || c.state == Renewing || c.state == Rebinding) && t == Nak:
		c.drop()
		return c.Start(now)
	}
	return nil
}

func (c *Client) parse(p *Packet, now time.Time) *Lease {
	lt := p.secsOpt(optLeaseTime)
	if lt <= 0 || !p.YIAddr.Is4() {
		return nil
	}
	bits := 24
	if m := p.Options[optSubnetMask]; len(m) == 4 {
		v := binary.BigEndian.Uint32(m)
		ones := 0
		for v&0x80000000 != 0 {
			ones++
			v <<= 1
		}
		if v == 0 && ones > 0 {
			bits = ones
		}
	}
	l := &Lease{Addr: netip.PrefixFrom(p.YIAddr, bits), Time: lt, Acquired: now,
		T1: p.secsOpt(optRenewal), T2: p.secsOpt(optRebinding), Server: p.addrOpt(optServerID), DNS: p.addrsOpt(optDNS),
		Domain: string(p.Options[optDomainName])}
	if !l.Server.IsValid() && c.lease != nil {
		l.Server = c.lease.Server
	}
	if r := p.addrsOpt(optRouter); len(r) > 0 {
		l.Router = r[0]
	}
	if l.T1 <= 0 || l.T1 >= lt {
		l.T1 = lt / 2
	}
	if l.T2 <= l.T1 || l.T2 >= lt {
		l.T2 = lt * 7 / 8
	}
	return l
}

func (c *Client) drop() {
	if c.lease != nil {
		c.lease = nil
		if c.OnLease != nil {
			c.OnLease(nil)
		}
	}
}

// Tick runs the timers.
func (c *Client) Tick(now time.Time) []Out {
	if c.next.IsZero() || now.Before(c.next) {
		return nil
	}
	switch c.state {
	case Init:
		return c.Start(now)
	case Selecting:
		return c.discover(now)
	case Requesting:
		c.tries++
		if c.tries > requestTries {
			return c.Start(now)
		}
		return c.request(now)
	case Bound, Renewing, Rebinding:
		switch {
		case !now.Before(c.lease.Expires()):
			c.drop()
			return c.Start(now)
		case !now.Before(c.lease.Acquired.Add(c.lease.T2)):
			if c.state != Rebinding {
				c.state = Rebinding
			}
		case c.state == Bound:
			c.state = Renewing
			c.newXID(now)
		}
		return c.request(now)
	}
	return nil
}

// Release gives the lease back (the statement was removed).
func (c *Client) Release(now time.Time) []Out {
	if c.lease == nil {
		return nil
	}
	p := c.base(Release, now)
	p.CIAddr = c.lease.Addr.Addr()
	p.Options[optServerID] = c.lease.Server.AsSlice()
	delete(p.Options, optParams)
	srv := c.lease.Server
	c.drop()
	c.state, c.next = Init, time.Time{}
	return []Out{{Pkt: p, Unicast: srv}}
}
