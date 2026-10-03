//go:build linux

package lldp

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"reflect"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Agent runs LLDP on this member's ports.
type Agent struct {
	Log *slog.Logger
	// Carrier reports whether a kernel port has a link.
	Carrier func(linux string) bool
	// Aggregated reports whether a bundle member port carries traffic for
	// its bundle now (LACP has it collecting and distributing); nil: as
	// PortSpec.InBundle says.
	Aggregated func(linux string) bool

	mu    sync.Mutex
	sys   System
	ports map[string]*agentPort // by kernel name
	tab   table
	stats map[string]*Stats // by interface name
	rx    chan rxFrame
	kick  chan struct{}
}

type agentPort struct {
	spec    PortSpec
	fd      int
	ifindex int
	mac     net.HardwareAddr
	stop    chan struct{}
	up      bool      // carrier at the last check
	next    time.Time // next periodic LLDPDU
	dirty   bool      // announce now (changed)
}

type rxFrame struct {
	linux string
	frame []byte
}

func (a *Agent) init() {
	if a.ports == nil {
		a.ports, a.stats, a.rx, a.kick = map[string]*agentPort{}, map[string]*Stats{}, make(chan rxFrame, 256), make(chan struct{}, 1)
	}
	if a.Log == nil {
		a.Log = slog.Default()
	}
}

// Sync sets what the ports announce (nil ports: LLDP off). Ports that
// leave send a shutdown LLDPDU.
func (a *Agent) Sync(sys System, ports []PortSpec) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	sysChanged := !reflect.DeepEqual(sys, a.sys)
	a.sys = sys
	want := map[string]PortSpec{}
	for _, p := range ports {
		want[p.Linux] = p
	}
	for n, p := range a.ports {
		if _, ok := want[n]; !ok {
			if p.up {
				a.send(p, 0)
			}
			close(p.stop)
			a.tab.drop(p.spec.Name)
			delete(a.ports, n)
		}
	}
	for _, s := range ports {
		p := a.ports[s.Linux]
		if p == nil {
			var err error
			if p, err = a.open(s); err != nil {
				a.Log.Warn("lldp: port", "port", s.Name, "err", err)
				continue
			}
			a.ports[s.Linux] = p
			p.dirty = true
		}
		if a.Aggregated != nil {
			s.InBundle = p.spec.InBundle // follows the bundle at run time (tick)
		}
		if sysChanged || !reflect.DeepEqual(p.spec, s) {
			if p.spec.Name != "" && p.spec.Name != s.Name {
				a.tab.drop(p.spec.Name)
			}
			p.spec, p.dirty = s, true
		}
	}
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

func (a *Agent) open(s PortSpec) (*agentPort, error) {
	ifi, err := net.InterfaceByName(s.Linux)
	if err != nil {
		return nil, err
	}
	proto := htons(EtherType)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: proto, Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	// Routed and management ports are not promiscuous: accept the LLDP
	// group address.
	mr := unix.PacketMreq{Ifindex: int32(ifi.Index), Type: unix.PACKET_MR_MULTICAST, Alen: 6}
	copy(mr.Address[:], Dest)
	_ = unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, &mr)
	tv := unix.NsecToTimeval(int64(500 * time.Millisecond))
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	p := &agentPort{spec: s, fd: fd, ifindex: ifi.Index, mac: ifi.HardwareAddr, stop: make(chan struct{})}
	go a.read(s.Linux, p)
	return p, nil
}

func (a *Agent) read(linux string, p *agentPort) {
	buf := make([]byte, 1600)
	for {
		select {
		case <-p.stop:
			unix.Close(p.fd)
			return
		default:
		}
		n, _, err := unix.Recvfrom(p.fd, buf, 0)
		if err != nil || n < 14 {
			continue
		}
		select {
		case a.rx <- rxFrame{linux, append([]byte(nil), buf[:n]...)}:
		default:
		}
	}
}

// send transmits the port's LLDPDU (called locked).
func (a *Agent) send(p *agentPort, ttl uint16) {
	var addr [8]byte
	copy(addr[:], Dest)
	f := a.sys.pdu(p.spec, ttl).Frame(p.mac)
	if err := unix.Sendto(p.fd, f, 0, &unix.SockaddrLinklayer{Protocol: htons(EtherType), Ifindex: p.ifindex, Halen: 6, Addr: addr}); err != nil {
		a.Log.Debug("lldp: send", "port", p.spec.Name, "err", err)
		return
	}
	a.stat(p.spec.Name).Sent++
}

func (a *Agent) stat(port string) *Stats {
	s := a.stats[port]
	if s == nil {
		s = &Stats{Port: port}
		a.stats[port] = s
	}
	return s
}

// Run sends and receives until ctx is done.
func (a *Agent) Run(ctx context.Context) {
	a.mu.Lock()
	a.init()
	a.mu.Unlock()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.mu.Lock()
			for _, p := range a.ports {
				if p.up {
					a.send(p, 0)
				}
			}
			a.mu.Unlock()
			return
		case f := <-a.rx:
			a.receive(f)
		case <-a.kick:
			a.tick(time.Now())
		case <-t.C:
			a.tick(time.Now())
		}
	}
}

func (a *Agent) tick(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for port, n := range a.tab.age(now) {
		a.stat(port).AgedOut += uint64(n)
	}
	for _, p := range a.ports {
		up := a.Carrier == nil || a.Carrier(p.spec.Linux)
		switch {
		case !up:
			if p.up {
				a.tab.drop(p.spec.Name)
			}
			p.up = false
			continue
		case !p.up:
			p.up, p.dirty = true, true // a link that comes up announces at once
		}
		if p.spec.Bundle != "" && a.Aggregated != nil {
			if in := a.Aggregated(p.spec.Linux); in != p.spec.InBundle {
				p.spec.InBundle, p.dirty = in, true // joined or left its bundle
			}
		}
		if p.dirty || !now.Before(p.next) {
			a.send(p, a.sys.TTL())
			p.dirty = false
			iv := a.sys.Interval
			if iv <= 0 {
				iv = 30
			}
			p.next = now.Add(time.Duration(iv) * time.Second)
		}
	}
}

func (a *Agent) receive(f rxFrame) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.ports[f.linux]
	if p == nil || len(f.frame) < 14 {
		return
	}
	payload := f.frame[14:]
	if binary.BigEndian.Uint16(f.frame[12:]) == 0x8100 && len(f.frame) >= 18 { // tagged (should not happen)
		payload = f.frame[18:]
	}
	st := a.stat(p.spec.Name)
	d, err := Parse(payload)
	if err != nil {
		st.Discarded++
		return
	}
	st.Received++
	if _, discarded := a.tab.learn(p.spec.Name, d, time.Now()); discarded {
		st.Discarded++
	}
}

// Neighbors lists the neighbours of this member's ports.
func (a *Agent) Neighbors() []Neighbor {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tab.list()
}

// Status returns what the ports announce and their counters.
func (a *Agent) Status() (System, []PortSpec, []Stats) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ps []PortSpec
	var st []Stats
	for _, p := range a.ports {
		ps = append(ps, p.spec)
		st = append(st, *a.stat(p.spec.Name))
	}
	return a.sys, ps, st
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// Snapshot returns the agent's state.
func (a *Agent) Snapshot() Status {
	sys, ports, stats := a.Status()
	return Status{System: sys, Ports: ports, Stats: stats, Neighbors: a.Neighbors()}
}
