package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Micro-BFD on the ports of the MC-LAG peer-link (reference 5.6,
// peer-link-bfd): every port sends a small frame to the peer each interval;
// a port is up while the peer's frames arrive. The peer-link is down when
// no port is up, even if the links keep their carrier.

const bfdEtherType = 0x88b5

var (
	bfdDest  = net.HardwareAddr{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e} // link-local: never forwarded by a bridge
	bfdMagic = []byte("MCLAGBFD")
)

// bfdPort is one peer-link port.
type bfdPort struct {
	fd      int
	ifindex int
	mac     net.HardwareAddr
	lastRx  time.Time
	since   time.Time // configured (grace before the first frame)
	stop    chan struct{}
}

type peerBFD struct {
	mu       sync.Mutex
	ports    map[string]*bfdPort // kernel name
	domain   int
	member   int
	peer     int
	interval time.Duration
	detect   time.Duration
	seq      uint32
}

func newPeerBFD() *peerBFD { return &peerBFD{ports: map[string]*bfdPort{}} }

// configure sets the ports and parameters (no ports: off).
func (b *peerBFD) configure(ports []string, domain, member, peer, intervalMS, mult int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if intervalMS <= 0 {
		intervalMS = 100
	}
	if mult <= 0 {
		mult = 3
	}
	b.domain, b.member, b.peer = domain, member, peer
	b.interval = time.Duration(intervalMS) * time.Millisecond
	b.detect = b.interval * time.Duration(mult)
	want := map[string]bool{}
	for _, p := range ports {
		want[p] = true
		if b.ports[p] == nil {
			if bp, err := openBFD(p); err == nil {
				bp.since = time.Now()
				b.ports[p] = bp
				go b.read(p, bp)
			}
		}
	}
	for p, bp := range b.ports {
		if !want[p] {
			close(bp.stop)
			delete(b.ports, p)
		}
	}
}

func openBFD(port string) (*bfdPort, error) {
	ifi, err := net.InterfaceByName(port)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons16(bfdEtherType)))
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons16(bfdEtherType), Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	tv := unix.NsecToTimeval(int64(200 * time.Millisecond))
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	return &bfdPort{fd: fd, ifindex: ifi.Index, mac: ifi.HardwareAddr, stop: make(chan struct{})}, nil
}

func htons16(v uint16) uint16 { return v<<8 | v>>8 }

// frame: magic, domain (2), member (1), seq (4).
func (b *peerBFD) frame(src net.HardwareAddr, seq uint32) []byte {
	f := make([]byte, 0, 64)
	f = append(f, bfdDest...)
	f = append(f, src...)
	f = binary.BigEndian.AppendUint16(f, bfdEtherType)
	f = append(f, bfdMagic...)
	f = binary.BigEndian.AppendUint16(f, uint16(b.domain))
	f = append(f, byte(b.member))
	f = binary.BigEndian.AppendUint32(f, seq)
	for len(f) < 60 {
		f = append(f, 0)
	}
	return f
}

func (b *peerBFD) read(port string, bp *bfdPort) {
	buf := make([]byte, 256)
	for {
		select {
		case <-bp.stop:
			unix.Close(bp.fd)
			return
		default:
		}
		n, _, err := unix.Recvfrom(bp.fd, buf, 0)
		if err != nil || n < 14+len(bfdMagic)+7 {
			continue
		}
		p := buf[14:n]
		if !bytes.HasPrefix(p, bfdMagic) {
			continue
		}
		p = p[len(bfdMagic):]
		b.mu.Lock()
		if int(binary.BigEndian.Uint16(p)) == b.domain && int(p[2]) == b.peer {
			bp.lastRx = time.Now()
		}
		b.mu.Unlock()
	}
}

func (b *peerBFD) run(ctx context.Context) {
	for {
		b.mu.Lock()
		iv := b.interval
		if iv == 0 {
			iv = 100 * time.Millisecond
		}
		b.seq++
		for _, bp := range b.ports {
			var addr [8]byte
			copy(addr[:], bfdDest)
			unix.Sendto(bp.fd, b.frame(bp.mac, b.seq), 0, &unix.SockaddrLinklayer{Protocol: htons16(bfdEtherType), Ifindex: bp.ifindex, Halen: 6, Addr: addr})
		}
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			b.configure(nil, 0, 0, 0, 0, 0)
			return
		case <-time.After(iv):
		}
	}
}

// state reports the ports that pass BFD and whether any does.
func (b *peerBFD) state(now time.Time) (up bool, ports map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ports = map[string]bool{}
	for n, bp := range b.ports {
		// A port just configured gets a grace period for the peer's first
		// frames (a restart is not a peer-link failure).
		ok := (!bp.lastRx.IsZero() && now.Sub(bp.lastRx) < b.detect) || (bp.lastRx.IsZero() && now.Sub(bp.since) < 3*time.Second)
		ports[n] = ok
		up = up || ok
	}
	return up, ports
}

func sortedBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
