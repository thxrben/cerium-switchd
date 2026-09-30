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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const sw1 = "10.5.176.95"

// sw1Names maps sw1's Linux interface names to switch names ("ens19" ->
// "1/1/0"), from 'show chassis hardware' the first time it is needed (all
// NICs are present then).
var (
	sw1NamesOnce sync.Once
	sw1Names     map[string]string
	oldName      = regexp.MustCompile(`\b1/(ens[0-9]+)\b`)
)

// portNames rewrites "1/<linux-name>" in s to sw1's switch names, so the
// tests can refer to the cabling by Linux name.
func portNames(t *testing.T, s string) string {
	t.Helper()
	sw1NamesOnce.Do(func() {
		sw1Names = map[string]string{}
		out := mustSSH(t, sw1, "swcli -c 'show chassis hardware'")
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) >= 2 && strings.Count(f[0], "/") == 2 {
				sw1Names[f[1]] = f[0]
			}
		}
	})
	return oldName.ReplaceAllStringFunc(s, func(m string) string {
		if n, ok := sw1Names[m[2:]]; ok {
			return n
		}
		t.Fatalf("sw1 has no port %s", m[2:])
		return m
	})
}

// A test host: the VM and its NIC towards sw1, and the sw1 port it is on
// (by Linux name; see portNames).
type host struct {
	name, vm, nic, sw1Port string
	n                      int // host number in 192.168.x.n
}

var (
	hSrv1 = host{"srv1", "10.5.176.101", "ens19", "1/ens23", 1}
	// sw2's host uses the underlay link (sw2 ens1 <-> sw1 ens1): the
	// stacking links (stk-*) stay free for the stacking tests.
	hSw2  = host{"sw2", "10.5.176.96", "ens1", "1/ens1", 2}
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

// testUsers are the login users the tests create; all other configured
// users (e.g. a person's own login on the lab switch) are kept.
var testUsers = []string{"alice", "bob"}

// keptUsers returns the set lines of the configured non-test users.
func keptUsers(t *testing.T) string {
	t.Helper()
	out := mustSSH(t, sw1, "swcli -c 'show configuration system login | display set'")
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "set system login user ") && !strings.HasPrefix(l, "deactivate system login user ") {
			continue
		}
		fs := strings.Fields(l)
		if len(fs) > 4 && !slices.Contains(testUsers, fs[4]) {
			keep = append(keep, l)
		}
	}
	if len(keep) == 0 {
		return ""
	}
	return strings.Join(keep, "\n") + "\n"
}

// keptSSH holds the CLI SSH server configuration found before the first
// test ran (e.g. the port people use to reach the lab switch); it is kept
// unless a test configures the SSH server itself.
var (
	keptSSHOnce sync.Once
	keptSSH     string
)

func keptServices(t *testing.T) string {
	t.Helper()
	keptSSHOnce.Do(func() {
		out := mustSSH(t, sw1, "swcli -c 'show configuration system services ssh | display set'")
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "set system services ssh") {
				keptSSH += l + "\n"
			}
		}
	})
	return keptSSH
}

// masterSw1 makes member 1 (sw1) the stack master, so that commits are
// made and logged there (tests that restart switchd move mastership).
func masterSw1(t *testing.T) {
	t.Helper()
	out := mustSSH(t, sw1, "swcli -c 'show virtual-chassis'")
	if regexp.MustCompile(`(?m)^1 +\S+ +master `).MatchString(out) {
		return
	}
	mustSSH(t, sw1, "swcli -c 'request chassis routing-engine master switch member 1'")
	for i := 0; i < 50; i++ {
		if regexp.MustCompile(`(?m)^1 +\S+ +master `).MatchString(mustSSH(t, sw1, "swcli -c 'show virtual-chassis'")) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("sw1 did not become master")
}

// stackBase keeps the per-member settings of the virtual chassis: sw2 and
// sw3 are members of sw1's stack, so a "load override" without them would
// rename them.
const stackBase = "set virtual-chassis member 1 host-name sw1\n" +
	"set virtual-chassis member 2 host-name sw2\n" +
	"set virtual-chassis member 3 host-name sw3\n"

// configure replaces sw1's configuration with setLines and commits it
// (commit + confirm). Configured login users other than the test users,
// and the CLI SSH server, are kept.
func configure(t *testing.T, setLines string) {
	t.Helper()
	masterSw1(t)
	base := "set system host-name sw1\n" + stackBase + keptUsers(t)
	if !strings.Contains(setLines, "system services ssh") {
		base += keptServices(t)
	}
	mustSSH(t, sw1, "cat > /root/lab.set <<'EOF'\n"+base+portNames(t, setLines)+"\nEOF")
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
		"set interfaces 1/ens1 unit 0 family ethernet-switching interface-mode trunk\n" +
		"set interfaces 1/ens1 unit 0 family ethernet-switching vlan members [ v20 v30 ]\n" +
		"set interfaces 1/ens1 native-vlan-id v10\n"
	configure(t, cfg)
	// Native VLAN 10: untagged between sw2 and srv1.
	if !reach(t, hSw2, hSrv1, 1) {
		t.Error("native VLAN not untagged on the trunk")
	}
	// VLAN 20 tagged on the trunk, untagged on sw3's access port: sw2's
	// ens1.20 (192.168.20.2) reaches sw3 once sw3 has 192.168.20.3 untagged.
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
		access(hSw2.sw1Port, "v30") + "set interfaces 1/ens1 description x\n",
		"set interfaces 1/ens1 unit 0 family ethernet-switching interface-mode trunk\nset interfaces 1/ens1 unit 0 family ethernet-switching vlan members [ v10 v20 ]\n",
		access(hSw2.sw1Port, "v20") + "set interfaces 1/ens1 mtu 9014\n",
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
	out := mustSSH(t, sw1, "ip -o link show ens1")
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
	// Park ens21 in another namespace: switchd sees it as absent.
	mustSSH(t, sw1, "ip netns add parked 2>/dev/null; ip link set ens21 netns parked 2>/dev/null; true")
	defer ssh(sw1, "ip -n parked link set ens21 netns 1 2>/dev/null; true")
	configure(t, vlans+"set interfaces 1/ens21 unit 0 family ethernet-switching vlan members v30\n")
	if out, _ := ssh(sw1, "ip link show ens21"); !strings.Contains(out, "does not exist") {
		t.Fatalf("ens21 should be absent: %s", out)
	}
	start := time.Now()
	mustSSH(t, sw1, "ip -n parked link set ens21 netns 1")
	if !waitFor(t, "bridge vlan show dev ens21", "30 PVID", 10*time.Second) {
		t.Fatal("appearing port was not configured")
	}
	t.Logf("port configured %v after it appeared", time.Since(start).Round(time.Millisecond))
	if !waitFor(t, "ip -o link show ens21", ",UP", 5*time.Second) {
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
	mustSSH(t, hSw3.vm, "ip -n h addr add 192.168.99.3/24 dev ens23; ip -n h addr add 192.168.10.3/24 dev ens23")
	mustSSH(t, hSw2.vm, "ip -n h addr add 192.168.98.2/24 dev "+hSw2.nic)
	base := vlans + "set vlans mgmt vlan-id 99\n" + access(hSrv1.sw1Port, "mgmt") + access(hSw3.sw1Port, "v10")
	mgmtVLAN := "set system management-instance\nset vlans mgmt l3-interface irb.99\n" +
		"set interfaces irb unit 99 family inet address 192.168.99.1/24 member 1\n" +
		"set routing-instances mgmt_junos interface irb.99\n"
	// A data irb in the default instance, to check the protection.
	dataIRB := "set vlans v10 l3-interface irb.10\nset interfaces irb unit 10 family inet address 192.168.10.1/24\n"

	// Management on VLAN 99 (irb in mgmt_junos).
	configure(t, base+mgmtVLAN+dataIRB)
	if _, err := ssh(hSrv1.vm, "ip netns exec h ping -c2 -W1 192.168.99.1"); err != nil {
		t.Error("management address not reachable from its VLAN")
	}
	if _, err := ssh(hSrv1.vm, "ip netns exec h timeout 3 bash -c '</dev/tcp/192.168.99.1/22'"); err != nil {
		t.Error("sshd not reachable through the management instance")
	}
	if _, err := ssh(hSw3.vm, "ip netns exec h ping -c2 -W1 192.168.99.1"); err == nil {
		t.Error("management address reachable from another VLAN")
	}
	// The data irb answers ping, but the switch's services are not
	// reachable through it.
	if _, err := ssh(hSw3.vm, "ip netns exec h ping -c2 -W1 192.168.10.1"); err != nil {
		t.Error("data irb does not answer ping")
	}
	if _, err := ssh(hSw3.vm, "ip netns exec h timeout 3 bash -c '</dev/tcp/192.168.10.1/22'"); err == nil {
		t.Error("SSH reachable through a data irb (protection filter missing)")
	}
	out := mustSSH(t, sw1, "ip -d link show irb.99; ip vrf show; nft list table inet switchd_protect")
	for _, want := range []string{"vlan protocol 802.1Q id 99", "master mgmt_junos", "mgmt_junos 100", `"irb.10"`} {
		if !strings.Contains(strings.Join(strings.Fields(out), " "), want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"irb.99"`) {
		t.Errorf("management irb must not be filtered:\n%s", out)
	}
	if out := mustSSH(t, sw1, "swcli -c 'show interfaces terse'"); !strings.Contains(out, "management") {
		t.Errorf("management role not shown:\n%s", out)
	}

	// Dedicated port instead.
	configure(t, base+"set system management-instance\nset interfaces 1/ens1 unit 0 family inet address 192.168.98.1/24\n"+
		"set routing-instances mgmt_junos interface 1/ens1.0\n")
	if _, err := ssh(hSw2.vm, "ip netns exec h ping -c2 -W1 192.168.98.1"); err != nil {
		t.Error("management address on the dedicated port not reachable")
	}
	out = mustSSH(t, sw1, "ip link show irb.99 2>&1; ip -o link show ens1; bridge vlan show dev swbr0 | grep -c 99; nft list table inet switchd_protect 2>&1; true")
	if !strings.Contains(out, "does not exist") || !strings.Contains(out, "master mgmt_junos") || !strings.Contains(out, "No such file") {
		t.Errorf("after switching to a dedicated port (irb.99 gone, ens1 in mgmt_junos, no protection table):\n%s", out)
	}

	// No management instance: switchd removes what it created.
	configure(t, base)
	out = mustSSH(t, sw1, "ip vrf show; ip -o link show ens1")
	if strings.Contains(out, "mgmt") || strings.Contains(out, ",UP") {
		t.Errorf("management not torn down:\n%s", out)
	}
}

// receiveSyslog listens on srv1's test host (192.168.99.2) and returns what
// arrived within the timeout.
func receiveSyslog(t *testing.T, transport string, ready chan<- struct{}) string {
	py := `import socket, sys
t = sys.argv[1]
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM if t == "udp" else socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("192.168.99.2", 5514))
s.settimeout(12)
out = b""
try:
    if t == "udp":
        while len(out) < 8000:
            out += s.recv(4096) + b"\n"
    else:
        s.listen(1)
        c, _ = s.accept()
        c.settimeout(3)
        while True:
            d = c.recv(4096)
            if not d:
                break
            out += d
except Exception:
    pass
print(out.decode(errors="replace"))
`
	mustSSH(t, hSrv1.vm, "cat > /tmp/rx.py <<'EOF'\n"+py+"\nEOF")
	go func() { time.Sleep(time.Second); close(ready) }()
	out, _ := ssh(hSrv1.vm, "ip netns exec h python3 /tmp/rx.py "+transport)
	return out
}

func TestSyslogOverManagementVRF(t *testing.T) {
	setupHost(t, hSrv1)
	mustSSH(t, hSrv1.vm, "ip -n h addr add 192.168.99.2/24 dev ens19")
	base := vlans + "set vlans mgmt vlan-id 99\n" + access(hSrv1.sw1Port, "mgmt") +
		"set system management-instance\nset vlans mgmt l3-interface irb.99\n" +
		"set interfaces irb unit 99 family inet address 192.168.99.1/24 member 1\nset routing-instances mgmt_junos interface irb.99\n"
	configure(t, base)
	for _, transport := range []string{"udp", "tcp"} {
		ready := make(chan struct{})
		got := make(chan string, 1)
		go func() { got <- receiveSyslog(t, transport, ready) }()
		<-ready
		configure(t, base+"set system syslog host 192.168.99.2 port 5514\nset system syslog host 192.168.99.2 transport "+transport+"\n")
		mustSSH(t, sw1, "swcli -c 'show version'") // an interactive-commands message
		out := <-got
		if !strings.Contains(out, " sw1 switchd ") || !strings.Contains(out, "commit") {
			t.Errorf("%s: no commit message received:\n%s", transport, out)
		}
		if !strings.Contains(out, "<18") { // local6 (change-log) = 22*8+sev
			t.Errorf("%s: change-log facility not local6:\n%s", transport, out)
		}
		if transport == "tcp" && !strings.Contains(out, "command=\"show version\"") {
			t.Errorf("tcp: CLI command not logged:\n%s", out)
		}
	}
	configure(t, base)
	if out := mustSSH(t, sw1, "swcli -c 'show system syslog'"); !strings.Contains(out, "No remote syslog servers") {
		t.Errorf("forwarder not removed: %s", out)
	}
}

// sshAs runs a command as a configured user with the dev machine's key.
func sshAs(user, cmd string) (string, error) {
	out, err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=accept-new",
		user+"@"+sw1, cmd).CombinedOutput()
	return string(out), err
}

func TestUserAccounts(t *testing.T) {
	key, err := os.ReadFile(os.Getenv("HOME") + "/.ssh/id_ed25519.pub")
	if err != nil {
		t.Skip("no ed25519 key")
	}
	k := strings.TrimSpace(string(key))
	users := fmt.Sprintf(`set system login user alice class super-user
set system login user alice full-name "Alice Admin"
set system login user alice authentication ssh-key "%s"
set system login user alice authentication encrypted-password "$6$labsaltlabsalt12$cJd.1Qyjf8fd1M.E0tTVRIZLML0MX2329RNEgjFj1byV0DKEh6n6H2vdyCDCE1CPvLLeSBk4wh.VSbu8glnNz/"
set system login user bob class read-only
set system login user bob authentication ssh-key "%s"
`, k, k)
	configure(t, users)
	out := mustSSH(t, sw1, "getent passwd alice bob; stat -c '%U %a %n' /home/alice/.ssh /home/alice/.ssh/authorized_keys")
	for _, want := range []string{"alice:x:20", "Alice Admin", ":/usr/local/bin/swcli", "bob:x:20", "root 755 /home/alice/.ssh", "root 644 /home/alice/.ssh/authorized_keys"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// Key login lands in the CLI with the configured class.
	if out, err := sshAs("alice", "show version"); err != nil || !strings.Contains(out, "mclag switchd") {
		t.Errorf("alice: %v\n%s", err, out)
	}
	if out, _ := sshAs("bob", "configure"); !strings.Contains(out, "permission denied") {
		t.Errorf("read-only bob could configure:\n%s", out)
	}
	if out, _ := sshAs("alice", "bash -c id"); !strings.Contains(out, "syntax error") {
		t.Errorf("a remote command ran outside the CLI:\n%s", out)
	}
	// Password login with switchd's SHA-512 crypt hash (password "labpassword").
	dir := t.TempDir()
	os.WriteFile(dir+"/askpass", []byte("#!/bin/sh\necho labpassword\n"), 0o700)
	cmd := exec.Command("ssh", "-o", "PubkeyAuthentication=no", "-o", "PreferredAuthentications=password",
		"-o", "NumberOfPasswordPrompts=1", "-o", "ConnectTimeout=5", "alice@"+sw1, "show version")
	cmd.Env = append(os.Environ(), "SSH_ASKPASS="+dir+"/askpass", "SSH_ASKPASS_REQUIRE=force", "DISPLAY=x")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "mclag switchd") {
		t.Errorf("password login: %v\n%s", err, out)
	}
	// An existing OS account is not taken over.
	out = mustSSH(t, sw1, `swcli -c "configure
set system login user user class super-user
commit check
exit configuration-mode"; true`)
	if !strings.Contains(out, "OS account named user exists") {
		t.Errorf("no conflict for the existing account:\n%s", out)
	}
	// Removing the users deletes the accounts and keeps the homes.
	configure(t, "")
	out = mustSSH(t, sw1, "getent passwd alice bob; ls -d /home/alice; true")
	if strings.Contains(out, "alice:x") || strings.Contains(out, "bob:x") || !strings.Contains(out, "/home/alice") {
		t.Errorf("accounts not removed or home deleted:\n%s", out)
	}
	if out := mustSSH(t, sw1, "getent passwd user"); !strings.Contains(out, "user:x:1000") {
		t.Error("unmanaged account touched")
	}
}

func TestSerialConsoles(t *testing.T) {
	configure(t, "")
	unit := "systemctl is-active serial-getty@ttyS0; systemctl show -p ExecStart serial-getty@ttyS0"
	if !waitFor(t, unit, "--autologin root --noreset --noclear 115200 ttyS0", 10*time.Second) {
		t.Fatalf("no auto-detected console on ttyS0:\n%s", mustSSH(t, sw1, unit+"; true"))
	}
	// Both the serial console and the display run the CLI as root, via the
	// profile hook, under the supervisor.
	mustSSH(t, sw1, "systemctl restart serial-getty@ttyS0 getty@tty1")
	cli := "for t in ttyS0 tty1; do ps -o user=,args= -t $t | grep -c 'root *swcli-session'; done | tr '\\n' ' '"
	if !waitFor(t, cli, "1 1 ", 10*time.Second) {
		t.Errorf("consoles do not run the CLI:\n%s", mustSSH(t, sw1, "ps -o tty=,user=,args= -t ttyS0,tty1; true"))
	}
	configure(t, "set system ports login-required\n")
	if !waitFor(t, unit+"; test -e /etc/systemd/system/getty@.service.d/switchd.conf || echo vt-default", "vt-default", 10*time.Second) ||
		strings.Contains(mustSSH(t, sw1, unit), "autologin") {
		t.Errorf("login-required not applied:\n%s", mustSSH(t, sw1, unit+"; true"))
	}
	configure(t, "")
	configure(t, "set system ports console ttyS0 speed 9600\n")
	if !waitFor(t, unit, "--noclear 9600 ttyS0", 10*time.Second) {
		t.Errorf("speed not applied:\n%s", mustSSH(t, sw1, unit+"; true"))
	}
	configure(t, "set system ports console ttyS0 disable\n")
	if out := mustSSH(t, sw1, "systemctl is-active serial-getty@ttyS0; systemctl is-enabled serial-getty@ttyS0; true"); !strings.Contains(out, "inactive") || !strings.Contains(out, "masked") {
		t.Errorf("console not disabled: %s", out)
	}
	configure(t, "")
	if !waitFor(t, unit, "--autologin root --noreset --noclear 115200 ttyS0", 10*time.Second) {
		t.Error("console not restored after removing the configuration")
	}
	mustSSH(t, sw1, "systemctl restart getty@tty1")
}

func TestCLISSHServer(t *testing.T) {
	key, err := os.ReadFile(os.Getenv("HOME") + "/.ssh/id_ed25519.pub")
	if err != nil {
		t.Skip("no ed25519 key")
	}
	alice := fmt.Sprintf("set system login user alice class super-user\nset system login user alice authentication ssh-key \"%s\"\n", strings.TrimSpace(string(key)))
	osSSHD := mustSSH(t, sw1, "md5sum /etc/ssh/sshd_config; ls /etc/ssh/sshd_config.d/")

	// Port 22 belongs to the OS SSH server: a commit error, nothing changes.
	mustSSH(t, sw1, "printf 'set system host-name sw1\\nset system services ssh\\n' > /root/lab.set")
	out := mustSSH(t, sw1, "swcli -c 'configure\nload override lab.set\ncommit check\nexit configuration-mode'; true")
	if !strings.Contains(out, "port 22 is already used") {
		t.Errorf("port conflict not reported:\n%s", out)
	}

	configure(t, alice+"set system services ssh port 2222\nset system login message \"lab switch\"\n")
	on2222 := func(user, cmd string) (string, error) {
		// The lab firewall only admits the CLI port from inside the lab.
		out, err := exec.Command("ssh", "-J", "root@"+hSw2.vm, "-p", "2222", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
			"-o", "StrictHostKeyChecking=accept-new", user+"@"+sw1, cmd).CombinedOutput()
		return string(out), err
	}
	if !waitFor(t, "systemctl is-active switchd-sshd", "active", 10*time.Second) {
		t.Fatal(mustSSH(t, sw1, "systemctl status switchd-sshd --no-pager; true"))
	}
	if out, err := on2222("alice", "show version"); err != nil || !strings.Contains(out, "mclag switchd") || !strings.Contains(out, "lab switch") {
		t.Errorf("alice on the CLI port: %v\n%s", err, out)
	}
	if out, err := on2222("root", "show version"); err == nil {
		t.Errorf("root admitted with root-login deny:\n%s", out)
	}
	configure(t, alice+"set system services ssh port 2222\nset system services ssh root-login key-only\n")
	if out, err := on2222("root", "show version"); err != nil || !strings.Contains(out, "mclag switchd") {
		t.Errorf("root (key-only) did not land in the CLI: %v\n%s", err, out)
	}
	// The OS SSH server was never modified.
	if now := mustSSH(t, sw1, "md5sum /etc/ssh/sshd_config; ls /etc/ssh/sshd_config.d/"); now != osSSHD {
		t.Errorf("OS SSH configuration changed:\n%s\n%s", osSSHD, now)
	}
	if keptServices(t) != "" {
		// Removal cannot be tested without cutting off the kept access;
		// the kept configuration comes back instead.
		configure(t, "")
		if !waitFor(t, "systemctl is-active switchd-sshd", "active", 10*time.Second) {
			t.Error("kept CLI SSH server not restored")
		}
		return
	}
	configure(t, "")
	if !waitFor(t, "systemctl is-active switchd-sshd; test -e /etc/switchd/sshd_config || echo gone", "gone", 10*time.Second) {
		t.Error("CLI SSH server not removed")
	}
}

// L3: routing between VLANs through irb interfaces, a routed port and
// static routes (reference 5.3.2, 5.3.3, 5.8), and clean removal.
func TestRouting(t *testing.T) {
	for _, h := range hosts {
		setupHost(t, h)
	}
	// Host addresses and default routes inside the test namespaces.
	route := func(h host, addr, gw string) {
		mustSSH(t, h.vm, fmt.Sprintf("ip -n h addr add %s dev %s; ip -n h route replace default via %s", addr, h.nic, gw))
	}
	route(hSrv1, "10.10.10.2/24", "10.10.10.1")
	route(hSw3, "10.10.20.3/24", "10.10.20.1")
	route(hSw2, "10.10.30.2/24", "10.10.30.1")
	mustSSH(t, hSw2.vm, "ip -n h addr add 10.99.0.1/32 dev lo")
	defer func() {
		for _, h := range hosts {
			ssh(h.vm, "ip -n h route del default; ip -n h addr del 10.99.0.1/32 dev lo")
		}
	}()
	ping := func(from host, to string) bool {
		_, err := ssh(from.vm, "ip netns exec h ping -c2 -i0.2 -W1 "+to)
		return err == nil
	}
	cfg := vlans + access(hSrv1.sw1Port, "v10") + access(hSw3.sw1Port, "v20") +
		"set vlans v10 l3-interface irb.10\nset vlans v20 l3-interface irb.20\n" +
		"set interfaces irb unit 10 family inet address 10.10.10.1/24\n" +
		"set interfaces irb unit 20 family inet address 10.10.20.1/24\n" +
		"set interfaces 1/ens1 unit 0 family inet address 10.10.30.1/24\n" +
		"set routing-options static route 10.99.0.0/24 next-hop 10.10.30.2\n" +
		"set routing-options static route 198.51.100.0/24 discard\n"
	configure(t, cfg)
	if !ping(hSrv1, "10.10.10.1") {
		t.Error("irb.10 gateway not reachable from v10")
	}
	if !ping(hSrv1, "10.10.20.3") {
		t.Error("no routing between v10 and v20")
	}
	if !ping(hSrv1, "10.10.30.2") {
		t.Error("no routing from v10 to the routed port")
	}
	if !ping(hSrv1, "10.99.0.1") {
		t.Error("static route not used")
	}
	out := mustSSH(t, sw1, "ip route show proto 250; sysctl -n net.ipv4.conf.irb/10.forwarding net.ipv4.ip_forward")
	for _, want := range []string{"10.99.0.0/24 via 10.10.30.2", "blackhole 198.51.100.0/24", "metric 20"} {
		if !strings.Contains(out, want) {
			t.Errorf("routes: missing %q:\n%s", want, out)
		}
	}
	// Forwarding only on switchd's interfaces: the OS management NIC stays a host.
	if f := strings.Fields(out); len(f) < 2 || f[len(f)-2] != "1" || f[len(f)-1] != "0" {
		t.Errorf("forwarding sysctls (irb.10, global): %q", f)
	}
	// The hosts in one VLAN still switch directly (not via the router).
	if !reach(t, hSrv1, hSrv1, 1) {
		t.Error("sanity: srv1 cannot reach itself")
	}

	// IPv6: forwarding is system-wide there; the OS management NIC keeps
	// accepting router advertisements (accept_ra 2), and it is undone.
	// A dummy interface stands in for an OS NIC that uses SLAAC.
	mustSSH(t, sw1, "ip link add labra0 type dummy 2>/dev/null; sysctl -qw net.ipv6.conf.labra0.accept_ra=1")
	defer ssh(sw1, "ip link del labra0")
	raBefore := strings.TrimSpace(mustSSH(t, sw1, "sysctl -n net.ipv6.conf.ens18.accept_ra"))
	mustSSH(t, hSrv1.vm, "ip -n h addr add fd00:10::2/64 dev "+hSrv1.nic+" nodad; ip -n h -6 route replace default via fd00:10::1")
	mustSSH(t, hSw3.vm, "ip -n h addr add fd00:20::3/64 dev "+hSw3.nic+" nodad; ip -n h -6 route replace default via fd00:20::1")
	configure(t, cfg+"set interfaces irb unit 10 family inet6 address fd00:10::1/64\nset interfaces irb unit 20 family inet6 address fd00:20::1/64\n")
	time.Sleep(2 * time.Second) // DAD on the irb addresses
	if !ping(hSrv1, "fd00:20::3") {
		t.Error("no IPv6 routing between v10 and v20")
	}
	out = mustSSH(t, sw1, "sysctl -n net.ipv6.conf.all.forwarding net.ipv6.conf.ens18.accept_ra net.ipv6.conf.labra0.accept_ra")
	if f := strings.Fields(out); len(f) != 3 || f[0] != "1" || (raBefore == "1" && f[1] != "2") || f[2] != "2" {
		t.Errorf("IPv6 forwarding / accept_ra (all, ens18, labra0): %q (ens18 before: %s)", f, raBefore)
	}
	configure(t, cfg)
	out = mustSSH(t, sw1, "sysctl -n net.ipv6.conf.all.forwarding net.ipv6.conf.ens18.accept_ra net.ipv6.conf.labra0.accept_ra")
	if f := strings.Fields(out); len(f) != 3 || f[0] != "0" || f[1] != raBefore || f[2] != "1" {
		t.Errorf("IPv6 forwarding / accept_ra not restored (all, ens18, labra0): %q (ens18 before: %s)", f, raBefore)
	}

	// A data routing instance: irb.20 and the routed port route among
	// themselves, but not to the default instance (irb.10).
	configure(t, cfg+"set routing-instances blue interface irb.20\nset routing-instances blue interface 1/ens1.0\n"+
		"set routing-instances blue routing-options static route 10.99.0.0/24 next-hop 10.10.30.2\n")
	if !ping(hSw3, "10.10.30.2") || !ping(hSw3, "10.99.0.1") {
		t.Error("no routing inside instance blue")
	}
	if ping(hSrv1, "10.10.20.3") || ping(hSrv1, "10.10.30.2") {
		t.Error("routed from the default instance into instance blue")
	}
	if out := mustSSH(t, sw1, "swcli -c 'show route instance blue'"); !strings.Contains(out, "10.99.0.0/24") || !strings.Contains(out, "static") {
		t.Errorf("show route instance blue:\n%s", out)
	}
	configure(t, cfg)
	if !ping(hSrv1, "10.10.20.3") {
		t.Error("routing between v10 and v20 not restored after removing the instance")
	}

	// Changing an address is hitless for the other interfaces.
	configure(t, strings.Replace(cfg, "10.10.20.1/24", "10.10.20.254/24", 1))
	mustSSH(t, hSw3.vm, "ip -n h route replace default via 10.10.20.254")
	if !ping(hSrv1, "10.10.20.3") {
		t.Error("routing broken after changing irb.20's address")
	}
	if out := mustSSH(t, sw1, "ip -br addr show irb.20"); strings.Contains(out, "10.10.20.1/") {
		t.Errorf("old address left: %s", out)
	}

	// Removing L3 removes devices, addresses on the port and routes.
	configure(t, vlans+access(hSrv1.sw1Port, "v10"))
	out = mustSSH(t, sw1, "ip -br link show type vlan | grep -c irb || true; ip route show proto 250 | wc -l; ip -br addr show ens1 2>/dev/null | grep -c 10.10.30 || true")
	if f := strings.Fields(out); len(f) != 3 || f[0] != "0" || f[1] != "0" || f[2] != "0" {
		t.Errorf("L3 leftovers (irb devices, routes, port addresses): %q", f)
	}
}

const (
	sw2Addr = "10.5.176.96"
	sw3Addr = "10.5.176.97"
)

func vcShow(t *testing.T, addr string) string {
	t.Helper()
	return mustSSH(t, addr, "swcli -c 'show virtual-chassis'")
}

// vcRow matches a member line of "show virtual-chassis".
func vcRow(id int, rest string) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`(?m)^%d +\S* +%s`, id, rest))
}

func waitVC(t *testing.T, addr, what string, cond func(string) bool) string {
	t.Helper()
	var out string
	for i := 0; i < 120; i++ {
		if o, err := ssh(addr, "swcli -c 'show virtual-chassis'"); err == nil {
			out = o
			if cond(out) {
				return out
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s: %s: timed out:\n%s", addr, what, out)
	return ""
}

var stackIDRe = regexp.MustCompile(`Virtual chassis (\S+),`)

// The virtual chassis (sw1-sw3, reference 5.2): configuration mode from
// any member runs on the master and reaches every member; the master is
// removed from the stack and joins again while traffic flows through it,
// without losing a frame or any configuration (PLAN.md 5b).
func TestVirtualChassis(t *testing.T) {
	out := vcShow(t, sw1)
	for id := 1; id <= 3; id++ {
		if !vcRow(id, `\S+ +\d+ +voter +present`).MatchString(out) {
			t.Skipf("the lab stack (sw1-sw3, three voters) is not formed:\n%s", out)
		}
	}
	stackID := stackIDRe.FindStringSubmatch(out)[1]
	setupHost(t, hSrv1)
	setupHost(t, hSw3)
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+access(hSw3.sw1Port, "v10"))
	if !reach(t, hSrv1, hSw3, 1) {
		t.Fatal("no traffic before the test")
	}

	// Configuration mode on sw2 runs on the master (sw1) and is applied on
	// every member.
	out = mustSSH(t, sw2Addr, `swcli -c "configure
set vlans v30 description from-sw2
commit
commit
exit"`)
	for id := 1; id <= 3; id++ {
		if !strings.Contains(out, fmt.Sprintf("member%d: commit complete", id)) {
			t.Fatalf("commit from sw2:\n%s", out)
		}
	}
	for _, addr := range []string{sw1, sw3Addr} {
		var o string
		for i := 0; i < 30; i++ { // replication takes a moment on the other members
			if o = mustSSH(t, addr, "swcli -c 'show configuration vlans v30'"); strings.Contains(o, "from-sw2") {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !strings.Contains(o, "from-sw2") {
			t.Errorf("%s: commit from sw2 missing:\n%s", addr, o)
		}
	}

	// Traffic through sw1 while it is removed from the stack and joins again.
	pingDone := make(chan string, 1)
	go func() {
		o, _ := ssh(hSrv1.vm, "ip netns exec h ping -i 0.01 -w 180 -q 192.168.1.3")
		pingDone <- o
	}()
	time.Sleep(time.Second)
	out = mustSSH(t, sw2Addr, "printf 'request virtual-chassis member remove 1\\nyes\\n' | swcli")
	if !strings.Contains(out, "Member 1 removed") {
		t.Fatalf("remove the master:\n%s", out)
	}
	waitVC(t, sw1, "sw1 in a stack of its own", func(o string) bool {
		m := stackIDRe.FindStringSubmatch(o)
		return m != nil && m[1] != stackID && vcRow(1, `master`).MatchString(o)
	})
	waitVC(t, sw2Addr, "sw1 gone from the stack", func(o string) bool {
		return vcRow(1, `\S+ +\d+ +- +not joined`).MatchString(o) &&
			(vcRow(2, `master`).MatchString(o) || vcRow(3, `master`).MatchString(o))
	})
	if o := mustSSH(t, sw1, "swcli -c 'show configuration vlans v30'"); !strings.Contains(o, "from-sw2") {
		t.Errorf("the removed switch lost its configuration:\n%s", o)
	}
	tok := regexp.MustCompile(`join token (\S+)`).FindStringSubmatch(mustSSH(t, sw3Addr, "swcli -c 'request virtual-chassis member add 1'"))
	if tok == nil {
		t.Fatal("no join token")
	}
	out = mustSSH(t, sw1, "printf 'request virtual-chassis join token "+tok[1]+"\\nyes\\n' | swcli")
	if !strings.Contains(out, "Joined as member 1") {
		t.Fatalf("join:\n%s", out)
	}
	waitVC(t, sw1, "sw1 back in the stack", func(o string) bool {
		m := stackIDRe.FindStringSubmatch(o)
		return m != nil && m[1] == stackID && vcRow(1, `\S+ +\d+ +voter +present`).MatchString(o) &&
			vcRow(2, `\S+ +\d+ +voter +present`).MatchString(o) && vcRow(3, `\S+ +\d+ +voter +present`).MatchString(o)
	})
	time.Sleep(2 * time.Second)
	ssh(hSrv1.vm, "pkill -INT -f 'ping -i 0.01'")
	res := <-pingDone
	m := regexp.MustCompile(`(\d+) packets transmitted, (\d+) received`).FindStringSubmatch(res)
	if m == nil {
		t.Fatalf("ping result:\n%s", res)
	}
	tx, _ := strconv.Atoi(m[1])
	rx, _ := strconv.Atoi(m[2])
	t.Logf("%d of %d pings answered during removal and re-join", rx, tx)
	if tx < 500 || rx < tx {
		t.Errorf("traffic through sw1 was interrupted: %d of %d answered", rx, tx)
	}
	if o := mustSSH(t, sw1, "swcli -c 'show configuration vlans v30'"); !strings.Contains(o, "from-sw2") {
		t.Errorf("configuration lost across re-join:\n%s", o)
	}
	masterSw1(t)
}

// A ring survives one cut stacking cable (reference 5.2): members reach
// each other the other way round, the master stays, and commits from the
// member behind the cut work.
func TestVirtualChassisRingCut(t *testing.T) {
	out := vcShow(t, sw1)
	for id := 1; id <= 3; id++ {
		if !vcRow(id, `\S+ +\d+ +voter +present`).MatchString(out) {
			t.Skipf("the lab stack (sw1-sw3, three voters) is not formed:\n%s", out)
		}
	}
	masterSw1(t)
	// sw1 port 1/0 (ens19) is cabled to sw2 (stk-12).
	cut := "tc qdisc replace dev ens19 root netem loss 100% && (tc qdisc add dev ens19 clsact 2>/dev/null; true) && " +
		"tc filter add dev ens19 ingress pref 1 matchall action drop"
	heal := "tc qdisc del dev ens19 root 2>/dev/null; tc filter del dev ens19 ingress pref 1 2>/dev/null; true"
	t.Cleanup(func() { ssh(sw1, heal) })
	mustSSH(t, sw1, cut)
	portDown := regexp.MustCompile(`(?m)^1/0 +ens19 +down`)
	for i := 0; ; i++ {
		if portDown.MatchString(mustSSH(t, sw1, "swcli -c 'show virtual-chassis vc-port'")) {
			break
		}
		if i == 50 {
			t.Fatal("VC port 1/0 not down after the cut")
		}
		time.Sleep(100 * time.Millisecond)
	}
	out = vcShow(t, sw1)
	if !vcRow(1, `master`).MatchString(out) || !vcRow(2, `\S+ +\d+ +voter +present`).MatchString(out) {
		t.Errorf("after the cut:\n%s", out)
	}
	out = mustSSH(t, sw2Addr, fmt.Sprintf(`swcli -c "configure
set vlans v30 description across-the-ring-%d
commit
commit
exit"`, time.Now().Unix()))
	for id := 1; id <= 3; id++ {
		if !strings.Contains(out, fmt.Sprintf("member%d: commit complete", id)) {
			t.Fatalf("commit from sw2 with the cable to the master cut:\n%s", out)
		}
	}
	mustSSH(t, sw1, heal)
	portUp := regexp.MustCompile(`(?m)^1/0 +ens19 +up +member 2`)
	for i := 0; ; i++ {
		if portUp.MatchString(mustSSH(t, sw1, "swcli -c 'show virtual-chassis vc-port'")) {
			break
		}
		if i == 100 {
			t.Fatal("VC port 1/0 not up after healing")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !vcRow(1, `master`).MatchString(vcShow(t, sw1)) {
		t.Error("mastership moved because of the cut")
	}
}
