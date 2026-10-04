package daemon

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/alarms"
)

// fakeMACsecKernel records the commands; the keys of every command.
type fakeMACsecKernel struct {
	mu    sync.Mutex
	lines []string
	devs  map[string]bool
}

func (k *fakeMACsecKernel) Batch(lines []string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, l := range lines {
		if strings.HasPrefix(l, "link add ") {
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
	la := []stackLinkSpec{{Port: "ens19", Index: 3, Neighbor: 2, PeerMAC: "02:00:00:00:02:01"}}
	lb := []stackLinkSpec{{Port: "ens20", Index: 4, Neighbor: 1, PeerMAC: "02:00:00:00:01:01"}}
	// Rounds of the 100 ms loop: the first push of member 1 comes before
	// member 2 knows the link; member 2's push asks for member 1's key.
	for range 2 {
		a.sync(la, true)
		b.sync(lb, true)
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
	a.sync(la, true)
	if a.links["ens19"].tx.AN != 1 || !ka.has("encodingsa 1") || !ka.has("macsec del msens19 tx sa 0") {
		t.Fatalf("rekey: %+v", a.links["ens19"].tx)
	}
	if got := b.links["ens20"].rx; len(got) != 2 || got[0] != 1 {
		t.Fatalf("member 2 receive SAs %v", got)
	}
	// Disabled: the device goes, the filter too.
	a.sync(la, false)
	if a.DataDev("ens19") != "" || !ka.has("link del msens19") || !ka.has("tc filter del dev ens19 ingress pref 49153") {
		t.Fatal("not removed")
	}
}

func TestStackMACsecOldNeighbour(t *testing.T) {
	a, _, _, _, _ := newSecPair(t)
	a.push = func(int, secPush) (secReply, error) { return secReply{}, errors.New(`unknown operation "stack-macsec"`) }
	a.sync([]stackLinkSpec{{Port: "ens19", Neighbor: 2, PeerMAC: "02:00:00:00:02:01"}}, true)
	if a.DataDev("ens19") != "" || len(a.alarms.List()) != 1 || !a.links["ens19"].plain {
		t.Fatalf("old neighbour: %+v %v", a.links["ens19"], a.alarms.List())
	}
	if st := a.status(); len(st) != 1 || st[0].State != "plain (neighbour without MACsec)" {
		t.Fatalf("status %+v", st)
	}
}

func TestStackMACsecRestart(t *testing.T) {
	a, b, _, kb, clock := newSecPair(t)
	la := []stackLinkSpec{{Port: "ens19", Neighbor: 2, PeerMAC: "02:00:00:00:02:01"}}
	lb := []stackLinkSpec{{Port: "ens20", Neighbor: 1, PeerMAC: "02:00:00:00:01:01"}}
	for range 2 {
		a.sync(la, true)
		b.sync(lb, true)
	}
	// switchd of member 2 restarts: the device stays (traffic flows), the
	// new run asks for and sends new keys.
	b2 := &stackMACsec{member: 2, log: b.log, alarms: &alarms.Set{}, k: kb, portMAC: b.portMAC, portMTU: b.portMTU, now: b.now}
	b2.push = func(n int, req secPush) (secReply, error) { return a.receive(2, req) }
	a.push = func(n int, req secPush) (secReply, error) { return b2.receive(1, req) }
	b2.sync(lb, true)
	if b2.DataDev("ens20") != "msens20" {
		t.Fatal("the restarted member stopped using the device")
	}
	*clock = clock.Add(time.Second)
	a.sync(la, true) // a re-sends its key: b2 asked for it
	if l := a.links["ens19"]; !l.secured() || l.tx.AN != 1 {
		t.Fatalf("member 1 did not renew its key: %+v", a.links["ens19"].tx)
	}
}
