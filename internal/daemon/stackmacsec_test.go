package daemon

import (
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/alarms"
	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/stack"
	"github.com/thxrben/cerium-switchd/pkg/macsec"
)

// plain marks links as not encrypted (with a reason).
func plain(links []stackLinkSpec, why string) []stackLinkSpec {
	out := append([]stackLinkSpec(nil), links...)
	for i := range out {
		out[i].Encrypt, out[i].Why = false, why
	}
	return out
}

// An offloading NIC gets the device offloaded; a driver that refuses keeps
// an auto link plain (no software encryption), an "on" link encrypts in
// software. Both raise an alarm.
func TestStackMACsecOffload(t *testing.T) {
	a, b, ka, _, _ := newSecPair(t)
	la := []stackLinkSpec{{Port: "ens19", Neighbor: 2, PeerMAC: "02:00:00:00:02:01", Name: "1/0/1", Encrypt: true, Offload: true}}
	lb := []stackLinkSpec{{Port: "ens20", Neighbor: 1, PeerMAC: "02:00:00:00:01:01", Name: "2/0/1", Encrypt: true, Offload: true}}
	for range 2 {
		a.sync(la)
		b.sync(lb)
	}
	if !ka.has("type macsec port 1 cipher gcm-aes-xpn-256 icvlen 16 encrypt on send_sci on validate strict offload mac") || a.DataDev("ens19") == "" {
		t.Fatalf("not offloaded:\n%s", strings.Join(ka.lines, "\n"))
	}
	if st := a.status(); len(st) != 1 || st[0].State != "secured (hardware)" {
		t.Fatalf("status %+v", st)
	}

	// The driver refuses, mode auto: plain, alarm, keys refused.
	a, b, ka, _, _ = newSecPair(t)
	ka.noOffload = true
	for range 2 {
		a.sync(la)
		b.sync(lb)
	}
	if a.DataDev("ens19") != "" || b.DataDev("ens20") != "" || len(a.alarms.List()) != 1 || ka.devs["msens19"] {
		t.Fatalf("refused offload, auto: a %q b %q alarms %v devs %v", a.DataDev("ens19"), b.DataDev("ens20"), a.alarms.List(), ka.devs)
	}
	if st := a.status(); st[0].State != "plain (the driver refused the offload)" {
		t.Fatalf("status %+v", st)
	}
	key, err := macsec.NewSAK(stackCipher, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.receive(2, secPush{SenderMAC: "02:00:00:00:02:01", Key: key}); err == nil {
		t.Fatal("a refused link accepted a key")
	}

	// The same with "on" (software allowed): encrypted in software.
	a, b, ka, _, _ = newSecPair(t)
	ka.noOffload = true
	sw := append([]stackLinkSpec(nil), la...)
	sw[0].Software = true
	lbs := append([]stackLinkSpec(nil), lb...)
	lbs[0].Software = true
	for range 2 {
		a.sync(sw)
		b.sync(lbs)
	}
	if a.DataDev("ens19") != "msens19" || len(a.alarms.List()) != 1 {
		t.Fatalf("software fallback: %q alarms %v", a.DataDev("ens19"), a.alarms.List())
	}
	if st := a.status(); st[0].State != "secured (software)" {
		t.Fatalf("status %+v", st)
	}
}

// Two links between the same members: one encrypted, one plain (mixed
// operation, migration); each keeps its own state.
func TestStackMACsecMixedLinks(t *testing.T) {
	a, b, _, _, _ := newSecPair(t)
	la := []stackLinkSpec{
		{Port: "ens19", Neighbor: 2, PeerMAC: "02:00:00:00:02:01", Encrypt: true, Offload: true},
		{Port: "ens21", Neighbor: 2, PeerMAC: "02:00:00:00:02:02", Why: "auto: 1/0/2 cannot offload"},
	}
	lb := []stackLinkSpec{
		{Port: "ens20", Neighbor: 1, PeerMAC: "02:00:00:00:01:01", Encrypt: true, Offload: true},
		{Port: "ens22", Neighbor: 1, PeerMAC: "02:00:00:00:01:02", Why: "auto: 1/0/2 cannot offload"},
	}
	for range 2 {
		a.sync(la)
		b.sync(lb)
	}
	if a.DataDev("ens19") != "msens19" || a.DataDev("ens21") != "" {
		t.Fatalf("mixed: %q %q", a.DataDev("ens19"), a.DataDev("ens21"))
	}
	st := a.status()
	if len(st) != 2 || st[1].State != "plain (auto: 1/0/2 cannot offload)" {
		t.Fatalf("status %+v", st)
	}
	// Migration done: the old (plain) port is no longer designated.
	a.sync(la[:1])
	if a.DataDev("ens19") != "msens19" || len(a.status()) != 1 {
		t.Fatal("the encrypted link changed when the plain one went")
	}
}

// Both members decide each link the same way from the replicated
// configuration and both ports' offload (reference 5.2).
func TestDecideLinkBothEnds(t *testing.T) {
	tree, err := config.ParseSet("set virtual-chassis member 1\nset virtual-chassis member 2\nset virtual-chassis macsec interface 2/0/3 mode on\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := model.Build(tree, nil)
	mac := net.HardwareAddr{2, 0, 0, 0, 0, 1}
	for _, c := range []struct {
		aPort, bPort        string
		aOff, bOff, encrypt bool
	}{
		{"0/1", "0/1", true, true, true},
		{"0/1", "0/1", true, false, false},
		{"0/2", "0/3", false, false, true}, // 2/0/3 is on: software
	} {
		ea := decideLink(cfg, 1, stack.StackLink{Linux: "x", Neighbor: 2, NeighborMAC: mac, Port: c.aPort, PeerPort: c.bPort, Offload: c.aOff, PeerOffload: c.bOff}, 1)
		eb := decideLink(cfg, 2, stack.StackLink{Linux: "y", Neighbor: 1, NeighborMAC: mac, Port: c.bPort, PeerPort: c.aPort, Offload: c.bOff, PeerOffload: c.aOff}, 1)
		if ea.Encrypt != c.encrypt || eb.Encrypt != c.encrypt || ea.Software != eb.Software {
			t.Errorf("%+v: member 1 %+v, member 2 %+v", c, ea, eb)
		}
	}
}

// fakeMACsecKernel records the commands; the keys of every command.
type fakeMACsecKernel struct {
	mu    sync.Mutex
	lines []string
	devs  map[string]bool
	// noOffload: the driver refuses "offload mac|phy".
	noOffload bool
}

func (k *fakeMACsecKernel) Batch(lines []string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, l := range lines {
		if strings.HasPrefix(l, "link add ") {
			if k.noOffload && strings.Contains(l, " offload ") {
				return errors.New("RTNETLINK answers: Operation not supported")
			}
			name := strings.Fields(l)[5]
			if k.devs[name] {
				return errors.New("RTNETLINK answers: File exists")
			}
			k.devs[name] = true
		}
		if strings.HasPrefix(l, "link del ") {
			delete(k.devs, strings.Fields(l)[2])
		}
		k.lines = append(k.lines, l)
	}
	return nil
}

func (k *fakeMACsecKernel) Run(name string, args ...string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.lines = append(k.lines, name+" "+strings.Join(args, " "))
	return nil
}

func (k *fakeMACsecKernel) has(sub string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, l := range k.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func newSecPair(t *testing.T) (a, b *stackMACsec, ka, kb *fakeMACsecKernel, clock *time.Time) {
	now := time.Unix(1000000, 0)
	clock = &now
	mk := func(member int, k *fakeMACsecKernel, mac string) *stackMACsec {
		return &stackMACsec{member: member, log: slog.New(slog.DiscardHandler), alarms: &alarms.Set{}, k: k,
			portMAC: func(string) string { return mac }, portMTU: func(string) int { return 9000 },
			now: func() time.Time { return *clock }}
	}
	ka = &fakeMACsecKernel{devs: map[string]bool{}}
	kb = &fakeMACsecKernel{devs: map[string]bool{}}
	a = mk(1, ka, "02:00:00:00:01:01")
	b = mk(2, kb, "02:00:00:00:02:01")
	a.push = func(n int, req secPush) (secReply, error) { return b.receive(1, req) }
	b.push = func(n int, req secPush) (secReply, error) { return a.receive(2, req) }
	return
}

func TestStackMACsec(t *testing.T) {
	a, b, ka, kb, clock := newSecPair(t)
	la := []stackLinkSpec{{Port: "ens19", Index: 3, Neighbor: 2, PeerMAC: "02:00:00:00:02:01", Encrypt: true}}
	lb := []stackLinkSpec{{Port: "ens20", Index: 4, Neighbor: 1, PeerMAC: "02:00:00:00:01:01", Encrypt: true}}
	// Rounds of the 100 ms loop: the first push of member 1 comes before
	// member 2 knows the link; member 2's push asks for member 1's key.
	for range 2 {
		a.sync(la)
		b.sync(lb)
	}
	if a.DataDev("ens19") != "msens19" || b.DataDev("ens20") != "msens20" {
		t.Fatalf("not secured: a %q b %q (%+v %+v)", a.DataDev("ens19"), b.DataDev("ens20"), a.links["ens19"], b.links["ens20"])
	}
	for _, want := range []string{"link add link ens19 name msens19 type macsec port 1 cipher gcm-aes-xpn-256", "encodingsa 0",
		"macsec add msens19 tx sa 0 xpn 1 on salt", "ssci 1 key", "macsec add msens19 rx port 1 address 02:00:00:00:02:01 on",
		"tc filter replace dev ens19 ingress pref 49153 protocol all matchall action drop", "link set msens19 mtu 8968"} {
		if !ka.has(want) {
			t.Errorf("member 1 lacks %q:\n%s", want, strings.Join(ka.lines, "\n"))
		}
	}
	if !kb.has("ssci 2 key") {
		t.Error("member 2 sends with ssci 2")
	}
	// An hour later: new keys on the next AN, the old transmit SA goes.
	*clock = clock.Add(stackRekey + time.Second)
	a.sync(la)
	if a.links["ens19"].tx.AN != 1 || !ka.has("encodingsa 1") || !ka.has("macsec del msens19 tx sa 0") {
		t.Fatalf("rekey: %+v", a.links["ens19"].tx)
	}
	if got := b.links["ens20"].rx; len(got) != 2 || got[0] != 1 {
		t.Fatalf("member 2 receive SAs %v", got)
	}
	// Plain now (e.g. mode off): the device goes, the filter too.
	a.sync(plain(la, "off"))
	if a.DataDev("ens19") != "" || !ka.has("link del msens19") || !ka.has("tc filter del dev ens19 ingress pref 49153") {
		t.Fatal("not removed")
	}
}

func TestStackMACsecOldNeighbour(t *testing.T) {
	a, _, _, _, _ := newSecPair(t)
	a.push = func(int, secPush) (secReply, error) {
		return secReply{}, errors.New(`unknown operation "stack-macsec"`)
	}
	a.sync([]stackLinkSpec{{Port: "ens19", Neighbor: 2, PeerMAC: "02:00:00:00:02:01", Encrypt: true}})
	if a.DataDev("ens19") != "" || len(a.alarms.List()) != 1 || !a.links["ens19"].plain {
		t.Fatalf("old neighbour: %+v %v", a.links["ens19"], a.alarms.List())
	}
	if st := a.status(); len(st) != 1 || st[0].State != "plain (neighbour without MACsec)" {
		t.Fatalf("status %+v", st)
	}
}

func TestStackMACsecRestart(t *testing.T) {
	a, b, _, kb, clock := newSecPair(t)
	la := []stackLinkSpec{{Port: "ens19", Neighbor: 2, PeerMAC: "02:00:00:00:02:01", Encrypt: true}}
	lb := []stackLinkSpec{{Port: "ens20", Neighbor: 1, PeerMAC: "02:00:00:00:01:01", Encrypt: true}}
	for range 2 {
		a.sync(la)
		b.sync(lb)
	}
	// switchd of member 2 restarts: the device stays (traffic flows), the
	// new run asks for and sends new keys.
	b2 := &stackMACsec{member: 2, log: b.log, alarms: &alarms.Set{}, k: kb, portMAC: b.portMAC, portMTU: b.portMTU, now: b.now}
	b2.push = func(n int, req secPush) (secReply, error) { return a.receive(2, req) }
	a.push = func(n int, req secPush) (secReply, error) { return b2.receive(1, req) }
	b2.sync(lb)
	if b2.DataDev("ens20") != "msens20" {
		t.Fatal("the restarted member stopped using the device")
	}
	*clock = clock.Add(time.Second)
	a.sync(la) // a re-sends its key: b2 asked for it
	if l := a.links["ens19"]; !l.secured() || l.tx.AN != 1 {
		t.Fatalf("member 1 did not renew its key: %+v", a.links["ens19"].tx)
	}
}
