//go:build linux

package dhcp

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"golang.org/x/sys/unix"
)

// Iface is an interface that takes its address from DHCP.
type Iface struct {
	Name string // kernel device
	Unit string // configuration name ("1/0/6.0", "irb.10")
	VRF  string
}

// Binding is one client for "show dhcp client binding".
type Binding struct {
	Unit, Device, VRF string
	State             State
	Lease             *Lease
}

// Manager runs the clients.
type Manager struct {
	HostName func() string
	// OnChange is called (from a client goroutine) when a lease is
	// obtained, changes or is lost.
	OnChange func()
	Log      *slog.Logger
	// StateFile keeps the leases while the program restarts (on tmpfs: a
	// reboot starts over); "" none.
	StateFile string

	mu      sync.Mutex
	clients map[string]*runner
	saved   map[string]savedLease // read from StateFile, by device
	loaded  bool
}

// savedLease is a lease of a device kept over a restart, with the MAC it
// was obtained with.
type savedLease struct {
	MAC   [6]byte `json:"mac"`
	Lease Lease   `json:"lease"`
}

type runner struct {
	iface Iface
	c     *Client
	stop  chan struct{}
	done  chan struct{}
	mu    sync.Mutex
	srvHW net.HardwareAddr // the server's (or relay's) MAC, for renewals
	// resumed: the client continues a saved lease (no DISCOVER).
	resumed bool
	// keep: stopping keeps the lease (shutdown for a restart) instead of
	// releasing it (the statement was removed).
	keep bool
}

// Sync starts clients for new interfaces and stops (releasing the lease)
// those that are no longer wanted.
func (m *Manager) Sync(ifs []Iface) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clients == nil {
		m.clients = map[string]*runner{}
	}
	m.loadLocked()
	want := map[string]Iface{}
	for _, i := range ifs {
		want[i.Name] = i
	}
	for n, r := range m.clients {
		if w, ok := want[n]; !ok || w != r.iface {
			close(r.stop)
			<-r.done
			delete(m.clients, n)
		}
	}
	for n, i := range want {
		if m.clients[n] == nil {
			r := &runner{iface: i, stop: make(chan struct{}), done: make(chan struct{})}
			m.clients[n] = r
			go m.run(r)
		}
	}
}

// Shutdown stops every client without releasing its lease (the program
// restarts and resumes them from StateFile).
func (m *Manager) Shutdown() {
	m.mu.Lock()
	rs := make([]*runner, 0, len(m.clients))
	for n, r := range m.clients {
		r.mu.Lock()
		r.keep = true
		r.mu.Unlock()
		close(r.stop)
		rs = append(rs, r)
		delete(m.clients, n)
	}
	m.mu.Unlock()
	for _, r := range rs {
		<-r.done
	}
}

func (m *Manager) loadLocked() {
	if m.loaded {
		return
	}
	m.loaded = true
	m.saved = map[string]savedLease{}
	if m.StateFile == "" {
		return
	}
	raw, err := hwio.ReadFile(m.StateFile)
	if err != nil {
		return
	}
	if err := json.Unmarshal(raw, &m.saved); err != nil {
		m.saved = map[string]savedLease{}
	}
}

// save writes the current leases to StateFile.
func (m *Manager) save() {
	if m.StateFile == "" {
		return
	}
	m.mu.Lock()
	out := map[string]savedLease{}
	for n, r := range m.clients {
		r.mu.Lock()
		if r.c != nil {
			if _, l := r.c.State(); l != nil {
				out[n] = savedLease{MAC: r.c.MAC, Lease: *l}
			}
		}
		r.mu.Unlock()
	}
	m.saved = out
	m.mu.Unlock()
	raw, _ := json.Marshal(out)
	tmp := m.StateFile + ".tmp"
	if hwio.WriteFile(tmp, raw, 0o600) == nil {
		hwio.Rename(tmp, m.StateFile)
	}
}

// Leases returns the current leases by device.
func (m *Manager) Leases() map[string]*Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]*Lease{}
	for n, r := range m.clients {
		r.mu.Lock()
		if r.c != nil {
			if _, l := r.c.State(); l != nil {
				cp := *l
				out[n] = &cp
			}
		}
		r.mu.Unlock()
	}
	return out
}

// Bindings reports every client.
func (m *Manager) Bindings() []Binding {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Binding
	for _, r := range m.clients {
		b := Binding{Unit: r.iface.Unit, Device: r.iface.Name, VRF: r.iface.VRF}
		r.mu.Lock()
		if r.c != nil {
			var l *Lease
			b.State, l = r.c.State()
			if l != nil {
				cp := *l
				b.Lease = &cp
			}
		}
		r.mu.Unlock()
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Unit < out[j].Unit })
	return out
}

func random() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

func (m *Manager) run(r *runner) {
	defer close(r.done)
	var fd, ifindex = -1, 0
	var mac net.HardwareAddr
	closeSock := func() {
		if fd >= 0 {
			unix.Close(fd)
			fd = -1
		}
	}
	defer closeSock()
	open := func() bool {
		ifi, err := net.InterfaceByName(r.iface.Name)
		if err != nil || len(ifi.HardwareAddr) != 6 {
			return false
		}
		if fd >= 0 && ifi.Index == ifindex && string(ifi.HardwareAddr) == string(mac) {
			return true
		}
		closeSock()
		s, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_IP)))
		if err != nil {
			return false
		}
		if err := unix.Bind(s, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: ifi.Index}); err != nil {
			unix.Close(s)
			return false
		}
		tv := unix.NsecToTimeval(int64(250 * time.Millisecond))
		unix.SetsockoptTimeval(s, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
		fd, ifindex, mac = s, ifi.Index, ifi.HardwareAddr
		r.mu.Lock()
		resume := false
		if r.c == nil || [6]byte(mac) != r.c.MAC {
			resume = true
			// A new client for a new MAC (the old lease belongs to the old one).
			host := ""
			if m.HostName != nil {
				host = m.HostName()
			}
			r.c = &Client{MAC: [6]byte(mac), HostName: host, Rand: random, OnLease: func(l *Lease) {
				if l != nil {
					m.Log.Info("dhcp: lease", "interface", r.iface.Unit, "address", l.Addr, "router", l.Router, "lease", l.Time)
				} else {
					m.Log.Warn("dhcp: lease lost", "interface", r.iface.Unit)
				}
				go m.save()
				if m.OnChange != nil {
					go m.OnChange()
				}
			}}
		}
		if resume {
			// The lease held before this program restarted.
			m.mu.Lock()
			sl, ok := m.saved[r.iface.Name]
			m.mu.Unlock()
			if ok && sl.MAC == r.c.MAC && r.c.Resume(&sl.Lease, time.Now()) {
				r.resumed = true
				m.Log.Info("dhcp: lease kept from before the restart", "interface", r.iface.Unit, "address", sl.Lease.Addr)
			}
		}
		r.mu.Unlock()
		return true
	}
	send := func(outs []Out) {
		for _, o := range outs {
			var src netip.Addr = netip.IPv4Unspecified()
			dst := netip.AddrFrom4([4]byte{255, 255, 255, 255})
			dstHW := net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
			if o.Pkt.CIAddr.IsValid() {
				src = o.Pkt.CIAddr
			}
			if o.Unicast.IsValid() {
				dst = o.Unicast
				r.mu.Lock()
				if r.srvHW != nil {
					dstHW = r.srvHW
				}
				r.mu.Unlock()
			}
			f := frame(mac, dstHW, src, dst, o.Pkt.Marshal())
			if err := unix.Sendto(fd, f, 0, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: ifindex, Halen: 6,
				Addr: [8]byte(append([]byte(dstHW), 0, 0))}); err != nil {
				m.Log.Debug("dhcp: send", "interface", r.iface.Unit, "err", err)
			}
		}
	}
	started := false
	buf := make([]byte, 2048)
	tick := time.Now()
	for {
		select {
		case <-r.stop:
			r.mu.Lock()
			var outs []Out
			if r.c != nil && fd >= 0 && !r.keep {
				outs = r.c.Release(time.Now())
			}
			r.mu.Unlock()
			if len(outs) > 0 {
				send(outs)
				m.Log.Info("dhcp: lease released", "interface", r.iface.Unit)
			}
			return
		default:
		}
		if !open() {
			time.Sleep(time.Second)
			continue
		}
		now := time.Now()
		if !started {
			r.mu.Lock()
			var outs []Out
			if !r.resumed {
				outs = r.c.Start(now)
			}
			r.mu.Unlock()
			send(outs)
			started = true
		}
		if now.Sub(tick) >= time.Second {
			tick = now
			r.mu.Lock()
			outs := r.c.Tick(now)
			r.mu.Unlock()
			send(outs)
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil || n < 14 {
			continue
		}
		srcHW, payload, ok := parseFrame(buf[:n])
		if !ok {
			continue
		}
		p, err := Unmarshal(payload)
		if err != nil {
			continue
		}
		r.mu.Lock()
		if p.Op == 2 && p.XID == r.c.xid {
			r.srvHW = srcHW
		}
		outs := r.c.Receive(p, time.Now())
		r.mu.Unlock()
		send(outs)
	}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func csum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

// frame builds Ethernet/IPv4/UDP 68->67 around payload (UDP checksum 0).
func frame(src, dst net.HardwareAddr, sip, dip netip.Addr, payload []byte) []byte {
	f := make([]byte, 14+20+8+len(payload))
	copy(f[0:6], dst)
	copy(f[6:12], src)
	binary.BigEndian.PutUint16(f[12:], unix.ETH_P_IP)
	ip := f[14:34]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(20+8+len(payload)))
	ip[8], ip[9] = 64, unix.IPPROTO_UDP
	s4, d4 := sip.As4(), dip.As4()
	copy(ip[12:16], s4[:])
	copy(ip[16:20], d4[:])
	binary.BigEndian.PutUint16(ip[10:], csum(ip))
	u := f[34:42]
	binary.BigEndian.PutUint16(u[0:], 68)
	binary.BigEndian.PutUint16(u[2:], 67)
	binary.BigEndian.PutUint16(u[4:], uint16(8+len(payload)))
	copy(f[42:], payload)
	return f
}

// parseFrame returns the source MAC and the UDP payload of a frame to port
// 68.
func parseFrame(f []byte) (net.HardwareAddr, []byte, bool) {
	if len(f) < 42 || binary.BigEndian.Uint16(f[12:]) != unix.ETH_P_IP {
		return nil, nil, false
	}
	ip := f[14:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[0]>>4 != 4 || ihl < 20 || len(ip) < ihl+8 || ip[9] != unix.IPPROTO_UDP {
		return nil, nil, false
	}
	u := ip[ihl:]
	if binary.BigEndian.Uint16(u[2:]) != 68 {
		return nil, nil, false
	}
	l := int(binary.BigEndian.Uint16(u[4:]))
	if l < 8 || l > len(u) {
		return nil, nil, false
	}
	return net.HardwareAddr(append([]byte(nil), f[6:12]...)), u[8:l], true
}

// Config is what the clients run (computed by switchd for cer-dhcpcd).
type Config struct {
	Ifaces   []Iface `json:"ifaces,omitempty"`
	HostName string  `json:"host_name,omitempty"`
}
