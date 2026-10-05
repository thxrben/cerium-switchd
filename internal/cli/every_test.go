package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/inventory"
	"github.com/thxrben/cerium-switchd/internal/usbstore"
)

// allOps implements every optional interface of Ops (RIB, BGP, OSPF and
// the base fake).
type allOps struct {
	bgpOps
	ospfOps
}

func (allOps) BFDSessions() ([]BFDSession, error) { return bfdOps{}.BFDSessions() }

func (allOps) USBList(dir string) (usbstore.Listing, error) {
	return usbstore.Listing{Info: usbstore.Info{Stick: usbstore.Stick{Disk: "sdb", Vendor: "Kingston", Model: "DT", Size: 16e9},
		Device: "/dev/sdb1", FSType: "vfat"}, Dir: ".", Entries: []usbstore.Entry{{Name: "configs", Dir: true, ModTime: time.Now()},
		{Name: "sw1.conf", Size: 1234, ModTime: time.Now()}}}, nil
}

func (allOps) USBEject() (usbstore.Stick, error) {
	return usbstore.Stick{Disk: "sdb", Vendor: "Kingston"}, nil
}

func (allOps) Optics(iface string) ([]OpticsPort, error) { return opticsOps{}.Optics(iface) }

func (allOps) Environment() ([]EnvSensor, error) {
	return []EnvSensor{{Member: 1, Sensor: inventory.Sensor{Class: "Temp", Chip: "coretemp", Label: "Core 0", Value: 40, Status: "OK"}}}, nil
}

func (allOps) WebManagement() (WebStatus, error) {
	st := WebStatus{Configured: true, Running: true, Member: 1, Port: 443, VRF: "mgmt", Certificate: "temporary self-signed",
		Generated: time.Now(), Fingerprint: "AB:CD", Pin: "sha256//x"}
	st.Upload.Version, st.Upload.Size, st.Upload.User, st.Upload.Time = "1.1", 300<<20, "admin", time.Now()
	return st, nil
}

func (allOps) Memory() (MemoryStatus, error) {
	return MemoryStatus{Member: 1, Slotted: true, RAM: 6 << 30, Slots: 1167, System: 9, Update: 129, Allocatable: 1029,
		Purposes: []MemoryPurpose{{Name: "bgp-ipv4", Bytes: 2100, PerSlot: 1997, Slots: 300, Capacity: 599100, AllSlots: 2000000, Used: 10},
			{Name: "ndp", Bytes: 512, PerSlot: 8192, Used: -1}}}, nil
}

func (allOps) MACsec() ([]MACsecConn, error) {
	return []MACsecConn{{Member: 1, Interface: "1/1/0", Dev: "msens19", CA: "stack", Cipher: "gcm-aes-xpn-256", State: "secured",
		TxSCI: "020000000101" + "0001", TxAN: 1, RxSCs: []string{"02:00:00:00:02:01 port 1, associations [1 0]"},
		KeySince: time.Now(), Neighbour: "member 2", Counters: map[string]uint64{"OutPktsEncrypted": 5}}}, nil
}

func (allOps) Alarms() ([]Alarm, error) {
	return []Alarm{{Member: 1, Class: "Major", Text: "x", Since: time.Now()}}, nil
}

// Every operational command runs, bare and with "?", against fakes:
// no panic, no internal error, no broken format verb, and nothing blocks.
func TestEveryCommand(t *testing.T) {
	skip := map[string]bool{"configure": true, "exit": true, "quit": true, "start shell": true}
	var lines []string
	var walk func(prefix []string, cs []*command)
	walk = func(prefix []string, cs []*command) {
		for _, c := range cs {
			p := append(append([]string{}, prefix...), c.name)
			line := strings.Join(p, " ")
			if c.run != nil && !skip[line] {
				lines = append(lines, line)
			}
			walk(p, c.sub)
		}
	}
	walk(nil, operational)
	if len(lines) < 100 {
		t.Fatalf("only %d commands found", len(lines))
	}
	for _, line := range lines {
		for _, l := range []string{line, line + " ?"} {
			ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
			ts.sh.env.Ops = &allOps{}
			done := make(chan string, 1)
			go func() { done <- ts.sh.Execute(context.Background(), l, ts.term).Output }()
			select {
			case out := <-done:
				if strings.Contains(out, "internal error") || strings.Contains(out, "%!") {
					t.Errorf("%q:\n%s", l, out)
				}
				if testing.Verbose() && !strings.HasSuffix(l, "?") && strings.Contains(out, "error") {
					t.Logf("%q: %s", l, strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
				}
			case <-time.After(5 * time.Second):
				t.Errorf("%q blocks", l)
			}
		}
	}
}
