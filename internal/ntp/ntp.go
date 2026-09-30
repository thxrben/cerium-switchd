// Package ntp is the NTP client of switchd (reference 5.1, system ntp): it
// queries the configured servers through the management instance and sets
// the system clock. Only client mode is spoken (SNTP, RFC 5905 packet
// format): several servers are compared, the preferred one wins when it
// answers, a large offset steps the clock, a small one is slewed.
package ntp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Server is one configured NTP server.
type Server struct {
	Host   string
	Prefer bool
}

// Clock changes the system time.
type Clock interface {
	// Step sets the clock d ahead (d < 0: back).
	Step(d time.Duration) error
	// Slew lets the clock catch up d gradually (|d| below the step limit).
	Slew(d time.Duration) error
}

// Timing of the client.
const (
	// StepLimit: a larger offset steps the clock, a smaller one is slewed.
	StepLimit = 128 * time.Millisecond
	// A round of queries runs this often while the clock is not
	// synchronised, and this often afterwards.
	pollUnsynced = 64 * time.Second
	pollSynced   = 512 * time.Second
	queryTimeout = 3 * time.Second
	// A first round runs soon after the servers are configured.
	firstPoll = 2 * time.Second
)

// ServerStatus is what the last round found out about a server.
type ServerStatus struct {
	Host     string
	Prefer   bool
	Addr     string // address that answered
	Stratum  int
	Offset   time.Duration
	Delay    time.Duration
	LastPoll time.Time
	Reach    bool
	Err      string
	Selected bool // its offset was used for the clock
}

// Status is the state of the client.
type Status struct {
	Servers    []ServerStatus
	Synced     bool
	LastAdjust time.Time
	LastStep   bool // the last adjustment stepped the clock
	Via        string
}

// Client queries the servers and adjusts the clock.
type Client struct {
	Clock Clock
	Log   *slog.Logger
	// Now returns the current time (tests).
	Now func() time.Time
	// Port is the UDP port of the servers (123; tests use others).
	Port int

	mu      sync.Mutex
	vrf     string
	servers []Server
	status  Status
	cancel  context.CancelFunc
	kick    chan struct{}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) port() int {
	if c.Port != 0 {
		return c.Port
	}
	return 123
}

func (c *Client) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log
}

// SetVRF sets the routing instance (VRF device) the queries leave through
// ("" = the default one); it applies from the next round.
func (c *Client) SetVRF(vrf string) {
	c.mu.Lock()
	c.vrf = vrf
	c.mu.Unlock()
}

// Configure sets the servers. An unchanged list does not restart anything;
// an empty one stops the client.
func (c *Client) Configure(servers []Server) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sameServers(c.servers, servers) {
		return
	}
	c.servers = append([]Server(nil), servers...)
	c.status = Status{}
	for _, s := range servers {
		c.status.Servers = append(c.status.Servers, ServerStatus{Host: s.Host, Prefer: s.Prefer})
	}
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if len(servers) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.kick = make(chan struct{}, 1)
	go c.run(ctx, c.kick)
}

// Poll makes the client run a round now (tests, and after a new commit).
func (c *Client) Poll() {
	c.mu.Lock()
	k := c.kick
	c.mu.Unlock()
	if k != nil {
		select {
		case k <- struct{}{}:
		default:
		}
	}
}

// Stop ends the client.
func (c *Client) Stop() { c.Configure(nil) }

func sameServers(a, b []Server) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Status returns a copy of the current state.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.status
	st.Servers = append([]ServerStatus(nil), st.Servers...)
	st.Via = c.vrf
	return st
}

func (c *Client) run(ctx context.Context, kick <-chan struct{}) {
	wait := firstPoll
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-kick:
			t.Stop()
		case <-t.C:
		}
		c.round(ctx)
		c.mu.Lock()
		synced := c.status.Synced
		c.mu.Unlock()
		wait = pollUnsynced
		if synced {
			wait = pollSynced
		}
	}
}

// sample is one successful query.
type sample struct {
	i       int
	addr    string
	stratum int
	offset  time.Duration
	delay   time.Duration
}

// round queries every server, picks one and adjusts the clock.
func (c *Client) round(ctx context.Context) {
	c.mu.Lock()
	servers := append([]Server(nil), c.servers...)
	vrf := c.vrf
	c.mu.Unlock()
	res := make([]*sample, len(servers))
	errs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i], errs[i] = c.query(ctx, s.Host, vrf)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	var ok []*sample
	for i, r := range res {
		if r != nil {
			r.i = i
			ok = append(ok, r)
		}
	}
	// The preferred servers first, then the lowest delay.
	sort.SliceStable(ok, func(a, b int) bool {
		pa, pb := servers[ok[a].i].Prefer, servers[ok[b].i].Prefer
		if pa != pb {
			return pa
		}
		return ok[a].delay < ok[b].delay
	})

	c.mu.Lock()
	defer c.mu.Unlock()
	if !sameServers(c.servers, servers) {
		return // reconfigured meanwhile
	}
	for i := range c.status.Servers {
		st := &c.status.Servers[i]
		st.LastPoll = c.now()
		st.Selected = false
		if r := res[i]; r != nil {
			st.Reach, st.Err = true, ""
			st.Addr, st.Stratum, st.Offset, st.Delay = r.addr, r.stratum, r.offset, r.delay
		} else {
			st.Reach = false
			st.Err = errs[i].Error()
		}
	}
	if len(ok) == 0 {
		c.status.Synced = false
		c.log().Warn("ntp: no server answered", "facility", "system")
		return
	}
	best := ok[0]
	c.status.Servers[best.i].Selected = true
	step := best.offset > StepLimit || best.offset < -StepLimit
	var err error
	if step {
		err = c.Clock.Step(best.offset)
	} else {
		err = c.Clock.Slew(best.offset)
	}
	if err != nil {
		c.status.Synced = false
		c.log().Error("ntp: setting the clock failed", "facility", "system", "err", err)
		return
	}
	c.status.Synced = true
	c.status.LastAdjust = c.now()
	c.status.LastStep = step
	if step {
		c.log().Warn("ntp: clock stepped", "facility", "system", "server", servers[best.i].Host, "offset", best.offset.String())
	}
}

// ---- protocol ----

// ntpEpoch is the NTP era 0 start (1900-01-01).
var ntpEpoch = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)

func toNTP(t time.Time) uint64 {
	d := t.Sub(ntpEpoch)
	sec := uint64(d / time.Second)
	frac := uint64(d%time.Second) << 32 / uint64(time.Second)
	return sec<<32 | frac
}

func fromNTP(v uint64) time.Time {
	sec := int64(v >> 32)
	frac := int64(v & 0xffffffff)
	return ntpEpoch.Add(time.Duration(sec)*time.Second + time.Duration(frac*int64(time.Second)>>32))
}

func put64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (56 - 8*i))
	}
}

func get64(b []byte) uint64 {
	var v uint64
	for i := 0; i < 8; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v
}

// request builds a client packet with the transmit time t1.
func request(t1 time.Time) []byte {
	b := make([]byte, 48)
	b[0] = 0<<6 | 4<<3 | 3 // no warning, version 4, client
	put64(b[40:], toNTP(t1))
	return b
}

// parse checks a reply to the request sent at t1 (received at t4) and
// returns offset and delay.
func parse(b []byte, t1, t4 time.Time, sent []byte) (offset, delay time.Duration, stratum int, err error) {
	if len(b) < 48 {
		return 0, 0, 0, errors.New("short reply")
	}
	li, mode := b[0]>>6, b[0]&7
	if mode != 4 {
		return 0, 0, 0, fmt.Errorf("not a server reply (mode %d)", mode)
	}
	if li == 3 {
		return 0, 0, 0, errors.New("server is not synchronised")
	}
	stratum = int(b[1])
	if stratum == 0 {
		return 0, 0, 0, fmt.Errorf("kiss-o'-death %q", string(b[12:16]))
	}
	if stratum > 15 {
		return 0, 0, 0, errors.New("server stratum too high")
	}
	// The reply must carry our transmit time (not a stray or forged packet).
	if get64(b[24:]) != get64(sent[40:]) {
		return 0, 0, 0, errors.New("reply to another request")
	}
	rx, tx := get64(b[32:]), get64(b[40:])
	if rx == 0 || tx == 0 {
		return 0, 0, 0, errors.New("reply without timestamps")
	}
	t2, t3 := fromNTP(rx), fromNTP(tx)
	offset = (t2.Sub(t1) + t3.Sub(t4)) / 2
	delay = t4.Sub(t1) - t3.Sub(t2)
	if delay < 0 {
		delay = 0
	}
	return offset, delay, stratum, nil
}

func (c *Client) query(ctx context.Context, host, vrf string) (*sample, error) {
	d := net.Dialer{Timeout: queryTimeout}
	if vrf != "" {
		if _, err := net.InterfaceByName(vrf); err == nil {
			d.Control = func(_, _ string, rc syscall.RawConn) error {
				var serr error
				if err := rc.Control(func(fd uintptr) { serr = syscall.BindToDevice(int(fd), vrf) }); err != nil {
					return err
				}
				return serr
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "udp", net.JoinHostPort(host, strconv.Itoa(c.port())))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(queryTimeout))
	t1 := c.now()
	sent := request(t1)
	if _, err := conn.Write(sent); err != nil {
		return nil, err
	}
	buf := make([]byte, 512)
	for {
		n, err := conn.Read(buf)
		t4 := c.now()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil, errors.New("no answer")
			}
			return nil, err
		}
		off, delay, stratum, perr := parse(buf[:n], t1, t4, sent)
		if perr != nil {
			if perr.Error() == "reply to another request" {
				continue // keep waiting for the right one
			}
			return nil, perr
		}
		return &sample{addr: conn.RemoteAddr().(*net.UDPAddr).IP.String(), stratum: stratum, offset: off, delay: delay}, nil
	}
}

// timeServices are programs that also set the system clock.
var timeServices = []string{"chronyd", "ntpd", "ntpd-rs", "systemd-timesyn", "openntpd"}

// OtherService returns the name of another time service running on the host
// (procDir is /proc), or "".
func OtherService(procDir string) string {
	ents, err := os.ReadDir(procDir)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if e.Name() == "" || e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(procDir, e.Name(), "comm"))
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(raw))
		for _, s := range timeServices {
			if comm == s {
				return comm
			}
		}
	}
	return ""
}
