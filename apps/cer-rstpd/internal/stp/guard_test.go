package stp

import (
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeGuardIO struct {
	mu sync.Mutex
	rx map[string]func([]byte)
}

func (f *fakeGuardIO) Listen(dev string, rx func([]byte)) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rx[dev] = rx
	return func() { f.mu.Lock(); delete(f.rx, dev); f.mu.Unlock() }, nil
}

func (f *fakeGuardIO) send(dev string, frame []byte) {
	f.mu.Lock()
	rx := f.rx[dev]
	f.mu.Unlock()
	if rx != nil {
		rx(frame)
	}
}

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestGuard(t *testing.T) {
	fio := &fakeGuardIO{rx: map[string]func([]byte){}}
	state := filepath.Join(t.TempDir(), "bpdu.json")
	g := NewGuard(fio, state, quietLog)
	var pub map[string]Blocked
	var alarms []string
	g.Publish = func(m map[string]Blocked) { pub = m }
	g.Alarm = func(s string) { alarms = append(alarms, s) }
	now := time.Unix(1000, 0)
	g.now = func() time.Time { return now }
	g.SetConfig(map[string]string{"1/0/1": "eth1", "ae1": "ae1"}, 0)
	bpdu := append([]byte{0x01, 0x80, 0xc2, 0, 0, 0, 0x02, 0xaa, 0, 0, 0, 1}, make([]byte, 40)...)
	lldp := append([]byte{0x01, 0x80, 0xc2, 0, 0, 0x0e, 0x02, 0xaa, 0, 0, 0, 1}, make([]byte, 40)...)
	fio.send("eth1", lldp)
	if len(pub) != 0 {
		t.Fatal("LLDP blocked the port")
	}
	fio.send("eth1", bpdu)
	if b, ok := pub["1/0/1"]; !ok || b.From != "02:aa:00:00:00:01" || !b.Until.IsZero() {
		t.Fatalf("published %+v", pub)
	}
	if len(alarms) != 1 {
		t.Fatalf("alarms %v", alarms)
	}
	fio.send("eth1", bpdu) // already blocked: one alarm only
	if len(alarms) != 1 {
		t.Fatalf("alarms %v", alarms)
	}
	// A restart keeps the block.
	g2 := NewGuard(fio, state, quietLog)
	if _, ok := g2.Blocked()["1/0/1"]; !ok {
		t.Fatal("block lost across a restart")
	}
	if g.Clear("1/0/1") != 1 || len(pub) != 0 {
		t.Fatalf("clear: %+v", pub)
	}
	// With a timeout the block ends by itself.
	g.SetConfig(map[string]string{"1/0/1": "eth1", "ae1": "ae1"}, 30*time.Second)
	fio.send("ae1", bpdu)
	if b := pub["ae1"]; b.Until != now.Add(30*time.Second) {
		t.Fatalf("until %v", b.Until)
	}
	now = now.Add(31 * time.Second)
	g.Tick()
	if len(pub) != 0 {
		t.Fatalf("after the timeout: %+v", pub)
	}
	// A port no longer listed is no longer blocked.
	fio.send("eth1", bpdu)
	g.SetConfig(map[string]string{"ae1": "ae1"}, 0)
	if len(pub) != 0 {
		t.Fatalf("unlisted port still blocked: %+v", pub)
	}
}
