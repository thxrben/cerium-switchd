package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/alarms"
	"github.com/thxrben/cerium-switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"github.com/thxrben/cerium-switchd/pkg/macsec"
)

// stackMACsec encrypts the stacking links (virtual-chassis macsec,
// reference 5.2): a MACsec device per encrypted stacking port carries the
// stack tunnels; which links are encrypted is decided per link
// (decideLink: both ends' modes and NIC offload). Each member makes the key of its own transmit direction and
// pushes it to the neighbour over the stacking protocol (mutually
// authenticated TLS); it sends with a key only after the neighbour has
// installed it for receiving, so a new key loses no frame.
type stackMACsec struct {
	member int
	log    *slog.Logger
	alarms *alarms.Set
	k      macsec.Kernel
	// push hands a key to a neighbour (the stacking protocol).
	push func(neighbor int, req secPush) (secReply, error)
	// portMAC is a port's MAC address; portMTU its MTU.
	portMAC func(port string) string
	portMTU func(port string) int
	now     func() time.Time
	// members counts the stack's members (nil: unknown): when one leaves,
	// every link gets new keys.
	members     func() int
	lastMembers int

	mu    sync.Mutex
	links map[string]*secLink // encrypted links by port
	plain []stackLinkSpec     // the other links (show security macsec)

	// The links the stack loop wants (want), taken by run: the key
	// exchange waits for neighbours and must not hold up that loop.
	wantMu    sync.Mutex
	wantLinks []stackLinkSpec
	wantNew   chan struct{}
}

// want records the stacking links and their decisions.
func (s *stackMACsec) want(links []stackLinkSpec) {
	s.wantMu.Lock()
	changed := fmt.Sprint(links) != fmt.Sprint(s.wantLinks)
	s.wantLinks = links
	if s.wantNew == nil {
		s.wantNew = make(chan struct{}, 1)
	}
	ch := s.wantNew
	s.wantMu.Unlock()
	if changed {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// run converges with what the stack loop wants: at once on a change, else
// every second (keys to renew, retries).
func (s *stackMACsec) run(ctx context.Context) {
	s.wantMu.Lock()
	if s.wantNew == nil {
		s.wantNew = make(chan struct{}, 1)
	}
	ch := s.wantNew
	s.wantMu.Unlock()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-ch:
		}
		s.wantMu.Lock()
		links := s.wantLinks
		s.wantMu.Unlock()
		if s.members != nil {
			if n := s.members(); n < s.lastMembers {
				s.renewAll("a member left the stack")
				s.lastMembers = n
			} else {
				s.lastMembers = n
			}
		}
		s.sync(links)
	}
}

// stackCipher is the stacking links' cipher suite: 64-bit packet numbers,
// so a key never runs out between renewals.
const stackCipher = macsec.GCMAESXPN256

// stackRekey is how often a link's keys are renewed.
const stackRekey = time.Hour

// secLink is one stacking link's MACsec state.
type secLink struct {
	port, dev string
	neighbor  int
	peerMAC   string
	created   bool
	mtu       int
	// tx is the key frames are sent with (nil: none yet); next one being
	// handed to the neighbour.
	tx      *macsec.SAK
	txSince time.Time
	repush  bool // the neighbour lost our key (it restarted)
	// rx are the association numbers installed for receiving.
	rx       []uint8
	rxSC     bool
	rxSince  time.Time
	lastTry  time.Time
	plain    bool // the neighbour's version has no MACsec
	filtered bool
	// name, peerName: the ends' interface names; offload the NIC's
	// ("mac", "phy" or "": software); software: encryption in software is
	// allowed (an end is "on"); refused: the driver refused the offload
	// and software is not allowed (the link stays plain until refusedAt +
	// 10 min).
	name, peerName string
	nicOffload     bool
	offload        string
	software       bool
	refused        bool
	refusedAt      time.Time
	lastErr        string // of the last key handover ("": it worked)
}

// secPush is a key for the neighbour's receive direction.
type secPush struct {
	// SenderMAC is the sender's port MAC (the receiver's link and receive
	// SC); Key the sender's new transmit key.
	SenderMAC string     `json:"sender_mac"`
	Key       macsec.SAK `json:"key"`
	// NeedYours: the sender has no key of the receiver (it restarted).
	NeedYours bool `json:"need_yours,omitempty"`
}

type secReply struct {
	OK bool `json:"ok"`
}

const stackMACsecAlarm = "switchd/macsec "

// secured: both directions have keys: the link's traffic goes through
// the MACsec device.
func (l *secLink) secured() bool { return l.created && l.tx != nil && len(l.rx) > 0 }

// devName is the MACsec device of a port (at most 15 characters).
func devName(port string, index int) string {
	if n := "ms" + port; len(n) <= 15 {
		return n
	}
	return "ms" + strconv.Itoa(index)
}

// DataDev is the device carrying a stacking link's tunnel traffic: its
// MACsec device once secured, else the port.
func (s *stackMACsec) DataDev(port string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.links[port]; l != nil && l.secured() {
		return l.dev
	}
	return ""
}

// stackLinkSpec is a stacking link and its MACsec decision.
type stackLinkSpec struct {
	Port     string
	Index    int
	Neighbor int
	PeerMAC  string
	// Name and PeerName are the ends' interface names; Offload: this end's
	// NIC encrypts MACsec.
	Name, PeerName string
	Offload        bool
	// Encrypt: the link is encrypted; Software: in software where the NIC
	// cannot (an end is "on"); Why explains a plain link.
	Encrypt, Software bool
	Why               string
}

// sync converges with the stacking links and their decisions; it runs
// every second and on every change.
func (s *stackMACsec) sync(links []stackLinkSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links == nil {
		s.links = map[string]*secLink{}
	}
	want := map[string]stackLinkSpec{}
	s.plain = nil
	for _, l := range links {
		if l.Encrypt {
			want[l.Port] = l
		} else {
			s.plain = append(s.plain, l)
		}
	}
	for port, l := range s.links {
		if w, ok := want[port]; !ok || w.Neighbor != l.neighbor || !strings.EqualFold(w.PeerMAC, l.peerMAC) ||
			w.Offload != l.nicOffload || w.Software != l.software {
			s.remove(l)
			delete(s.links, port)
		}
	}
	for port, w := range want {
		l := s.links[port]
		if l == nil {
			l = &secLink{port: port, dev: devName(port, w.Index), neighbor: w.Neighbor, peerMAC: strings.ToLower(w.PeerMAC),
				name: w.Name, peerName: w.PeerName, nicOffload: w.Offload, software: w.Software}
			s.links[port] = l
		}
		s.step(l)
	}
}

// renewAll makes every link change its transmit key at the next step.
func (s *stackMACsec) renewAll(why string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		if l.tx != nil {
			l.repush, l.lastTry = true, time.Time{}
		}
	}
	if len(s.links) > 0 {
		s.log.Info("stack macsec: new keys on every link", "why", why)
	}
}

// remove takes a link's MACsec away (the port carries plain traffic).
func (s *stackMACsec) remove(l *secLink) {
	if l.filtered {
		macsec.FilterPlain(s.k, l.port, false)
	}
	if l.created {
		if err := macsec.Delete(s.k, l.dev); err != nil {
			s.log.Warn("stack macsec: device not removed", "dev", l.dev, "err", err)
		}
	}
	s.alarms.Clear(stackMACsecAlarm + l.port)
}

// ssci is the transmitter's short SCI: unique per link (the lower member
// 1, the higher 2).
func (s *stackMACsec) ssci(neighbor int) uint32 {
	if s.member < neighbor {
		return 1
	}
	return 2
}

func (s *stackMACsec) step(l *secLink) {
	now := s.now()
	if l.refused {
		if now.Sub(l.refusedAt) < 10*time.Minute {
			return
		}
		l.refused = false // the driver may take it now (e.g. after an update)
	}
	if !s.create(l, now) {
		return
	}
	// The MACsec device carries the port's MTU less its overhead (it does
	// not follow a larger port MTU by itself).
	if mtu := s.portMTU(l.port) - 32; mtu > 0 && mtu != l.mtu {
		if s.k.Batch([]string{fmt.Sprintf("link set %s mtu %d", l.dev, mtu)}) == nil {
			l.mtu = mtu
		}
	}
	due := l.tx == nil || l.repush || now.Sub(l.txSince) >= stackRekey
	if due && !l.plain && now.Sub(l.lastTry) >= 5*time.Second {
		s.rekey(l, now)
	}
	if l.plain && now.Sub(l.lastTry) >= 10*time.Minute {
		l.plain = false // try again: the neighbour may have been updated
	}
	if sec := l.secured(); sec != l.filtered {
		if err := macsec.FilterPlain(s.k, l.port, sec); err == nil {
			l.filtered = sec
		}
	}
}

// create makes the link's device (true: it exists).
func (s *stackMACsec) create(l *secLink, now time.Time) bool {
	if !l.created {
		err := s.createDevice(l)
		if errors.Is(err, errOffloadRefused) {
			l.refused, l.refusedAt = true, now
			s.alarms.Raise(stackMACsecAlarm+l.port, alarms.Minor, fmt.Sprintf("stacking port %s is not encrypted: its driver refused the MACsec offload (mode auto: no software encryption)", l.name))
			s.log.Warn("stack macsec: the driver refused the offload; the link stays plain", "port", l.port)
			return false
		}
		if err != nil && strings.Contains(err.Error(), "File exists") {
			// switchd restarted: the device and its keys are still there
			// (traffic keeps flowing); new keys in both directions replace
			// the ones this run does not know.
			err = nil
			l.tx = &macsec.SAK{AN: s.encodingSA(l.dev)}
			l.rx, l.rxSC = []uint8{0}, true
			l.txSince, l.rxSince = time.Time{}, now
			l.repush = true
		}
		if err != nil {
			if now.Sub(l.lastTry) > time.Minute {
				s.log.Warn("stack macsec: device not created", "port", l.port, "err", err)
				l.lastTry = now
			}
			return false
		}
		l.created = true
	}
	return true
}

var errOffloadRefused = errors.New("the driver refused the MACsec offload")

// createDevice makes the link's MACsec device: offloaded to the NIC (mac,
// else phy) where it can; in software when it cannot, or when the driver
// refuses and software is allowed (reference 5.2).
func (s *stackMACsec) createDevice(l *secLink) error {
	d := macsec.Device{Name: l.dev, Parent: l.port, Cipher: stackCipher}
	if !l.nicOffload {
		l.offload = ""
		return macsec.Create(s.k, d)
	}
	var errs []error
	for _, mode := range []string{"mac", "phy"} {
		d.Offload = mode
		err := macsec.Create(s.k, d)
		if err == nil || strings.Contains(err.Error(), "File exists") {
			l.offload = mode
			return err
		}
		errs = append(errs, err)
		macsec.Delete(s.k, l.dev) // a device left half made by the failed offload
	}
	if !l.software {
		return fmt.Errorf("%w: %v", errOffloadRefused, errors.Join(errs...))
	}
	s.alarms.Raise(stackMACsecAlarm+l.port, alarms.Minor, fmt.Sprintf("stacking port %s encrypts in software: its driver refused the MACsec offload", l.name))
	d.Offload, l.offload = "", ""
	return macsec.Create(s.k, d)
}

// rekey hands a new transmit key to the neighbour and sends with it once
// the neighbour has it.
func (s *stackMACsec) rekey(l *secLink, now time.Time) {
	l.lastTry = now
	an := uint8(0)
	if l.tx != nil {
		an = (l.tx.AN + 1) % 4
	}
	sak, err := macsec.NewSAK(stackCipher, an, s.ssci(l.neighbor))
	if err != nil {
		return
	}
	// Unlocked while the neighbour answers (it may push to us meanwhile).
	req := secPush{SenderMAC: s.portMAC(l.port), Key: sak, NeedYours: len(l.rx) == 0 || l.repushRx()}
	s.mu.Unlock()
	rep, err := s.push(l.neighbor, req)
	s.mu.Lock()
	if s.links[l.port] != l {
		return // removed meanwhile
	}
	if err != nil {
		l.lastErr = err.Error()
		// Only a member that does not know the operation lacks MACsec
		// (user report: "member 2 is not reachable"-like errors made a
		// link plain for 10 minutes).
		if strings.Contains(err.Error(), `unknown operation "stack-macsec"`) {
			l.plain = true
			s.alarms.Raise(stackMACsecAlarm+l.port, alarms.Minor, fmt.Sprintf("stacking port %s is not encrypted: member %d's version has no MACsec", l.port, l.neighbor))
		} else {
			s.log.Debug("stack macsec: key not handed over", "port", l.port, "neighbor", l.neighbor, "err", err)
		}
		return
	}
	if !rep.OK {
		return
	}
	l.lastErr = ""
	if err := macsec.AddTx(s.k, l.dev, stackCipher, sak); err != nil {
		s.log.Warn("stack macsec: transmit key not installed", "port", l.port, "err", err)
		return
	}
	if err := macsec.SetEncoding(s.k, l.dev, an); err != nil {
		s.log.Warn("stack macsec: transmit key not used", "port", l.port, "err", err)
		return
	}
	if old := l.tx; old != nil && old.AN != an {
		macsec.DelTx(s.k, l.dev, old.AN)
	}
	first := l.tx == nil
	l.tx, l.txSince, l.repush = &sak, now, false
	if l.offload != "" || !l.nicOffload {
		s.alarms.Clear(stackMACsecAlarm + l.port) // keeps a software fallback's alarm
	}
	if first {
		s.log.Info("stack macsec: link encrypted (sending)", "port", l.port, "neighbor", l.neighbor)
	}
}

// repushRx: the receive keys were assumed after a restart; ask for new ones.
func (l *secLink) repushRx() bool { return l.rxSince.IsZero() || l.repush }

// receive installs a neighbour's key (the stacking protocol handler).
func (s *stackMACsec) receive(from int, req secPush) (secReply, error) {
	if err := req.Key.Valid(stackCipher); err != nil {
		return secReply{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var l *secLink
	for _, x := range s.links {
		if x.neighbor == from && strings.EqualFold(x.peerMAC, req.SenderMAC) {
			l = x
		}
	}
	if l == nil || l.refused {
		return secReply{}, errors.New("MACsec is not used on this stacking link (reference 5.2: mode, offload)")
	}
	if !s.create(l, s.now()) {
		return secReply{}, errors.New("the MACsec device is not ready")
	}
	// Room for the new key: at most this and the previous one stay.
	for _, an := range l.rx {
		if an == req.Key.AN {
			macsec.DelRx(s.k, l.dev, l.peerMAC, an)
		}
	}
	if err := macsec.AddRx(s.k, l.dev, stackCipher, l.peerMAC, req.Key, !l.rxSC); err != nil {
		return secReply{}, fmt.Errorf("receive key not installed: %v", err)
	}
	l.rxSC = true
	keep := []uint8{req.Key.AN}
	for _, an := range l.rx {
		if an == req.Key.AN {
			continue
		}
		if len(keep) < 2 {
			keep = append(keep, an)
		} else {
			macsec.DelRx(s.k, l.dev, l.peerMAC, an)
		}
	}
	l.rx, l.rxSince = keep, s.now()
	switch {
	case req.NeedYours && l.tx != nil:
		l.repush, l.lastTry = true, time.Time{} // send ours again at once
	case l.tx == nil:
		l.lastTry = time.Time{} // the neighbour is ready: ours at once
	}
	return secReply{OK: true}, nil
}

var encodingRE = regexp.MustCompile(`encodingsa (\d)`)

// encodingSA reads the device's current encoding SA.
func (s *stackMACsec) encodingSA(dev string) uint8 {
	type outputter interface {
		Output(name string, args ...string) ([]byte, error)
	}
	o, ok := s.k.(outputter)
	if !ok {
		return 0
	}
	out, err := o.Output("ip", "-d", "link", "show", "dev", dev)
	if err != nil {
		return 0
	}
	if m := encodingRE.FindSubmatch(out); m != nil {
		n, _ := strconv.Atoi(string(m[1]))
		return uint8(n)
	}
	return 0
}

// StackSecStatus is one stacking link for show security macsec.
type StackSecStatus struct {
	Port, Dev  string
	Neighbor   int
	State      string
	TxAN       int
	RxANs      []uint8
	KeySince   time.Time
	PeerMAC    string
	Offloading bool
	// Name and PeerName are the ends' interface names.
	Name, PeerName string
}

// status lists the links.
func (s *stackMACsec) status() []StackSecStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StackSecStatus
	for _, l := range s.links {
		st := StackSecStatus{Port: l.port, Dev: l.dev, Neighbor: l.neighbor, TxAN: -1, RxANs: append([]uint8(nil), l.rx...),
			PeerMAC: l.peerMAC, KeySince: l.txSince, Offloading: l.offload != "", Name: l.name, PeerName: l.peerName}
		switch {
		case l.refused:
			st.State, st.Dev = "plain (the driver refused the offload)", ""
		case l.plain:
			st.State = "plain (neighbour without MACsec)"
		case l.secured() && l.offload != "":
			st.State = "secured (hardware)"
		case l.secured():
			st.State = "secured (software)"
		case l.lastErr != "":
			st.State = "negotiating (last error: " + l.lastErr + ")"
		default:
			st.State = "negotiating"
		}
		if l.tx != nil {
			st.TxAN = int(l.tx.AN)
		}
		out = append(out, st)
	}
	for _, p := range s.plain {
		out = append(out, StackSecStatus{Port: p.Port, Neighbor: p.Neighbor, TxAN: -1, PeerMAC: strings.ToLower(p.PeerMAC),
			Name: p.Name, PeerName: p.PeerName, State: "plain (" + p.Why + ")"})
	}
	return out
}

// portMAC and portMTU of the kernel, from sysfs (net.InterfaceByName
// dumps every interface; this runs every second per stacking link).
func kernelPortMAC(port string) string {
	if raw, err := hwio.ReadFile("/sys/class/net/" + port + "/address"); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

func kernelPortMTU(port string) int {
	if raw, err := hwio.ReadFile("/sys/class/net/" + port + "/mtu"); err == nil {
		n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		return n
	}
	return 0
}

func decodePush(raw json.RawMessage) (secPush, error) {
	var p secPush
	return p, json.Unmarshal(raw, &p)
}

// MACsec is show security macsec (this member): the stacking links, with
// the kernel's counters.
func (o *ops) MACsec() ([]cli.MACsecConn, error) {
	counters := map[string]map[string]any{}
	if out, err := (macsec.Linux{}).Output("ip", "-j", "-s", "macsec", "show"); err == nil {
		var devs []map[string]any
		if json.Unmarshal(out, &devs) == nil {
			for _, d := range devs {
				if n, ok := d["ifname"].(string); ok {
					counters[n] = d
				}
			}
		}
	}
	var out []cli.MACsecConn
	if o.stackSec == nil {
		return out, nil
	}
	for _, l := range o.stackSec.status() {
		name := l.Port
		if n, ok := o.names.Name(l.Port); ok {
			name = n
		}
		peer := fmt.Sprintf("member %d (%s)", l.Neighbor, l.PeerMAC)
		if l.PeerName != "" {
			peer = fmt.Sprintf("member %d, port %s (%s)", l.Neighbor, l.PeerName, l.PeerMAC)
		}
		cipher := stackCipher
		if l.Dev == "" || strings.HasPrefix(l.State, "plain") {
			cipher = "-" // plain
		}
		c := cli.MACsecConn{Member: o.member, Interface: name, Dev: l.Dev, CA: "stack", Cipher: cipher, State: l.State,
			TxAN: l.TxAN, KeySince: l.KeySince, Neighbour: peer, Counters: map[string]uint64{}}
		if len(l.RxANs) > 0 {
			c.RxSCs = []string{fmt.Sprintf("%s port 1, associations %v", l.PeerMAC, l.RxANs)}
		}
		macsecCounters(&c, counters[l.Dev])
		out = append(out, c)
	}
	// Ports secured with MKA (reference 5.15).
	cfg := o.model()
	if cfg == nil {
		return out, nil
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.MACsec.Ports)) {
		ca := cfg.MACsecPort(name)
		i := cfg.Interfaces[name]
		if ca == nil || i == nil || i.Member != o.member {
			continue
		}
		c := cli.MACsecConn{Member: o.member, Interface: name, CA: ca.Name, Cipher: ca.Cipher, TxAN: -1, Counters: map[string]uint64{}}
		port, _ := o.names.Linux(name)
		dev, secured := "", false
		if o.applier != nil {
			dev, secured = o.applier.dataNames(cfg)(name)
		}
		problem := ""
		if o.mka != nil {
			_, problem = o.mka.state(port)
		}
		switch {
		case secured:
			c.Dev, c.State = dev, "secured"
			macsecCounters(&c, counters[dev])
		case problem != "":
			c.State = "failed: " + problem
		default:
			c.State = "negotiating (no traffic until MKA secures the link)"
		}
		out = append(out, c)
	}
	return out, nil
}

// macsecCounters fills a connection from its device's `ip -j -s macsec`
// entry (nil: none).
func macsecCounters(c *cli.MACsecConn, d map[string]any) {
	if d == nil {
		return
	}
	if v, ok := d["sci"].(string); ok {
		c.TxSCI = strings.TrimPrefix(v, "0x")
	}
	if v, ok := d["offload"].(string); ok {
		c.Offload = v
	}
	for k, v := range d {
		if f, ok := v.(float64); ok {
			c.Counters[k] = uint64(f)
		}
	}
	// The receive counters are per receive SC.
	if scs, ok := d["rx_sc"].([]any); ok {
		for _, sc := range scs {
			if m, ok := sc.(map[string]any); ok {
				for k, v := range m {
					if f, ok := v.(float64); ok && strings.HasPrefix(k, "In") {
						c.Counters[k] += uint64(f)
					}
				}
			}
		}
	}
}
