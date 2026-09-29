//go:build lab

// Package lab runs end-to-end tests against the Proxmox lab (lab/README.md).
// switchd must be deployed on sw1 without --dry-run:
//
//	make cross && (cd lab && ansible-playbook deploy.yml -u root -l sw1 -e switchd_args=)
//	go test -tags lab ./test/lab -v
//
// Test hosts are network namespaces "h" on srv1, sw2 and sw3 holding the
// NIC that is cabled to sw1; the management NICs are never touched.
package lab

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const sw1 = "10.5.176.95"

// A test host: the VM and its NIC towards sw1, and the sw1 port it is on.
type host struct {
	name, vm, nic, sw1Port string
	n                      int // host number in 192.168.x.n
}

var (
	hSrv1 = host{"srv1", "10.5.176.101", "ens19", "1/ens23", 1}
	hSw2  = host{"sw2", "10.5.176.96", "ens19", "1/ens19", 2}
	hSw3  = host{"sw3", "10.5.176.97", "ens23", "1/ens2", 3}
	hosts = []host{hSrv1, hSw2, hSw3}
)

func ssh(addr, cmd string) (string, error) {
	out, err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "root@"+addr, cmd).CombinedOutput()
	return string(out), err
}

func mustSSH(t *testing.T, addr, cmd string) string {
	t.Helper()
	out, err := ssh(addr, cmd)
	if err != nil {
		t.Fatalf("%s: %s: %v\n%s", addr, cmd, err, out)
	}
	return out
}

// bondHost is a namespace "b" on sw2 bonding its peer1/peer2 NICs, which
// are cabled to sw1's ens21/ens22.
var bondHost = host{"sw2-bond", "10.5.176.96", "bond0", "ae1", 22}

func setupBondHost(t *testing.T, mtu int) {
	t.Helper()
	mustSSH(t, bondHost.vm, fmt.Sprintf(`ip netns add b 2>/dev/null; for i in ens21 ens22; do ip link set $i netns b 2>/dev/null; done
ip -n b link del bond0 2>/dev/null; ip -n b link set lo up
ip -n b link add bond0 type bond mode balance-xor xmit_hash_policy layer3+4 miimon 100
for i in ens21 ens22; do ip -n b link set $i down; ip -n b link set $i master bond0; done
ip -n b link set bond0 mtu %d up; ip -n b addr add 192.168.1.22/24 dev bond0`, mtu))
}

// hostCmd runs cmd inside the test namespace of h.
func hostCmd(t *testing.T, h host, cmd string) string {
	return mustSSH(t, h.vm, "ip netns exec h sh -c '"+cmd+"'")
}

// setupHost (re)creates the namespace with the NIC untagged and optional
// VLAN sub-interfaces: address 192.168.<vid>.<n> on each, 192.168.1.<n>
// untagged.
func setupHost(t *testing.T, h host, vids ...int) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "ip netns add h 2>/dev/null; ip link set %s netns h 2>/dev/null; ip -n h link set lo up;", h.nic)
	fmt.Fprintf(&b, "for l in $(ip -n h -o link show type vlan | cut -d: -f2 | cut -d@ -f1); do ip -n h link del $l; done;")
	fmt.Fprintf(&b, "ip -n h addr flush dev %[1]s; ip -n h link set %[1]s mtu 1500 up; ip -n h addr add 192.168.1.%[2]d/24 dev %[1]s;", h.nic, h.n)
	for _, v := range vids {
		fmt.Fprintf(&b, "ip -n h link add link %[1]s name %[1]s.%[2]d type vlan id %[2]d; ip -n h addr add 192.168.%[2]d.%[3]d/24 dev %[1]s.%[2]d; ip -n h link set %[1]s.%[2]d up;", h.nic, v, h.n)
	}
	b.WriteString("ip -n h neigh flush all")
	mustSSH(t, h.vm, b.String())
}

// configure replaces sw1's configuration with setLines and commits it
// (commit + confirm).
func configure(t *testing.T, setLines string) {
	t.Helper()
	base := "set system host-name sw1\n"
	mustSSH(t, sw1, "cat > /root/lab.set <<'EOF'\n"+base+setLines+"\nEOF")
	out := mustSSH(t, sw1, `swcli -c "configure
load override lab.set
commit
commit
exit"`)
	if strings.Contains(out, "error") || !strings.Contains(out, "commit complete") {
		t.Fatalf("commit failed:\n%s", out)
	}
	time.Sleep(1500 * time.Millisecond) // carrier after admin up
}

func access(port, vlan string) string {
	return fmt.Sprintf("set interfaces %s unit 0 family ethernet-switching vlan members %s\n", port, vlan)
}

const vlans = "set vlans v10 vlan-id 10\nset vlans v20 vlan-id 20\nset vlans v30 vlan-id 30\n"

// reach reports whether from can ping to (addresses 192.168.<net>.x).
func reach(t *testing.T, from, to host, net int) bool {
	t.Helper()
	return reachSize(t, from, to, net, 56)
}

// reachSize pings with a payload of size bytes and the don't-fragment bit.
func reachSize(t *testing.T, from, to host, net, size int) bool {
	t.Helper()
	ns := "h"
	if from == bondHost {
		ns = "b"
	}
	_, err := ssh(from.vm, fmt.Sprintf("ip netns exec %s ping -M do -s %d -c2 -i0.2 -W1 192.168.%d.%d", ns, size, net, to.n))
	return err == nil
}

func TestMain(m *testing.M) {
	if out, err := ssh(sw1, "systemctl is-active switchd; grep -c -- --dry-run /etc/default/switchd"); !strings.HasPrefix(out, "active\n0") {
		fmt.Fprintf(os.Stderr, "switchd must run on sw1 without --dry-run (%v): %s\n", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestAccessIsolation(t *testing.T) {
	for _, h := range hosts {
		setupHost(t, h)
	}
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+access(hSw3.sw1Port, "v10")+access(hSw2.sw1Port, "v20"))
	if !reach(t, hSrv1, hSw3, 1) {
		t.Error("srv1 cannot reach sw3 in the same access VLAN")
	}
	if reach(t, hSrv1, hSw2, 1) {
		t.Error("srv1 reaches sw2 in another VLAN")
	}
}

func TestTaggedFramesDroppedOnAccess(t *testing.T) {
	setupHost(t, hSrv1, 10)
	setupHost(t, hSw3, 10)
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+access(hSw3.sw1Port, "v10"))
	// Tagged VLAN 10 into an access port of VLAN 10: dropped (reference 5.3.2).
	if reach(t, hSrv1, hSw3, 10) {
		t.Error("tagged frames were forwarded by an access port")
	}
	if !reach(t, hSrv1, hSw3, 1) {
		t.Error("untagged traffic broken")
	}
}

func TestTrunkWithNative(t *testing.T) {
	setupHost(t, hSrv1)
	setupHost(t, hSw3)
	setupHost(t, hSw2, 20, 30)
	cfg := vlans + access(hSrv1.sw1Port, "v10") + access(hSw3.sw1Port, "v20") +
		"set interfaces 1/ens19 unit 0 family ethernet-switching interface-mode trunk\n" +
		"set interfaces 1/ens19 unit 0 family ethernet-switching vlan members [ v20 v30 ]\n" +
		"set interfaces 1/ens19 native-vlan-id v10\n"
	configure(t, cfg)
	// Native VLAN 10: untagged between sw2 and srv1.
	if !reach(t, hSw2, hSrv1, 1) {
		t.Error("native VLAN not untagged on the trunk")
	}
	// VLAN 20 tagged on the trunk, untagged on sw3's access port: sw2's
	// ens19.20 (192.168.20.2) reaches sw3 once sw3 has 192.168.20.3 untagged.
	mustSSH(t, hSw3.vm, "ip -n h addr add 192.168.20.3/24 dev "+hSw3.nic)
	if !reach(t, hSw2, hSw3, 20) {
		t.Error("tagged VLAN 20 not carried between trunk and access port")
	}
	// VLAN 30 has no other port: nothing to reach, and sw3 must not see it.
	if reach(t, hSw2, hSw3, 30) {
		t.Error("VLAN 30 leaked")
	}
}

// TestHitlessCommits changes one port repeatedly while traffic flows
// between two other ports; not a single ping may be lost.
func TestHitlessCommits(t *testing.T) {
	for _, h := range hosts {
		setupHost(t, h)
	}
	base := vlans + access(hSrv1.sw1Port, "v10") + access(hSw3.sw1Port, "v10")
	configure(t, base+access(hSw2.sw1Port, "v20"))

	var wg sync.WaitGroup
	var pingOut string
	var pingErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		pingOut, pingErr = ssh(hSrv1.vm, "ip netns exec h ping -q -c 2400 -i 0.005 -W1 192.168.1.3")
	}()
	time.Sleep(time.Second)
	variants := []string{
		access(hSw2.sw1Port, "v10"),
		access(hSw2.sw1Port, "v30") + "set interfaces 1/ens19 description x\n",
		"set interfaces 1/ens19 unit 0 family ethernet-switching interface-mode trunk\nset interfaces 1/ens19 unit 0 family ethernet-switching vlan members [ v10 v20 ]\n",
		access(hSw2.sw1Port, "v20") + "set interfaces 1/ens19 mtu 9014\n",
		"set vlans v40 vlan-id 40\n" + access(hSw2.sw1Port, "v40"),
	}
	for _, v := range variants {
		configure(t, base+v)
	}
	wg.Wait()
	if pingErr != nil || !strings.Contains(pingOut, " 0% packet loss") {
		t.Fatalf("traffic between unchanged ports was disturbed: %v\n%s", pingErr, pingOut)
	}
}

func TestReleasedPortGoesDown(t *testing.T) {
	for _, h := range hosts {
		setupHost(t, h)
	}
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+access(hSw3.sw1Port, "v10")+access(hSw2.sw1Port, "v10"))
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+access(hSw3.sw1Port, "v10"))
	out := mustSSH(t, sw1, "ip -o link show ens19")
	if strings.Contains(out, ",UP") || strings.Contains(out, "master") {
		t.Errorf("released port still up or enslaved: %s", out)
	}
	if !reach(t, hSrv1, hSw3, 1) {
		t.Error("remaining ports disturbed")
	}
}

func TestRestartIsIdempotent(t *testing.T) {
	setupHost(t, hSrv1)
	setupHost(t, hSw3)
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+access(hSw3.sw1Port, "v10"))
	mustSSH(t, sw1, "systemctl restart switchd; sleep 2")
	out := mustSSH(t, sw1, "journalctl -u switchd --no-pager -o cat --since -4s")
	if !strings.Contains(out, "nothing to change") {
		t.Errorf("restart re-planned changes:\n%s", out)
	}
	if !reach(t, hSrv1, hSw3, 1) {
		t.Error("traffic broken after restart")
	}
}

func TestStaticLAG(t *testing.T) {
	setupHost(t, hSrv1)
	setupBondHost(t, 1500)
	lag := "set interfaces ae1 unit 0 family ethernet-switching vlan members v10\n" +
		"set interfaces 1/ens21 ether-options 802.3ad ae1\nset interfaces 1/ens22 ether-options 802.3ad ae1\n"
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+lag)
	out := mustSSH(t, sw1, "cat /proc/net/bonding/ae1; ip -o link show ens21; ip -o link show ens22")
	for _, want := range []string{"load balancing (xor)", "Slave Interface: ens21", "Slave Interface: ens22", "layer3+4"} {
		if !strings.Contains(out, want) {
			t.Errorf("bond lacks %q:\n%s", want, out)
		}
	}
	if !reach(t, hSrv1, bondHost, 1) || !reach(t, bondHost, hSrv1, 1) {
		t.Fatal("no traffic over the static bundle")
	}
	// Many flows (different source ports) must all work, whichever member
	// they hash to in each direction.
	res := mustSSH(t, hSrv1.vm, "for p in $(seq 1 20); do ip netns exec h ping -c1 -W1 192.168.1.22 >/dev/null && echo ok; done | wc -l")
	if strings.TrimSpace(res) != "20" {
		t.Errorf("only %s of 20 pings over the bundle", strings.TrimSpace(res))
	}
	// Removing the bundle releases both ports (down, no master) and deletes it.
	configure(t, vlans+access(hSrv1.sw1Port, "v10"))
	out = mustSSH(t, sw1, "ip -o link show ens21; ip -o link show ens22; ip link show ae1 2>&1 || true")
	if strings.Contains(out, "master") || strings.Contains(out, ",UP") || !strings.Contains(out, "does not exist") {
		t.Errorf("bundle not released:\n%s", out)
	}
}

func TestJumboMTU(t *testing.T) {
	setupHost(t, hSrv1)
	setupBondHost(t, 9000)
	mustSSH(t, hSrv1.vm, "ip -n h link set ens19 mtu 9000")
	defer mustSSH(t, hSrv1.vm, "ip -n h link set ens19 mtu 1500")
	lag := "set interfaces ae1 unit 0 family ethernet-switching vlan members v10\n" +
		"set interfaces 1/ens21 ether-options 802.3ad ae1\nset interfaces 1/ens22 ether-options 802.3ad ae1\n"
	// Default MTU 1514: jumbo frames are dropped, standard frames pass.
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+lag)
	if !reachSize(t, hSrv1, bondHost, 1, 1472) {
		t.Fatal("1500-byte packets do not pass")
	}
	if reachSize(t, hSrv1, bondHost, 1, 8972) {
		t.Error("9000-byte packets pass with mtu 1514")
	}
	// mtu 9014 on the ports and the bundle (members inherit it).
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+lag+
		"set interfaces 1/ens23 mtu 9014\nset interfaces ae1 mtu 9014\n")
	out := mustSSH(t, sw1, "ip -o link show ens21 | grep -o 'mtu [0-9]*'; ip -o link show ae1 | grep -o 'mtu [0-9]*'")
	if strings.Count(out, "mtu 9000") != 2 {
		t.Errorf("MTU not applied to bundle and member: %s", out)
	}
	if !reachSize(t, hSrv1, bondHost, 1, 8972) {
		t.Error("9000-byte packets do not pass with mtu 9014")
	}
}

// waitFor polls cmd on sw1 until its output contains want.
func waitFor(t *testing.T, cmd, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if out, _ := ssh(sw1, cmd); strings.Contains(out, want) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func TestForeignChangesReverted(t *testing.T) {
	setupHost(t, hSrv1)
	setupHost(t, hSw3)
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+access(hSw3.sw1Port, "v10"))
	start := time.Now()
	mustSSH(t, sw1, "ip link set ens23 down")
	if !waitFor(t, "ip -o link show ens23", ",UP", 5*time.Second) {
		t.Fatal("link taken down by someone else was not brought back up")
	}
	t.Logf("link state restored after %v", time.Since(start).Round(time.Millisecond))
	start = time.Now()
	mustSSH(t, sw1, "bridge vlan del dev ens23 vid 10")
	if !waitFor(t, "bridge vlan show dev ens23", "10 PVID", 40*time.Second) {
		t.Fatal("removed VLAN was not restored")
	}
	t.Logf("VLAN restored after %v", time.Since(start).Round(time.Millisecond))
	out := mustSSH(t, sw1, "journalctl -u switchd --no-pager -o cat --since -60s")
	if !strings.Contains(out, "correcting kernel state") {
		t.Error("correction not logged")
	}
	if !reach(t, hSrv1, hSw3, 1) {
		t.Error("traffic not restored")
	}
}

func TestHotplug(t *testing.T) {
	// Park ens20 in another namespace: switchd sees it as absent.
	mustSSH(t, sw1, "ip netns add parked 2>/dev/null; ip link set ens20 netns parked 2>/dev/null; true")
	defer ssh(sw1, "ip -n parked link set ens20 netns 1 2>/dev/null; true")
	configure(t, vlans+"set interfaces 1/ens20 unit 0 family ethernet-switching vlan members v30\n")
	if out, _ := ssh(sw1, "ip link show ens20"); !strings.Contains(out, "does not exist") {
		t.Fatalf("ens20 should be absent: %s", out)
	}
	start := time.Now()
	mustSSH(t, sw1, "ip -n parked link set ens20 netns 1")
	if !waitFor(t, "bridge vlan show dev ens20", "30 PVID", 10*time.Second) {
		t.Fatal("appearing port was not configured")
	}
	t.Logf("port configured %v after it appeared", time.Since(start).Round(time.Millisecond))
	if !waitFor(t, "ip -o link show ens20", ",UP", 5*time.Second) {
		t.Error("appearing port not brought up")
	}
}

func TestMACLimit(t *testing.T) {
	setupHost(t, hSrv1)
	setupHost(t, hSw3)
	base := vlans + access(hSrv1.sw1Port, "v10") + access(hSw3.sw1Port, "v10")
	configure(t, base+"set interfaces 1/ens23 mac-limit 3\n")
	mustSSH(t, sw1, "bridge fdb flush dev ens23 dynamic 2>/dev/null; true")
	// Six source MACs behind srv1's port.
	var cmds []string
	for i := 1; i <= 6; i++ {
		cmds = append(cmds, fmt.Sprintf("ip -n h link add m%[1]d link ens19 type macvlan mode bridge; ip -n h addr add 192.168.1.%[2]d/24 dev m%[1]d; ip -n h link set m%[1]d up", i, 100+i))
	}
	mustSSH(t, hSrv1.vm, strings.Join(cmds, "; "))
	defer ssh(hSrv1.vm, "for i in 1 2 3 4 5 6; do ip -n h link del m$i; done")
	ok := 0
	for i := 1; i <= 6; i++ {
		if _, err := ssh(hSrv1.vm, fmt.Sprintf("ip netns exec h ping -c2 -W1 -I m%d 192.168.1.3", i)); err == nil {
			ok++
		}
	}
	if ok != 6 {
		t.Errorf("only %d of 6 sources reached sw3: the limit must never drop traffic", ok)
	}
	out := mustSSH(t, sw1, "bridge -d link show dev ens23; bridge fdb show dev ens23 | grep -v permanent | grep -c vlan")
	if !strings.Contains(out, "learning off") {
		t.Errorf("learning not switched off at the limit:\n%s", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if n, _ := strconv.Atoi(lines[len(lines)-1]); n > 5 {
		t.Errorf("%d addresses learned with mac-limit 3", n)
	}
	configure(t, base)
	if !waitFor(t, "bridge -d link show dev ens23", "learning on", 10*time.Second) {
		t.Error("learning not re-enabled after the limit was removed")
	}
}

// blast sends n frames to dst from srv1's test host (as fast as possible)
// and returns how many sw3's host received.
func blast(t *testing.T, dst string, n int) int {
	t.Helper()
	capture := make(chan string, 1)
	go func() {
		out, _ := ssh(hSw3.vm, "timeout 4 ip netns exec h tcpdump -lnni ens23 ether proto 0x88b6 2>/dev/null | wc -l")
		capture <- out
	}()
	time.Sleep(1500 * time.Millisecond)
	py := fmt.Sprintf(`import socket
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW)
s.bind(("ens19", 0))
dst = bytes.fromhex("%s")
f = dst + s.getsockname()[4] + b"\x88\xb6" + bytes(46)
for i in range(%d):
    s.send(f)
`, strings.ReplaceAll(dst, ":", ""), n)
	mustSSH(t, hSrv1.vm, "ip netns exec h python3 -c '"+py+"'")
	got, _ := strconv.Atoi(strings.TrimSpace(<-capture))
	return got
}

func TestStormControl(t *testing.T) {
	setupHost(t, hSrv1)
	setupHost(t, hSw3)
	base := vlans + access(hSrv1.sw1Port, "v10") + access(hSw3.sw1Port, "v10")
	configure(t, base)
	if got := blast(t, "ff:ff:ff:ff:ff:ff", 3000); got < 2900 {
		t.Fatalf("without storm control only %d of 3000 broadcasts arrived (test setup problem)", got)
	}
	configure(t, base+"set interfaces 1/ens23 storm-control broadcast 100\nset interfaces 1/ens23 storm-control multicast 100\n")
	if got := blast(t, "ff:ff:ff:ff:ff:ff", 3000); got == 0 || got > 400 {
		t.Errorf("broadcast limited to 100 pps: %d of 3000 arrived", got)
	} else {
		t.Logf("broadcast: %d of 3000 arrived", got)
	}
	if got := blast(t, "01:00:5e:00:00:01", 3000); got == 0 || got > 400 {
		t.Errorf("multicast limited to 100 pps: %d of 3000 arrived", got)
	}
	// IEEE link-local (here the bridge group address, forwarded while STP is
	// off) is never rate-limited.
	if got := blast(t, "01:80:c2:00:00:00", 500); got < 490 {
		t.Errorf("link-local frames were limited: %d of 500 arrived", got)
	}
	// Unicast is unaffected.
	if !reach(t, hSrv1, hSw3, 1) {
		t.Error("unicast broken by storm control")
	}
	configure(t, base)
	if got := blast(t, "ff:ff:ff:ff:ff:ff", 3000); got < 2900 {
		t.Errorf("after removing storm control only %d of 3000 broadcasts arrived", got)
	}
	if out := mustSSH(t, sw1, "tc filter show dev ens23 ingress"); strings.Contains(out, "police") {
		t.Errorf("policer left behind:\n%s", out)
	}
}

func TestManagementPlane(t *testing.T) {
	for _, h := range hosts {
		setupHost(t, h)
	}
	mustSSH(t, hSrv1.vm, "ip -n h addr add 192.168.99.2/24 dev ens19")
	mustSSH(t, hSw3.vm, "ip -n h addr add 192.168.99.3/24 dev ens23")
	mustSSH(t, hSw2.vm, "ip -n h addr add 192.168.98.2/24 dev ens19")
	base := vlans + "set vlans mgmt vlan-id 99\n" + access(hSrv1.sw1Port, "mgmt") + access(hSw3.sw1Port, "v10")

	// IRB-like: management address on VLAN 99.
	configure(t, base+"set stack member 1 management vlan mgmt\nset stack member 1 management address 192.168.99.1/24\n")
	if _, err := ssh(hSrv1.vm, "ip netns exec h ping -c2 -W1 192.168.99.1"); err != nil {
		t.Error("management address not reachable from its VLAN")
	}
	if _, err := ssh(hSrv1.vm, "ip netns exec h timeout 3 bash -c '</dev/tcp/192.168.99.1/22'"); err != nil {
		t.Error("sshd (default VRF) not reachable through the management VRF")
	}
	if _, err := ssh(hSw3.vm, "ip netns exec h ping -c2 -W1 192.168.99.1"); err == nil {
		t.Error("management address reachable from another VLAN")
	}
	out := mustSSH(t, sw1, "ip -d link show mgmt0; bridge vlan show dev swbr0; ip vrf show")
	for _, want := range []string{"vlan protocol 802.1Q id 99", "master mgmt", "mgmt 100"} {
		if !strings.Contains(strings.Join(strings.Fields(out), " "), want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}

	// Dedicated port instead.
	configure(t, base+"set stack member 1 management interface ens19\nset stack member 1 management address 192.168.98.1/24\n")
	if _, err := ssh(hSw2.vm, "ip netns exec h ping -c2 -W1 192.168.98.1"); err != nil {
		t.Error("management address on the dedicated port not reachable")
	}
	out = mustSSH(t, sw1, "ip link show mgmt0 2>&1; bridge vlan show dev swbr0 | grep -c 99; true")
	if !strings.Contains(out, "does not exist") || !strings.HasSuffix(strings.TrimSpace(out), "0") {
		t.Errorf("IRB not removed after switching to a dedicated port:\n%s", out)
	}

	// No management block: switchd removes what it created.
	configure(t, base)
	out = mustSSH(t, sw1, "ip vrf show; ip -o link show ens19")
	if strings.Contains(out, "mgmt") || strings.Contains(out, ",UP") {
		t.Errorf("management not torn down:\n%s", out)
	}
}
