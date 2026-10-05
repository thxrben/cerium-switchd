package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNIC creates /sys/devices/<devPath>/net/<linux> and the class link.
func fakeNIC(t *testing.T, sys, devPath, linux, driver string, devPort int) {
	t.Helper()
	dev := filepath.Join(sys, "devices", devPath)
	netDir := filepath.Join(dev, "net", linux)
	if err := os.MkdirAll(netDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(netDir, "dev_port"), []byte{byte('0' + devPort), '\n'}, 0o644)
	os.WriteFile(filepath.Join(netDir, "address"), []byte("02:00:00:00:00:01\n"), 0o644)
	os.Symlink(dev, filepath.Join(netDir, "device"))
	drv := filepath.Join(sys, "bus", "pci", "drivers", driver)
	os.MkdirAll(drv, 0o755)
	os.Symlink(drv, filepath.Join(dev, "driver"))
	os.MkdirAll(filepath.Join(sys, "class", "net"), 0o755)
	if err := os.Symlink(netDir, filepath.Join(sys, "class", "net", linux)); err != nil {
		t.Fatal(err)
	}
}

func names(n *Naming) map[string]string {
	out := map[string]string{}
	for _, p := range n.Ports() {
		out[p.Linux] = p.Name
	}
	return out
}

func TestNamingPhysicalBox(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "sys")
	// physw4: onboard at 07:00 is enumerated first by the kernel, but
	// numbering follows PCI order.
	fakeNIC(t, sys, "pci0000:00/0000:00:1c.4/0000:07:00.0", "eno1", "r8169", 0)
	for i, n := range []string{"enp1s0f0", "enp1s0f1", "enp1s0f2", "enp1s0f3"} {
		fakeNIC(t, sys, "pci0000:00/0000:00:1c.0/0000:01:00."+string(rune('0'+i)), n, "tg3", 0)
	}
	fakeNIC(t, sys, "pci0000:00/0000:00:1c.2/0000:04:00.1", "enp4s0f1", "igb", 0)
	fakeNIC(t, sys, "pci0000:00/0000:00:1c.2/0000:04:00.0", "enp4s0f0", "igb", 0)
	// A USB adapter, a virtio NIC below a PCI device, a virtual device.
	fakeNIC(t, sys, "pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0", "enx0011", "r8152", 0)
	fakeNIC(t, sys, "pci0000:00/0000:00:12.0/virtio3", "ens18", "virtio_net", 0)
	os.MkdirAll(filepath.Join(sys, "class", "net", "lo"), 0o755)
	// SR-IOV VF: not a switch port.
	fakeNIC(t, sys, "pci0000:00/0000:00:1c.0/0000:01:10.0", "enp1s0v0", "igbvf", 0)
	os.MkdirAll(filepath.Join(sys, "devices", "pci0000:00/0000:00:1c.0/0000:01:10.0", "physfn"), 0o755)

	state := filepath.Join(dir, "names.json")
	n := &Naming{SysRoot: sys, StateFile: state, Member: 1}
	if changed, err := n.Refresh(); err != nil || !changed {
		t.Fatalf("Refresh: %v %v", changed, err)
	}
	got := names(n)
	// PCI order: 0000:00:12.0 (virtio) < 0000:01:00 < 0000:04:00 < 0000:07:00;
	// then USB.
	want := map[string]string{
		"ens18":    "1/0/0",
		"enp1s0f0": "1/1/0", "enp1s0f1": "1/1/1", "enp1s0f2": "1/1/2", "enp1s0f3": "1/1/3",
		"enp4s0f0": "1/2/0", "enp4s0f1": "1/2/1",
		"eno1":    "1/3/0",
		"enx0011": "1/4/0",
	}
	if len(got) != len(want) {
		t.Errorf("ports: %v", got)
	}
	for l, w := range want {
		if got[l] != w {
			t.Errorf("%s = %q, want %q", l, got[l], w)
		}
	}
	if l, ok := n.Linux("1/2/1"); !ok || l != "enp4s0f1" {
		t.Errorf("Linux(1/2/1) = %q %v", l, ok)
	}
	if nm, ok := n.Name("eno1"); !ok || nm != "1/3/0" {
		t.Errorf("Name(eno1) = %q %v", nm, ok)
	}
	for _, p := range n.Ports() {
		if p.Linux == "enp4s0f1" && (p.Driver != "igb" || p.Bus != "0000:04:00.1" || p.Card != "pci:0000:04:00") {
			t.Errorf("port details: %+v", p)
		}
	}

	// A card added later in a lower slot gets the next number; nothing is
	// renumbered, also after a restart (new Naming on the same state).
	fakeNIC(t, sys, "pci0000:00/0000:00:1b.0/0000:02:00.0", "enp2s0", "ixgbe", 0)
	n2 := &Naming{SysRoot: sys, StateFile: state, Member: 1}
	n2.Refresh()
	got = names(n2)
	if got["enp2s0"] != "1/5/0" || got["enp1s0f0"] != "1/1/0" || got["eno1"] != "1/3/0" {
		t.Errorf("after adding a card: %v", got)
	}
	// A removed card keeps its number reserved.
	os.Remove(filepath.Join(sys, "class", "net", "enp2s0"))
	n2.Refresh()
	fakeNIC(t, sys, "pci0000:00/0000:00:1b.0/0000:03:00.0", "enp3s0", "ixgbe", 0)
	n2.Refresh()
	if got := names(n2); got["enp3s0"] != "1/6/0" {
		t.Errorf("card numbers reused: %v", got)
	}
	if changed, _ := n2.Refresh(); changed {
		t.Error("Refresh without changes reported a change")
	}
}

// Ports on one PCI function are ordered by dev_port (e.g. mlx4, DSA).
func TestNamingDevPort(t *testing.T) {
	sys := filepath.Join(t.TempDir(), "sys")
	fakeNIC(t, sys, "pci0000:00/0000:00:01.0/0000:05:00.0", "p1", "mlx4_core", 1)
	fakeNIC(t, sys, "pci0000:00/0000:00:01.0/0000:05:00.0/x", "p0", "mlx4_core", 0)
	n := &Naming{SysRoot: sys, Member: 2}
	n.Refresh()
	if got := names(n); got["p0"] != "2/0/0" || got["p1"] != "2/0/1" {
		t.Errorf("%v", got)
	}
}

func setMAC(t *testing.T, sys, linux, mac string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(sys, "class", "net", linux, "address"), []byte(mac+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Card lifecycle: an absent card keeps its number and is listed; a card
// that moved slots is recognised by its MACs and can take its old number
// back; a card that changed model is noted; forget releases a number.
func TestCardLifecycle(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "sys")
	fakeNIC(t, sys, "pci0000:00/0000:00:1c.0/0000:01:00.0", "enp1s0f0", "tg3", 0)
	fakeNIC(t, sys, "pci0000:00/0000:00:1c.0/0000:01:00.1", "enp1s0f1", "tg3", 0)
	setMAC(t, sys, "enp1s0f0", "02:00:00:00:01:00")
	setMAC(t, sys, "enp1s0f1", "02:00:00:00:01:01")
	fakeNIC(t, sys, "pci0000:00/0000:00:1c.4/0000:07:00.0", "eno1", "r8169", 0)
	setMAC(t, sys, "eno1", "02:00:00:00:07:00")
	state := filepath.Join(dir, "names.json")
	n := &Naming{SysRoot: sys, StateFile: state, Member: 1}
	n.Refresh()

	// The tg3 card moves from 01:00 to 03:00.
	os.Remove(filepath.Join(sys, "class", "net", "enp1s0f0"))
	os.Remove(filepath.Join(sys, "class", "net", "enp1s0f1"))
	fakeNIC(t, sys, "pci0000:00/0000:00:1c.1/0000:03:00.0", "enp3s0f0", "tg3", 0)
	fakeNIC(t, sys, "pci0000:00/0000:00:1c.1/0000:03:00.1", "enp3s0f1", "tg3", 0)
	setMAC(t, sys, "enp3s0f0", "02:00:00:00:01:00")
	setMAC(t, sys, "enp3s0f1", "02:00:00:00:01:01")
	n2 := &Naming{SysRoot: sys, StateFile: state, Member: 1}
	n2.Refresh()
	cards := n2.Cards()
	if len(cards) != 3 || cards[0].Present || cards[0].Driver != "tg3" || cards[0].Ports != 2 || cards[2].MovedFrom != 0 {
		t.Fatalf("cards after the move: %+v", cards)
	}
	if got := names(n2); got["enp3s0f0"] != "1/2/0" {
		t.Fatalf("moved card named %v", got)
	}
	if err := n2.Renumber(2, 1); err == nil {
		t.Error("renumbered onto a present card")
	}
	if err := n2.Renumber(2, 0); err != nil {
		t.Fatal(err)
	}
	n2.Refresh()
	if got := names(n2); got["enp3s0f0"] != "1/0/0" || got["enp3s0f1"] != "1/0/1" || got["eno1"] != "1/1/0" {
		t.Errorf("after renumber: %v", got)
	}
	if cs := n2.Cards(); len(cs) != 2 || !cs[0].Present || cs[0].MovedFrom != -1 {
		t.Errorf("cards after renumber: %+v", cs)
	}

	// The onboard NIC is replaced by another model in the same slot.
	os.Remove(filepath.Join(sys, "class", "net", "eno1"))
	os.RemoveAll(filepath.Join(sys, "devices", "pci0000:00/0000:00:1c.4/0000:07:00.0"))
	fakeNIC(t, sys, "pci0000:00/0000:00:1c.4/0000:07:00.0", "eno1", "igb", 0)
	fakeNIC(t, sys, "pci0000:00/0000:00:1c.4/0000:07:00.1", "eno2", "igb", 0)
	n3 := &Naming{SysRoot: sys, StateFile: state, Member: 1}
	n3.Refresh()
	for _, c := range n3.Cards() {
		if c.Number == 1 && !strings.Contains(c.Note, "was r8169 with 1 ports, now igb with 2") {
			t.Errorf("model change not noted: %+v", c)
		}
	}
	// Forget: only absent cards.
	if err := n3.Forget(1); err == nil {
		t.Error("forgot a present card")
	}
	os.Remove(filepath.Join(sys, "class", "net", "eno1"))
	os.Remove(filepath.Join(sys, "class", "net", "eno2"))
	n3.Refresh()
	if err := n3.Forget(1); err != nil {
		t.Fatal(err)
	}
	if cs := n3.Cards(); len(cs) != 1 {
		t.Errorf("after forget: %+v", cs)
	}
}
