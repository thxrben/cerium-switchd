package bgpd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/bgp"
)

// Sessions to neighbours on routed ports of other members (reference 5.8):
// BGP runs on the master, but the neighbour's TCP connection ends on the
// member that owns the port. That member's cer-bgpd accepts (or dials) the
// connection with the neighbour's TCP MD5 key and TTL and relays the byte
// stream to the master over the stacking protocol: one ordered call per
// chunk and direction (each call waits for the previous one, so the bytes
// arrive in order). On the master the speaker sees an ordinary connection
// with the real addresses. Sessions over irb interfaces and MC-LAG bundles
// are not relayed: their frames reach the master in their VLAN.

// Stacking-protocol methods of the relay.
const (
	StackRelayOpen  = "bgp-relay-open"  // owner -> master: an accepted connection
	StackRelayDial  = "bgp-relay-dial"  // master -> owner: connect to a neighbour
	StackRelayData  = "bgp-relay-data"  // both ways: bytes
	StackRelayClose = "bgp-relay-close" // both ways: the end
)

const relayChunk = 16 << 10

// RelayOpen describes a relayed connection.
type RelayOpen struct {
	ID       uint64         `json:"id"`
	Instance string         `json:"instance"`
	Local    netip.AddrPort `json:"local"`
	Remote   netip.AddrPort `json:"remote"`
}

// RelayDial asks an owner to connect to a neighbour.
type RelayDial struct {
	ID       uint64       `json:"id"`
	Instance string       `json:"instance"`
	VRF      string       `json:"vrf"`
	Neighbor bgp.Neighbor `json:"neighbor"`
}

// RelayData is a chunk of a relayed connection (Seq counts per direction).
type RelayData struct {
	ID   uint64 `json:"id"`
	Seq  uint64 `json:"seq"`
	Data []byte `json:"data"`
}

// RelayClose ends a relayed connection.
type RelayClose struct {
	ID uint64 `json:"id"`
}

// relay is the relayed connections of this daemon (both roles).
type relay struct {
	mu     sync.Mutex
	conns  map[uint64]*relayConn
	nextID atomic.Uint64
}

// relayConn is one relayed connection: on the owner the TCP connection,
// on the master one end of a pipe whose other end the speaker has.
type relayConn struct {
	id     uint64
	peer   int      // the other member
	local  net.Conn // what this side reads from and writes to
	seq    uint64   // next incoming chunk
	closed bool
	in     chan []byte   // incoming chunks, written in order (never closed)
	done   chan struct{} // closed when the connection ends
}

func newRelayConn(id uint64, peer int, local net.Conn) *relayConn {
	return &relayConn{id: id, peer: peer, local: local, in: make(chan []byte, 64), done: make(chan struct{})}
}

func (r *relay) add(c *relayConn) {
	r.mu.Lock()
	if r.conns == nil {
		r.conns = map[uint64]*relayConn{}
	}
	r.conns[c.id] = c
	r.mu.Unlock()
}

func (r *relay) get(id uint64) *relayConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns[id]
}

func (r *relay) remove(id uint64) *relayConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.conns[id]
	delete(r.conns, id)
	return c
}

// newID is unique per member (the member id in the high bits).
func (d *Daemon) newID() uint64 {
	return uint64(d.Member)<<48 | d.rel.nextID.Add(1)
}

// pump copies the local connection to the other member, chunk by chunk;
// then closes the relayed connection everywhere.
func (d *Daemon) pump(c *relayConn) {
	buf := make([]byte, relayChunk)
	var seq uint64
	for {
		n, err := c.local.Read(buf)
		if n > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cerr := d.StackCall(ctx, c.peer, StackRelayData, RelayData{ID: c.id, Seq: seq, Data: append([]byte(nil), buf[:n]...)}, nil)
			cancel()
			seq++
			if cerr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	d.closeRelay(c.id, true)
}

// writer writes incoming chunks in order.
func (d *Daemon) writer(c *relayConn) {
	for {
		select {
		case b := <-c.in:
			if _, err := c.local.Write(b); err != nil {
				d.closeRelay(c.id, true)
				return
			}
		case <-c.done:
			return
		}
	}
}

// closeRelay ends a relayed connection (tell: the other member too).
func (d *Daemon) closeRelay(id uint64, tell bool) {
	c := d.rel.remove(id)
	if c == nil {
		return
	}
	d.rel.mu.Lock()
	c.closed = true
	d.rel.mu.Unlock()
	c.local.Close()
	close(c.done)
	if tell && d.StackCall != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = d.StackCall(ctx, c.peer, StackRelayClose, RelayClose{ID: id}, nil)
		}()
	}
}

// RelayedData is StackRelayData.
func (d *Daemon) RelayedData(m RelayData) error {
	c := d.rel.get(m.ID)
	if c == nil {
		return fmt.Errorf("bgp relay: no connection %d", m.ID)
	}
	d.rel.mu.Lock()
	ok := m.Seq == c.seq && !c.closed
	if ok {
		c.seq++
	}
	d.rel.mu.Unlock()
	if !ok {
		d.closeRelay(m.ID, true)
		return fmt.Errorf("bgp relay: connection %d out of order", m.ID)
	}
	select {
	case c.in <- m.Data:
		return nil
	case <-c.done:
		return fmt.Errorf("bgp relay: connection %d closed", m.ID)
	}
}

// RelayedClose is StackRelayClose.
func (d *Daemon) RelayedClose(m RelayClose) { d.closeRelay(m.ID, false) }

func (d *Daemon) startRelay(c *relayConn) {
	d.rel.add(c)
	go d.writer(c)
	go d.pump(c)
}

// ---- the master ----

// pipeConn is the speaker's end of a relayed connection: the addresses
// are those of the real TCP connection on the owner.
type pipeConn struct {
	net.Conn
	local, remote net.Addr
}

func (p pipeConn) LocalAddr() net.Addr  { return p.local }
func (p pipeConn) RemoteAddr() net.Addr { return p.remote }

func tcpAddr(a netip.AddrPort) net.Addr { return net.TCPAddrFromAddrPort(a) }

// RelayOpened is StackRelayOpen on the master: a neighbour connected to an
// owner; the speaker of the instance gets the connection.
func (d *Daemon) RelayOpened(from int, o RelayOpen) error {
	d.mu.Lock()
	in := d.insts[o.Instance]
	d.mu.Unlock()
	if in == nil {
		return fmt.Errorf("BGP does not run in instance %s", instName(o.Instance))
	}
	speakerEnd, relayEnd := net.Pipe()
	d.startRelay(newRelayConn(o.ID, from, relayEnd))
	in.sp.Accept(pipeConn{Conn: speakerEnd, local: tcpAddr(o.Local), remote: tcpAddr(o.Remote)})
	return nil
}

// dialRelay connects to a neighbour through its owner.
func (d *Daemon) dialRelay(ctx context.Context, owner int, instance, vrf string, n bgp.Neighbor) (net.Conn, error) {
	if d.StackCall == nil {
		return nil, errors.New("no stacking")
	}
	id := d.newID()
	speakerEnd, relayEnd := net.Pipe()
	// Registered before the owner connects: its first bytes may come
	// before the answer.
	c := newRelayConn(id, owner, relayEnd)
	d.rel.add(c)
	var o RelayOpen
	if err := d.StackCall(ctx, owner, StackRelayDial, RelayDial{ID: id, Instance: instance, VRF: vrf, Neighbor: n}, &o); err != nil {
		d.rel.remove(id)
		speakerEnd.Close()
		relayEnd.Close()
		return nil, fmt.Errorf("through member %d: %w", owner, err)
	}
	go d.writer(c)
	go d.pump(c)
	return pipeConn{Conn: speakerEnd, local: tcpAddr(o.Local), remote: tcpAddr(o.Remote)}, nil
}

// ---- the owner ----

// RelayDialed is StackRelayDial on an owner: connect to the neighbour and
// relay the connection to the master (from).
func (d *Daemon) RelayDialed(ctx context.Context, from int, q RelayDial) (RelayOpen, error) {
	conn, err := d.Net.Dial(ctx, q.VRF, q.Neighbor)
	if err != nil {
		return RelayOpen{}, err
	}
	o := RelayOpen{ID: q.ID, Instance: q.Instance, Local: addrPort(conn.LocalAddr()), Remote: addrPort(conn.RemoteAddr())}
	d.startRelay(newRelayConn(q.ID, from, conn))
	return o, nil
}

func addrPort(a net.Addr) netip.AddrPort {
	if t, ok := a.(*net.TCPAddr); ok {
		ap := t.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	return netip.AddrPort{}
}

// ownerListen keeps a listener per instance on a non-master member for the
// neighbours on its routed ports (d.mu held).
func (d *Daemon) ownerListen(cfg Config, master int) {
	want := map[string]Instance{}
	if !d.master && master != 0 && d.Member != 0 {
		for _, in := range cfg.Instances {
			var mine []Neighbor
			for _, n := range in.Neighbors {
				if n.Owner == d.Member && !n.Disabled {
					mine = append(mine, n)
				}
			}
			if len(mine) > 0 {
				c := in
				c.Neighbors = mine
				want[in.Name] = c
			}
		}
	}
	for name, l := range d.owned {
		if w, ok := want[name]; !ok || w.VRF != l.vrf {
			l.lis.Close()
			delete(d.owned, name)
		}
	}
	for _, name := range sortedKeys(want) {
		w := want[name]
		if l := d.owned[name]; l != nil {
			l.mu.Lock()
			l.cfg = w
			l.mu.Unlock()
			_ = l.lis.SetKeys(keys(w))
			continue
		}
		lis, err := d.Net.Listen(w.VRF, keys(w))
		if err != nil {
			d.Log.Warn("bgp relay: cannot listen", "instance", instName(name), "err", err)
			continue
		}
		ol := &ownerListener{lis: lis, vrf: w.VRF, cfg: w}
		if d.owned == nil {
			d.owned = map[string]*ownerListener{}
		}
		d.owned[name] = ol
		go d.acceptOwned(ol, master)
	}
}

// ownerListener accepts neighbours' connections for the master.
type ownerListener struct {
	lis Listener
	vrf string
	mu  sync.Mutex
	cfg Instance
}

func (d *Daemon) acceptOwned(ol *ownerListener, master int) {
	for {
		conn, err := ol.lis.Accept()
		if err != nil {
			return
		}
		remote := addrPort(conn.RemoteAddr())
		ol.mu.Lock()
		cfg := ol.cfg
		ol.mu.Unlock()
		var nb *Neighbor
		for i := range cfg.Neighbors {
			if cfg.Neighbors[i].Addr == remote.Addr() {
				nb = &cfg.Neighbors[i]
			}
		}
		if nb == nil {
			conn.Close()
			continue
		}
		SetAcceptedTTL(conn, nb.Neighbor)
		d.mu.Lock()
		m := d.masterID
		d.mu.Unlock()
		if m == 0 {
			conn.Close()
			continue
		}
		go func() {
			o := RelayOpen{ID: d.newID(), Instance: cfg.Name, Local: addrPort(conn.LocalAddr()), Remote: remote}
			c := newRelayConn(o.ID, m, conn)
			d.rel.add(c)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := d.StackCall(ctx, m, StackRelayOpen, o, nil)
			cancel()
			if err != nil {
				d.rel.remove(o.ID)
				conn.Close()
				return
			}
			go d.writer(c)
			d.pump(c)
		}()
	}
}
