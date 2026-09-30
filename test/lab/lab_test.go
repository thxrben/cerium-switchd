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
	"maps"
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
		out := mustSSH(t, sw1, "swcli -c 'show chassis hardware local'")
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
	ssh(sw1, "swcli -c 'request chassis routing-engine master switch member 1'") // (fails when it is already)
	time.Sleep(2 * time.Second)
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
	jumbo, _ := stackJumbo(t)
	variants := []string{
		access(hSw2.sw1Port, "v10"),
		access(hSw2.sw1Port, "v30") + "set interfaces 1/ens1 description x\n",
		"set interfaces 1/ens1 unit 0 family ethernet-switching interface-mode trunk\nset interfaces 1/ens1 unit 0 family ethernet-switching vlan members [ v10 v20 ]\n",
		access(hSw2.sw1Port, "v20") + fmt.Sprintf("set interfaces 1/ens1 mtu %d\n", jumbo),
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

// stackJumbo returns the largest data mtu (frame size) the lab's stack can
// carry, at most 9014, and the host MTU that fits it: the lab's stacking
// NICs carry frames of 9014 and the tunnels need 58 more (reference 5.2),
// so it is 8956 / 8942 there and 9014 / 9000 where the stacking NICs are
// larger.
func stackJumbo(t *testing.T) (mtu, host int) {
	t.Helper()
	mtu = 9014
	for _, addr := range []string{sw1, sw2Addr, "10.5.176.97"} {
		o := mustSSH(t, addr, "swcli -c 'show virtual-chassis mtu'")
		if m := regexp.MustCompile(`allow data mtu up to (\d+)`).FindStringSubmatch(o); m != nil {
			if n, _ := strconv.Atoi(m[1]); n < mtu {
				mtu = n
			}
		}
	}
	return mtu, mtu - 14
}

func TestJumboMTU(t *testing.T) {
	jumbo, host := stackJumbo(t)
	setupHost(t, hSrv1)
	setupBondHost(t, host)
	mustSSH(t, hSrv1.vm, fmt.Sprintf("ip -n h link set ens19 mtu %d", host))
	defer mustSSH(t, hSrv1.vm, "ip -n h link set ens19 mtu 1500")
	lag := "set interfaces ae1 unit 0 family ethernet-switching vlan members v10\n" +
		"set interfaces 1/ens21 ether-options 802.3ad ae1\nset interfaces 1/ens22 ether-options 802.3ad ae1\n"
	// Default MTU 1514: jumbo frames are dropped, standard frames pass.
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+lag)
	if !reachSize(t, hSrv1, bondHost, 1, 1472) {
		t.Fatal("1500-byte packets do not pass")
	}
	if reachSize(t, hSrv1, bondHost, 1, host-28) {
		t.Errorf("%d-byte packets pass with mtu 1514", host)
	}
	// The jumbo mtu on the ports and the bundle (members inherit it).
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+lag+
		fmt.Sprintf("set interfaces 1/ens23 mtu %[1]d\nset interfaces ae1 mtu %[1]d\n", jumbo))
	out := mustSSH(t, sw1, "ip -o link show ens21 | grep -o 'mtu [0-9]*'; ip -o link show ae1 | grep -o 'mtu [0-9]*'")
	if strings.Count(out, fmt.Sprintf("mtu %d", host)) != 2 {
		t.Errorf("MTU not applied to bundle and member: %s", out)
	}
	if !reachSize(t, hSrv1, bondHost, 1, host-28) {
		t.Errorf("%d-byte packets do not pass with mtu %d", host, jumbo)
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
		"set routing-instances mgmt_ceros interface irb.99\n"
	// A data irb in the default instance, to check the protection.
	dataIRB := "set vlans v10 l3-interface irb.10\nset interfaces irb unit 10 family inet address 192.168.10.1/24\n"

	// Management on VLAN 99 (irb in mgmt_ceros).
	configure(t, base+mgmtVLAN+dataIRB+"set system services ssh port 2222\nset system services ssh root-login key-only\n")
	if _, err := ssh(hSrv1.vm, "ip netns exec h ping -c2 -W1 192.168.99.1"); err != nil {
		t.Error("management address not reachable from its VLAN")
	}
	// The CLI SSH server runs inside mgmt_ceros: reachable through the
	// management irb, not through the OS NIC or a data irb. (Port 22, the
	// OS server that the tests use, is untouched.)
	if _, err := ssh(hSrv1.vm, "ip netns exec h timeout 3 bash -c '</dev/tcp/192.168.99.1/2222'"); err != nil {
		t.Error("CLI SSH server not reachable through the management instance")
	}
	if _, err := ssh(hSw2.vm, "timeout 3 bash -c '</dev/tcp/"+sw1+"/2222'"); err == nil {
		t.Error("CLI SSH server reachable through the OS NIC, outside the management instance")
	}
	if _, err := ssh(hSw3.vm, "ip netns exec h timeout 3 bash -c '</dev/tcp/192.168.10.1/2222'"); err == nil {
		t.Error("CLI SSH server reachable through a data irb")
	}
	if out := mustSSH(t, sw1, "systemctl cat switchd-sshd | grep ExecStart="); !strings.Contains(out, "ip vrf exec mgmt_ceros") {
		t.Errorf("sshd unit:\n%s", out)
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
	for _, want := range []string{"vlan protocol 802.1Q id 99", "master mgmt_ceros", "mgmt_ceros 100", `"irb.10"`} {
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
		"set routing-instances mgmt_ceros interface 1/ens1.0\n")
	if _, err := ssh(hSw2.vm, "ip netns exec h ping -c2 -W1 192.168.98.1"); err != nil {
		t.Error("management address on the dedicated port not reachable")
	}
	out = mustSSH(t, sw1, "ip link show irb.99 2>&1; ip -o link show ens1; bridge vlan show dev swbr0 | grep -c 99; nft list table inet switchd_protect 2>&1; true")
	if !strings.Contains(out, "does not exist") || !strings.Contains(out, "master mgmt_ceros") || !strings.Contains(out, "No such file") {
		t.Errorf("after switching to a dedicated port (irb.99 gone, ens1 in mgmt_ceros, no protection table):\n%s", out)
	}

	// No management instance: switchd removes what it created.
	configure(t, base)
	out = mustSSH(t, sw1, "ip vrf show; ip -o link show ens1")
	if strings.Contains(out, "mgmt") || strings.Contains(out, ",UP") {
		t.Errorf("management not torn down:\n%s", out)
	}
}

// What the switch itself originates goes through mgmt_ceros only (reference
// 1.5): a program on the switch that connects somewhere reaches a server
// behind the management default route, and cannot reach anything through
// the OS NIC or a data interface. Replies of the OS sshd are not affected
// (this test's own ssh session survives).
func TestOriginViaManagement(t *testing.T) {
	setupHost(t, hSrv1)
	mustSSH(t, hSrv1.vm, "ip -n h addr add 192.168.99.2/24 dev ens19; ip -n h addr add 203.0.113.7/32 dev lo;"+
		"setsid -f ip netns exec h python3 -m http.server 8080 --bind 203.0.113.7 >/dev/null 2>&1 </dev/null")
	defer ssh(hSrv1.vm, "pkill -f 'http[.]server 8080'; ip -n h addr del 203.0.113.7/32 dev lo")
	dial := func(addr string) bool {
		_, err := ssh(sw1, "timeout 3 bash -c '</dev/tcp/"+addr+"'")
		return err == nil
	}
	base := vlans + "set vlans mgmt vlan-id 99\n" + access(hSrv1.sw1Port, "mgmt")
	mgmt := "set system management-instance\nset vlans mgmt l3-interface irb.99\n" +
		"set interfaces irb unit 99 family inet address 192.168.99.1/24 member 1\n" +
		"set routing-instances mgmt_ceros interface irb.99\n"
	const viaOS = "10.5.176.101/22" // srv1's own address, reachable through the OS NIC (not through mgmt)
	configure(t, base)
	if !dial(viaOS) {
		t.Fatal("sanity: srv1's OS address is not reachable from sw1 before the management instance")
	}
	if out := mustSSH(t, sw1, "ip rule show"); strings.Contains(out, "1100:") {
		t.Errorf("origin rules without a management instance:\n%s", out)
	}

	// Management instance without a default route: nothing leaves.
	configure(t, base+mgmt)
	out := mustSSH(t, sw1, "ip rule show; ip -6 rule show")
	// IPv4: unspecified source and the management address; IPv6: unspecified.
	if strings.Count(out, "1100:") != 3 || strings.Count(out, "unreachable") != 3 || !strings.Contains(out, "from 192.168.99.1 lookup") {
		t.Errorf("origin rules:\n%s", out)
	}
	if dial(viaOS) {
		t.Error("the switch reached srv1 through the OS NIC although mgmt_ceros has no route")
	}
	if dial("203.0.113.7/8080") {
		t.Error("reached an address without a management route")
	}
	// The session of this very test (OS sshd on ens18) still works, as its
	// replies keep their source address.
	mustSSH(t, sw1, "true")

	// With a default route in mgmt_ceros, "the internet" is reached through it.
	configure(t, base+mgmt+"set routing-instances mgmt_ceros routing-options static route 0.0.0.0/0 next-hop 192.168.99.2\n")
	if !dial("203.0.113.7/8080") {
		t.Errorf("no connection through the management default route:\n%s", mustSSH(t, sw1, "ip rule show; ip route show table 100; ip route get 203.0.113.7; ip neigh show; ip -br addr show irb.99; (timeout 3 bash -c '</dev/tcp/203.0.113.7/8080'; echo rc=$?) 2>&1; ip -s link show irb.99 | tail -4"))
	}
	if dial(viaOS) {
		t.Error("srv1's OS address reached outside the management instance")
	}
	// Removing the management instance removes the rules and the old paths work again.
	configure(t, base)
	if out := mustSSH(t, sw1, "ip rule show; ip -6 rule show"); strings.Contains(out, "1100:") || strings.Contains(out, "1101:") {
		t.Errorf("origin rules left behind:\n%s", out)
	}
	if !dial(viaOS) {
		t.Error("the OS path does not work after removing the management instance")
	}
}

// ntpResponder answers NTP requests on srv1's test host (192.168.99.2) with a
// clock that is ahead by seconds, and writes the source addresses it saw to
// /tmp/ntp-src.
const ntpResponder = `import socket, struct, time, sys
ahead = float(sys.argv[1])
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("192.168.99.2", 123))
def ts(t):
    t += 2208988800
    return struct.pack(">II", int(t), int((t % 1) * (1 << 32)))
while True:
    d, a = s.recvfrom(512)
    if len(d) < 48:
        continue
    open("/tmp/ntp-src", "a").write(a[0] + "\n")
    now = time.time() + ahead
    r = bytearray(48)
    r[0] = 0x24
    r[1] = 2
    r[24:32] = d[40:48]
    r[32:40] = ts(now)
    r[40:48] = ts(now)
    s.sendto(bytes(r), a)
`

// switchd's own NTP client (reference 5.1, system ntp): the queries leave
// through the management instance, the clock is stepped, show system ntp
// tells about it.
func TestNTPViaManagement(t *testing.T) {
	setupHost(t, hSrv1)
	mustSSH(t, hSrv1.vm, "ip -n h addr add 192.168.99.2/24 dev ens19; rm -f /tmp/ntp-src; cat > /tmp/ntp.py <<'EOF'\n"+ntpResponder+"\nEOF\n"+
		"setsid -f ip netns exec h python3 /tmp/ntp.py 5 >/dev/null 2>&1 </dev/null")
	defer ssh(hSrv1.vm, "pkill -f 'ntp[.]py'")
	base := vlans + "set vlans mgmt vlan-id 99\n" + access(hSrv1.sw1Port, "mgmt")
	mgmt := "set system management-instance\nset vlans mgmt l3-interface irb.99\n" +
		"set interfaces irb unit 99 family inet address 192.168.99.1/24 member 1\n" +
		"set routing-instances mgmt_ceros interface irb.99\n"
	before := time.Now().Unix()
	defer func() { // put the clock of sw1 back
		ssh(sw1, fmt.Sprintf("date -s @%d >/dev/null", time.Now().Unix()))
	}()
	configure(t, base+mgmt+"set system ntp server 192.168.99.2 prefer\n")
	var out string
	for i := 0; i < 40; i++ {
		out = mustSSH(t, sw1, "swcli -c 'show system ntp'")
		if strings.Contains(out, "Synchronized: yes") {
			break
		}
		time.Sleep(time.Second)
	}
	for _, want := range []string{"Synchronized: yes, clock stepped", "Queries leave through: mgmt_ceros", "* 192.168.99.2 (prefer)"} {
		if !strings.Contains(out, want) {
			t.Errorf("show system ntp lacks %q:\n%s", want, out)
		}
	}
	// The clock of sw1 is about 5 seconds ahead of this machine's.
	now, _ := strconv.ParseInt(strings.TrimSpace(mustSSH(t, sw1, "date +%s")), 10, 64)
	if d := now - time.Now().Unix(); d < 3 || d > 8 {
		t.Errorf("sw1's clock is %d s ahead of the lab clock, want about 5 (test started at %d)", d, before)
	}
	// The queries came from the management address.
	if src := mustSSH(t, hSrv1.vm, "sort -u /tmp/ntp-src"); strings.TrimSpace(src) != "192.168.99.1" {
		t.Errorf("NTP requests came from %q, want 192.168.99.1", strings.TrimSpace(src))
	}
	// Removing the servers stops the client.
	configure(t, base)
	if out := mustSSH(t, sw1, "swcli -c 'show system ntp'"); !strings.Contains(out, "No NTP servers configured") {
		t.Errorf("after removing the servers:\n%s", out)
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
		"set interfaces irb unit 99 family inet address 192.168.99.1/24 member 1\nset routing-instances mgmt_ceros interface irb.99\n"
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
	if out, err := sshAs("alice", "show version"); err != nil || !strings.Contains(out, "cerOS (switchd)") {
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
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "cerOS (switchd)") {
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
	if out, err := on2222("alice", "show version"); err != nil || !strings.Contains(out, "cerOS (switchd)") || !strings.Contains(out, "lab switch") {
		t.Errorf("alice on the CLI port: %v\n%s", err, out)
	}
	if out, err := on2222("root", "show version"); err == nil {
		t.Errorf("root admitted with root-login deny:\n%s", out)
	}
	configure(t, alice+"set system services ssh port 2222\nset system services ssh root-login key-only\n")
	if out, err := on2222("root", "show version"); err != nil || !strings.Contains(out, "cerOS (switchd)") {
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
		t.Errorf("no IPv6 routing between v10 and v20:\n%s\n%s", mustSSH(t, sw1, "ip -6 -br addr show irb.10; ip -6 -br addr show irb.20; ip -6 neigh; sysctl net.ipv6.conf.all.forwarding net.ipv6.conf.irb/10.forwarding"),
			mustSSH(t, hSrv1.vm, "ip -n h -6 addr; ip -n h -6 neigh; ip netns exec h ping -6 -c1 -W1 fd00:10::1; true"))
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

// Routed subinterfaces (vlan-tagging, unit N vlan-id) in several routing
// instances, with the same subnet in each: the instances keep them apart,
// tagged frames of other VLANs are dropped, and adding a unit does not
// disturb the others.
func TestRoutedUnits(t *testing.T) {
	setupHost(t, hSrv1)
	setupHost(t, hSw2)
	mustSSH(t, hSw2.vm, "for v in 100 200 300; do ip -n h link add link ens1 name ens1.$v type vlan id $v; ip -n h link set ens1.$v up; done;"+
		"ip -n h addr add 10.20.0.2/24 dev ens1.100; ip -n h addr add 10.20.0.3/24 dev ens1.200; ip -n h addr add 10.30.0.2/24 dev ens1.300")
	mustSSH(t, hSrv1.vm, "ip -n h addr add 10.20.0.5/24 dev "+hSrv1.nic)
	defer func() {
		ssh(hSw2.vm, "for v in 100 200 300; do ip -n h link del ens1.$v; done")
		ssh(hSrv1.vm, "ip -n h addr del 10.20.0.5/24 dev "+hSrv1.nic)
	}()
	pingIf := func(dev, to string) bool {
		_, err := ssh(hSw2.vm, "ip netns exec h ping -I "+dev+" -c2 -i0.2 -W1 "+to)
		return err == nil
	}
	base := vlans + access(hSrv1.sw1Port, "v10") +
		"set vlans v10 l3-interface irb.10\n" +
		"set interfaces irb unit 10 family inet address 10.20.0.1/24\n" +
		"set interfaces 1/ens1 vlan-tagging\n" +
		"set interfaces 1/ens1 unit 100 vlan-id 100\nset interfaces 1/ens1 unit 100 family inet address 10.20.0.1/24\n" +
		"set interfaces 1/ens1 unit 200 vlan-id 200\nset interfaces 1/ens1 unit 200 family inet address 10.20.0.1/24\n" +
		"set routing-instances red interface 1/ens1.100\nset routing-instances blue interface 1/ens1.200\n"
	configure(t, base)
	if !pingIf("ens1.100", "10.20.0.1") || !pingIf("ens1.200", "10.20.0.1") {
		t.Error("units 100 (red) and 200 (blue) do not answer on the same address")
	}
	if !ping1(t, hSrv1, "10.20.0.1") {
		t.Error("irb.10 in the default instance does not answer")
	}
	// Same subnet in three instances: no routing between them.
	if pingIf("ens1.100", "10.20.0.5") || pingIf("ens1.200", "10.20.0.5") {
		t.Error("routed from an instance to the default instance")
	}
	if pingIf("ens1.100", "10.20.0.3") {
		t.Error("routed from red to a host of blue")
	}
	// VLAN 300 has no unit: its frames are dropped.
	if pingIf("ens1.300", "10.30.0.1") {
		t.Error("frame of a VLAN without a unit was answered")
	}
	out := mustSSH(t, sw1, "swcli -c 'show route instance red'; ip -d link show sw-*.100 2>/dev/null | head -3; ip -br addr show vrf red")
	if !strings.Contains(out, "10.20.0.0/24") {
		t.Errorf("show route instance red:\n%s", out)
	}
	// Adding a unit (instance green) is hitless for units 100 and 200.
	done := make(chan string, 1)
	go func() {
		out, _ := ssh(hSw2.vm, "ip netns exec h ping -I ens1.100 -c 150 -i 0.02 -W1 10.20.0.1 | tail -2")
		done <- out
	}()
	time.Sleep(500 * time.Millisecond)
	configure(t, base+"set interfaces 1/ens1 unit 300 vlan-id 300\nset interfaces 1/ens1 unit 300 family inet address 10.30.0.1/24\nset routing-instances green interface 1/ens1.300\n")
	if r := <-done; !strings.Contains(r, " 0% packet loss") {
		t.Errorf("loss on unit 100 while unit 300 was added:\n%s", r)
	}
	if !pingIf("ens1.300", "10.30.0.1") {
		t.Error("new unit 300 does not answer")
	}
	// Removing everything leaves nothing behind.
	configure(t, vlans)
	out = mustSSH(t, sw1, "ip -br link show type vlan | grep -c 'sw-\\|irb' || true; ip -br link show type vrf | grep -cE 'red|blue|green' || true")
	if f := strings.Fields(out); len(f) != 2 || f[0] != "0" || f[1] != "0" {
		t.Errorf("leftovers (vlan devices, vrfs): %q", f)
	}
}

func ping1(t *testing.T, h host, to string) bool {
	t.Helper()
	_, err := ssh(h.vm, "ip netns exec h ping -c2 -i0.2 -W1 "+to)
	return err == nil
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
	desc := fmt.Sprintf("from-sw2-%d", time.Now().Unix())
	out = mustSSH(t, sw2Addr, `swcli -c "configure
set vlans v30 description `+desc+`
commit
commit
exit
show configuration vlans v30"`)
	for id := 1; id <= 3; id++ {
		if !strings.Contains(out, fmt.Sprintf("member%d: commit complete", id)) {
			t.Fatalf("commit from sw2:\n%s", out)
		}
	}
	// sw2 shows its own commit at once when configuration mode ends.
	if !strings.Contains(out[strings.Index(out, "Exiting configuration mode"):], "description "+desc) {
		t.Errorf("sw2 does not show its own commit:\n%s", out)
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

	// Operational commands on other members.
	out = mustSSH(t, sw2Addr, "swcli -c 'show virtual-chassis vc-port all-members'")
	for id := 1; id <= 3; id++ {
		if !strings.Contains(out, fmt.Sprintf("member%d:\n", id)) {
			t.Errorf("show virtual-chassis vc-port all-members lacks member %d:\n%s", id, out)
		}
	}
	if !regexp.MustCompile(`member3:\n-+\n(?s:.*)member 1 \(sw1\)`).MatchString(out) {
		t.Errorf("member 3's stacking ports missing:\n%s", out)
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

// A member that is removed from the stack becomes member 1 of a stack of
// its own: its ports are renamed, stacking and MC-LAG statements, VC ports
// and other members' addresses are gone (reference 5.2, removal).
func TestVirtualChassisRemoveRenumber(t *testing.T) {
	out := vcShow(t, sw1)
	for id := 1; id <= 3; id++ {
		if !vcRow(id, `\S+ +\d+ +voter +present`).MatchString(out) {
			t.Skipf("the lab stack (sw1-sw3, three voters) is not formed:\n%s", out)
		}
	}
	masterSw1(t)
	stackID := stackIDRe.FindStringSubmatch(out)[1]
	hw := mustSSH(t, sw3Addr, "swcli -c 'show chassis hardware local'")
	var port3 string
	for _, l := range strings.Split(hw, "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[1] == "ens21" && strings.HasPrefix(f[0], "3/") {
			port3 = f[0]
		}
	}
	if port3 == "" {
		t.Skipf("sw3 has no ens21:\n%s", hw)
	}
	one := "1" + strings.TrimPrefix(port3, "3")
	vcPorts := regexp.MustCompile(`(?m)^\d+/(\d+)/(\d+) +\S+ +(up|down)`).FindAllStringSubmatch(mustSSH(t, sw3Addr, "swcli -c 'show virtual-chassis vc-port'"), -1)
	if len(vcPorts) == 0 {
		t.Fatal("sw3 has no VC ports")
	}
	configure(t, vlans+"set vlans v10 l3-interface irb.10\n"+
		"set interfaces irb unit 10 family inet address 10.66.0.1/24 member 1\n"+
		"set interfaces irb unit 10 family inet address 10.66.0.3/24 member 3\n"+
		"set interfaces "+port3+" description renumber-marker\n")
	defer func() { // whatever happened: sw3 back in the stack
		if o := vcShow(t, sw3Addr); !strings.Contains(o, "this switch is member 3") {
			if tok := regexp.MustCompile(`join token (\S+)`).FindStringSubmatch(mustSSH(t, sw1, "swcli -c 'request virtual-chassis member add 3'")); tok != nil {
				for _, p := range vcPorts {
					ssh(sw3Addr, "swcli -c 'request virtual-chassis vc-port set pic-slot "+p[1]+" port "+p[2]+"'")
				}
				ssh(sw3Addr, "printf 'request virtual-chassis join token "+tok[1]+"\\nyes\\n' | swcli")
			}
		}
	}()
	out = mustSSH(t, sw1, "printf 'request virtual-chassis member remove 3\\nyes\\n' | swcli")
	if !strings.Contains(out, "Member 3 removed") {
		t.Fatalf("remove member 3:\n%s", out)
	}
	waitVC(t, sw3Addr, "sw3 in a stack of its own as member 1", func(o string) bool {
		m := stackIDRe.FindStringSubmatch(o)
		return m != nil && m[1] != stackID && strings.Contains(o, "this switch is member 1") && vcRow(1, `master`).MatchString(o)
	})
	set := mustSSH(t, sw3Addr, "swcli -c 'show configuration | display set'")
	for _, want := range []string{"set interfaces " + one + " description renumber-marker", "set interfaces irb unit 10 family inet address 10.66.0.3/24\n"} {
		if !strings.Contains(set+"\n", want) {
			t.Errorf("sw3 lacks %q:\n%s", want, set)
		}
	}
	for _, gone := range []string{"virtual-chassis", "mclag", port3, "10.66.0.1/24", "member 3"} {
		if strings.Contains(set, gone) {
			t.Errorf("sw3 still has %q:\n%s", gone, set)
		}
	}
	// The old stack lost it, and it can join again as member 3.
	waitVC(t, sw1, "member 3 gone", func(o string) bool { return vcRow(3, `\S+ +\d+ +- +not joined`).MatchString(o) })
	tok := regexp.MustCompile(`join token (\S+)`).FindStringSubmatch(mustSSH(t, sw1, "swcli -c 'request virtual-chassis member add 3'"))
	if tok == nil {
		t.Fatal("no join token")
	}
	out = mustSSH(t, sw3Addr, "printf 'request virtual-chassis join token "+tok[1]+"\\nyes\\n' | swcli")
	if !strings.Contains(out, "Joined as member 3") {
		t.Fatalf("join:\n%s", out)
	}
	waitVC(t, sw1, "sw3 back", func(o string) bool {
		m := stackIDRe.FindStringSubmatch(o)
		return m != nil && m[1] == stackID && vcRow(3, `\S+ +\d+ +voter +present`).MatchString(o)
	})
	masterSw1(t)
	configure(t, vlans)
}

// Every member of the stack uses the same gateway MAC on its bridge and irb
// units (anycast gateway, reference 5.3.3).
func TestGatewayMAC(t *testing.T) {
	configure(t, vlans+"set vlans v10 l3-interface irb.10\nset interfaces irb unit 10 family inet address 10.77.0.1/24\n")
	defer configure(t, vlans)
	var first string
	for _, a := range []string{sw1, sw2Addr, sw3Addr} {
		var out string
		for i := 0; i < 20; i++ {
			out = mustSSH(t, a, "cat /sys/class/net/swbr0/address /sys/class/net/irb.10/address 2>/dev/null")
			if f := strings.Fields(out); len(f) == 2 {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		f := strings.Fields(out)
		if len(f) != 2 || f[0] != f[1] {
			t.Fatalf("%s: bridge and irb.10 MAC differ: %q", a, f)
		}
		if first == "" {
			first = f[0]
		} else if f[0] != first {
			t.Errorf("%s uses gateway MAC %s, member 1 uses %s", a, f[0], first)
		}
	}
}

// force-master (reference 5.2): sw2 and sw3 are down, sw1 has no majority;
// the override lets it commit again, and the others catch up when they
// return.
func TestForceMaster(t *testing.T) {
	out := vcShow(t, sw1)
	for id := 1; id <= 3; id++ {
		if !vcRow(id, `\S+ +\d+ +voter +present`).MatchString(out) {
			t.Skipf("the lab stack (sw1-sw3, three voters) is not formed:\n%s", out)
		}
	}
	masterSw1(t)
	configure(t, vlans)
	defer func() {
		ssh(sw2Addr, "systemctl start switchd")
		ssh(sw3Addr, "systemctl start switchd")
	}()
	mustSSH(t, sw2Addr, "systemctl stop switchd")
	mustSSH(t, sw3Addr, "systemctl stop switchd")
	waitVC(t, sw1, "no master", func(o string) bool { return !vcRow(1, `master`).MatchString(o) })
	if o, _ := ssh(sw1, "printf 'request virtual-chassis force-master\\nyes\\n' | swcli"); !strings.Contains(o, "restarts") {
		t.Fatalf("force-master:\n%s", o)
	}
	time.Sleep(3 * time.Second)
	waitVC(t, sw1, "sw1 master alone", func(o string) bool { return vcRow(1, `master`).MatchString(o) })
	desc := fmt.Sprintf("forced-%d", time.Now().Unix())
	mustSSH(t, sw1, "cat > /root/f.set <<'EOF'\nset vlans v10 description "+desc+"\nEOF")
	if o := mustSSH(t, sw1, "swcli -c \"configure\nload merge f.set\ncommit\ncommit\nexit\""); !strings.Contains(o, "commit complete") {
		t.Fatalf("commit after force-master:\n%s", o)
	}
	mustSSH(t, sw2Addr, "systemctl start switchd")
	mustSSH(t, sw3Addr, "systemctl start switchd")
	waitVC(t, sw1, "all voters again", func(o string) bool {
		for id := 1; id <= 3; id++ {
			if !vcRow(id, `\S+ +\d+ +voter +present`).MatchString(o) {
				return false
			}
		}
		return true
	})
	for _, addr := range []string{sw2Addr, sw3Addr} {
		var o string
		for i := 0; i < 60; i++ {
			if o = mustSSH(t, addr, "swcli -c 'show configuration vlans v10'"); strings.Contains(o, desc) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !strings.Contains(o, desc) {
			t.Errorf("%s did not take over the forced commit:\n%s", addr, o)
		}
	}
	masterSw1(t)
	configure(t, vlans)
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
	portDown := regexp.MustCompile(`(?m)^1/1/0 +ens19 +down`)
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
	portUp := regexp.MustCompile(`(?m)^1/1/0 +ens19 +up +\S+ +member 2`)
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

// Stacking frames (EtherType 0x88b5) that arrive on a data port are data:
// they are switched like any frame, unchanged, and never reach the stack
// (reference 1.5, planes; PLAN.md Phase 5 step 4).
func TestStackingFramesOnDataPorts(t *testing.T) {
	setupHost(t, hSrv1)
	setupHost(t, hSw3)
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+access(hSw3.sw1Port, "v10"))
	capture := make(chan string, 1)
	go func() {
		o, _ := ssh(hSw3.vm, "timeout 6 ip netns exec h tcpdump -c 3 -nn -x -i "+hSw3.nic+" ether proto 0x88b5 2>/dev/null")
		capture <- o
	}()
	time.Sleep(2 * time.Second)
	// Broadcast frames with the stacking EtherType and a recognisable payload.
	send := `ip netns exec h python3 -c '
import socket
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW)
s.bind(("` + hSrv1.nic + `", 0))
mac = open("/sys/class/net/` + hSrv1.nic + `/address").read().strip().replace(":", "")
f = bytes.fromhex("ffffffffffff" + mac + "88b5") + b"MJOINFORGEDSTACKFRAME" + bytes(40)
for _ in range(3): s.send(f)
'`
	mustSSH(t, hSrv1.vm, send)
	out := <-capture
	if strings.Count(out, "0x88b5") < 3 && strings.Count(out, "ethertype Unknown") < 3 {
		t.Fatalf("stacking-EtherType frames were not switched to the other data port:\n%s", out)
	}
	// "MJOI" = 4d4a 4f49: the payload arrives unchanged.
	if !strings.Contains(strings.ReplaceAll(out, " ", ""), "4d4a4f494e464f52474544") {
		t.Errorf("payload changed:\n%s", out)
	}
	// The frames look like a join attempt; the stack must not have seen them.
	if logs := mustSSH(t, sw1, "journalctl -u switchd --no-pager -o cat --since -8s"); strings.Contains(logs, "join") {
		t.Errorf("the stack reacted to frames on a data port:\n%s", logs)
	}
}

// The master fails (switchd killed): another member takes over, commits
// work, and the old master catches up when it is back.
func TestVirtualChassisMasterKilled(t *testing.T) {
	out := vcShow(t, sw1)
	for id := 1; id <= 3; id++ {
		if !vcRow(id, `\S+ +\d+ +voter +present`).MatchString(out) {
			t.Skipf("the lab stack (sw1-sw3, three voters) is not formed:\n%s", out)
		}
	}
	masterSw1(t)
	mustSSH(t, sw1, "systemctl kill -s KILL switchd")
	waitVC(t, sw2Addr, "a new master", func(o string) bool {
		return vcRow(2, `master`).MatchString(o) || vcRow(3, `master`).MatchString(o)
	})
	desc := fmt.Sprintf("after-kill-%d", time.Now().Unix())
	out = mustSSH(t, sw3Addr, `swcli -c "configure
set vlans v30 description `+desc+`
commit
commit
exit"`)
	if !strings.Contains(out, "member2: commit complete") || !strings.Contains(out, "member3: commit complete") {
		t.Fatalf("commit after the master was killed:\n%s", out)
	}
	waitVC(t, sw1, "sw1 back", func(o string) bool { return vcRow(1, `\S+ +\d+ +voter +present`).MatchString(o) })
	for i := 0; ; i++ {
		if strings.Contains(mustSSH(t, sw1, "swcli -c 'show configuration vlans v30'"), desc) {
			break
		}
		if i == 50 {
			t.Fatal("sw1 did not catch up with the commit it missed")
		}
		time.Sleep(200 * time.Millisecond)
	}
	masterSw1(t)
}

// A member cut off from the others (minority) keeps forwarding with the
// last configuration; configuration mode fails with a message; the majority
// commits; the member catches up when the cables are back.
func TestVirtualChassisPartition(t *testing.T) {
	out := vcShow(t, sw1)
	for id := 1; id <= 3; id++ {
		if !vcRow(id, `\S+ +\d+ +voter +present`).MatchString(out) {
			t.Skipf("the lab stack (sw1-sw3, three voters) is not formed:\n%s", out)
		}
	}
	setupHost(t, hSrv1)
	setupHost(t, hSw3)
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+access(hSw3.sw1Port, "v10"))
	pingDone := make(chan string, 1)
	go func() {
		o, _ := ssh(hSrv1.vm, "ip netns exec h ping -i 0.01 -w 120 -q 192.168.1.3")
		pingDone <- o
	}()
	time.Sleep(time.Second)
	// Both stacking ports of sw1 (1/0 = ens19, 2/0 = ens20).
	var cut, heal []string
	for _, p := range []string{"ens19", "ens20"} {
		cut = append(cut, "tc qdisc replace dev "+p+" root netem loss 100% && (tc qdisc add dev "+p+" clsact 2>/dev/null; true) && "+
			"tc filter add dev "+p+" ingress pref 1 matchall action drop")
		heal = append(heal, "tc qdisc del dev "+p+" root 2>/dev/null; tc filter del dev "+p+" ingress pref 1 2>/dev/null")
	}
	healAll := strings.Join(heal, "; ") + "; true"
	t.Cleanup(func() { ssh(sw1, healAll) })
	mustSSH(t, sw1, strings.Join(cut, " && "))
	waitVC(t, sw1, "sw1 without master", func(o string) bool { return strings.Contains(o, "No master") })
	out = mustSSH(t, sw1, "swcli -c 'configure' || true")
	if !strings.Contains(out, "configuration unavailable") {
		t.Errorf("configure on the minority side:\n%s", out)
	}
	desc := fmt.Sprintf("during-partition-%d", time.Now().Unix())
	waitVC(t, sw2Addr, "a master on the majority side", func(o string) bool {
		return vcRow(2, `master`).MatchString(o) || vcRow(3, `master`).MatchString(o)
	})
	out = mustSSH(t, sw2Addr, `swcli -c "configure
set vlans v30 description `+desc+`
commit
commit
exit"`)
	if !strings.Contains(out, "member1: pending") || !strings.Contains(out, "member3: commit complete") {
		t.Fatalf("commit on the majority side:\n%s", out)
	}
	mustSSH(t, sw1, healAll)
	waitVC(t, sw1, "sw1 back in the stack", func(o string) bool {
		return !strings.Contains(o, "No master") && vcRow(2, `\S+ +\d+ +voter +present`).MatchString(o)
	})
	for i := 0; ; i++ {
		if strings.Contains(mustSSH(t, sw1, "swcli -c 'show configuration vlans v30'"), desc) {
			break
		}
		if i == 100 {
			t.Fatal("sw1 did not catch up with the commit made during the partition")
		}
		time.Sleep(200 * time.Millisecond)
	}
	ssh(hSrv1.vm, "pkill -INT -f 'ping -i 0.01'")
	res := <-pingDone
	m := regexp.MustCompile(`(\d+) packets transmitted, (\d+) received`).FindStringSubmatch(res)
	if m == nil {
		t.Fatalf("ping result:\n%s", res)
	}
	tx, _ := strconv.Atoi(m[1])
	rx, _ := strconv.Atoi(m[2])
	t.Logf("%d of %d pings answered through the partitioned member", rx, tx)
	if tx < 300 || rx < tx {
		t.Errorf("forwarding interrupted during the partition: %d of %d answered", rx, tx)
	}
	masterSw1(t)
}

// setupLACPHost bonds sw2's peer1/peer2 NICs (cabled to sw1's ens21/ens22)
// with Linux 802.3ad LACP in namespace "b".
func setupLACPHost(t *testing.T) {
	t.Helper()
	mustSSH(t, bondHost.vm, `ip netns add b 2>/dev/null; for i in ens21 ens22; do ip link set $i netns b 2>/dev/null; done
ip -n b link del bond0 2>/dev/null; ip -n b link set lo up
ip -n b link add bond0 type bond mode 802.3ad lacp_rate fast xmit_hash_policy layer3+4 miimon 100
# 802.3ad needs a known speed and duplex; virtio NICs have none until set.
for i in ens21 ens22; do ip netns exec b ethtool -s $i speed 10000 duplex full autoneg off; done
for i in ens21 ens22; do ip -n b link set $i down; ip -n b link set $i master bond0; done
ip -n b link set bond0 up; ip -n b addr add 192.168.1.22/24 dev bond0`)
}

// LACP (reference 5.1.3) against a Linux 802.3ad bond: the bundle forms on
// both ports, a failed port leaves it, and restarting switchd does not
// disturb the partner.
func TestLACP(t *testing.T) {
	setupHost(t, hSrv1)
	setupLACPHost(t)
	t.Cleanup(func() { setupBondHost(t, 1500) }) // back to the static bond the other tests expect
	lag := "set interfaces ae1 aggregated-ether-options lacp active\nset interfaces ae1 aggregated-ether-options lacp periodic fast\n" +
		"set interfaces ae1 unit 0 family ethernet-switching vlan members v10\n" +
		"set interfaces 1/ens21 ether-options 802.3ad ae1\nset interfaces 1/ens22 ether-options 802.3ad ae1\n"
	configure(t, vlans+access(hSrv1.sw1Port, "v10")+lag)
	both := regexp.MustCompile(`(?s)Current +Fast periodic Collecting distributing.*Current +Fast periodic Collecting distributing`)
	var out string
	for i := 0; ; i++ {
		out = mustSSH(t, sw1, "swcli -c 'show lacp interfaces ae1'")
		if both.MatchString(out) {
			break
		}
		if i == 60 {
			t.Fatalf("bundle did not form:\n%s\n%s", out, mustSSH(t, bondHost.vm, "ip netns exec b cat /proc/net/bonding/bond0"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	// The Linux side sees one partner on both ports, in one aggregator.
	bond := mustSSH(t, bondHost.vm, "ip netns exec b cat /proc/net/bonding/bond0")
	ids := regexp.MustCompile(`(?m)^\s*Aggregator ID: (\d+)`).FindAllStringSubmatch(bond, -1)
	if len(ids) != 3 || ids[1][1] != ids[0][1] || ids[2][1] != ids[0][1] || !strings.Contains(bond, "Number of ports: 2") {
		t.Errorf("the Linux bond did not aggregate both ports:\n%s", bond)
	}
	sysMAC := regexp.MustCompile(`Actor system: \d+,(\S+),`).FindStringSubmatch(out)
	if sysMAC == nil || !strings.Contains(bond, "Partner Mac Address: "+sysMAC[1]) {
		t.Errorf("the Linux bond's partner is not this switch's LACP system:\n%s", bond)
	}
	if !reach(t, hSrv1, bondHost, 1) {
		t.Fatal("no traffic over the LACP bundle")
	}
	res := mustSSH(t, hSrv1.vm, "for p in $(seq 1 20); do ip netns exec h ping -c1 -W1 192.168.1.22 >/dev/null && echo ok; done | wc -l")
	if strings.TrimSpace(res) != "20" {
		t.Errorf("only %s of 20 pings over the bundle", strings.TrimSpace(res))
	}

	// switchd restarts under traffic: the partner must not notice. (A fixed
	// count: stopping ping early would count the request in flight as lost.)
	pingDone := make(chan string, 1)
	go func() {
		o, _ := ssh(hSrv1.vm, "ip netns exec h ping -i 0.01 -c 800 -q 192.168.1.22")
		pingDone <- o
	}()
	time.Sleep(time.Second)
	mustSSH(t, sw1, "systemctl restart switchd")
	res = <-pingDone
	m := regexp.MustCompile(`(\d+) packets transmitted, (\d+) received`).FindStringSubmatch(res)
	if m == nil {
		t.Fatalf("ping result:\n%s", res)
	}
	tx, _ := strconv.Atoi(m[1])
	rx, _ := strconv.Atoi(m[2])
	t.Logf("%d of %d pings answered across a switchd restart", rx, tx)
	if rx < tx {
		t.Errorf("switchd restart disturbed the LACP bundle: %d of %d answered", rx, tx)
	}
	if o := mustSSH(t, sw1, "journalctl -u switchd --no-pager -o cat --since -20s"); !strings.Contains(o, "lacp: port state restored") {
		t.Errorf("LACP state not restored after the restart:\n%s", o)
	}

	// A port whose partner goes away leaves the bundle; traffic continues.
	mustSSH(t, bondHost.vm, "ip -n b link set ens21 down")
	for i := 0; ; i++ {
		out = mustSSH(t, sw1, "swcli -c 'show lacp interfaces ae1'")
		if strings.Count(out, "Collecting distributing") == 1 {
			break
		}
		if i == 50 {
			t.Fatalf("failed port still in the bundle:\n%s", out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !reach(t, hSrv1, bondHost, 1) {
		t.Error("no traffic with one port left")
	}
	mustSSH(t, bondHost.vm, "ip -n b link set ens21 up")
	for i := 0; ; i++ {
		if both.MatchString(mustSSH(t, sw1, "swcli -c 'show lacp interfaces ae1'")) {
			break
		}
		if i == 60 {
			t.Fatal("port did not rejoin")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if o := mustSSH(t, sw1, "swcli -c 'show lacp statistics interfaces ae1'"); !regexp.MustCompile(`1/\d+/0 +[1-9]\d* +[1-9]`).MatchString(o) {
		t.Errorf("statistics:\n%s", o)
	}
}

// memberPort returns member addr's switch name of a Linux port.
func memberPort(t *testing.T, addr, linux string) string {
	t.Helper()
	for _, l := range strings.Split(mustSSH(t, addr, "swcli -c 'show chassis hardware local'"), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[1] == linux && strings.Count(f[0], "/") == 2 {
			return f[0]
		}
	}
	t.Fatalf("%s has no port %s", addr, linux)
	return ""
}

// MC-LAG (reference 5.6): srv1 bonds srv1-a (to sw1) and srv1-b (to sw2)
// with LACP and sees one partner; sw1 and sw2 are the domain, and the stack
// tunnel between them replaces a peer-link. Traffic reaches a single-homed
// host on sw1 without duplicates, survives the loss of either leg and of a
// ring cable, and a member cut off from the stack (minority) holds its leg.
func TestMCLAG(t *testing.T) {
	out := vcShow(t, sw1)
	for id := 1; id <= 2; id++ {
		if !vcRow(id, `\S+ +\d+ +voter +present`).MatchString(out) {
			t.Skipf("sw1 and sw2 are not in one stack:\n%s", out)
		}
	}
	// sw2's peer1/peer2 become switch ports again (the LAG tests use them in
	// a namespace), srv1 bonds its two NICs.
	mustSSH(t, sw2Addr, "for i in ens21 ens22; do ip -n b link set $i netns 1 2>/dev/null; done; ip netns del b 2>/dev/null; true")
	mustSSH(t, hSrv1.vm, `ip netns add m 2>/dev/null; ip -n h link set ens19 netns m 2>/dev/null; ip link set ens19 netns m 2>/dev/null; ip link set ens20 netns m 2>/dev/null
ip -n m link del bond0 2>/dev/null; ip -n m link set lo up
ip -n m link add bond0 type bond mode 802.3ad lacp_rate fast xmit_hash_policy layer3+4 miimon 100
for i in ens19 ens20; do ip netns exec m ethtool -s $i speed 10000 duplex full autoneg off; ip -n m link set $i down; ip -n m link set $i master bond0; done
ip -n m link set bond0 up; ip -n m addr add 192.168.1.1/24 dev bond0`)
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("LAB_KEEP") != "" {
			return // leave the setup for inspection
		}
		ssh(hSrv1.vm, "ip -n m link del bond0; ip -n m link set ens19 netns h; ip -n m link set ens20 netns 1; ip netns del m")
		setupBondHost(t, 1500) // sw2's peer1/peer2 back into namespace b
	})
	setupHost(t, hSw3)
	time.Sleep(2 * time.Second) // sw2 numbers the returned ports
	p1, p2, srv1b := memberPort(t, sw2Addr, "ens21"), memberPort(t, sw2Addr, "ens22"), memberPort(t, sw2Addr, "ens23")
	_, _ = p1, p2 // (peer1/peer2 stay unconfigured: no peer-link)
	cfg := vlans + access(hSw3.sw1Port, "v10") +
		"set interfaces ae1 aggregated-ether-options lacp active\nset interfaces ae1 aggregated-ether-options lacp periodic fast\n" +
		"set interfaces ae1 aggregated-ether-options mclag\nset interfaces ae1 unit 0 family ethernet-switching vlan members v10\n" +
		"set interfaces 1/ens23 ether-options 802.3ad ae1\nset interfaces " + srv1b + " ether-options 802.3ad ae1\n" +
		"set mclag domain 1 members [ 1 2 ]\nset mclag domain 1 delay-restore 5\n"
	configure(t, cfg)

	// srv1 aggregates both legs towards one partner.
	var bond string
	for i := 0; ; i++ {
		bond = mustSSH(t, hSrv1.vm, "ip netns exec m cat /proc/net/bonding/bond0")
		if strings.Contains(bond, "Number of ports: 2") {
			break
		}
		if i == 100 {
			t.Fatalf("srv1 did not aggregate both legs:\n%s\nsw1:\n%s\nsw2:\n%s", bond,
				mustSSH(t, sw1, "swcli -c 'show mclag'; swcli -c 'show lacp interfaces ae1'"),
				mustSSH(t, sw2Addr, "swcli -c 'show mclag'; swcli -c 'show lacp interfaces ae1'"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	ping := func(what string, n int) bool {
		t.Helper()
		o, _ := ssh(hSw3.vm, fmt.Sprintf("ip netns exec h ping -i 0.01 -c %d 192.168.1.1", n))
		m := regexp.MustCompile(`(\d+) packets transmitted, (\d+) received`).FindStringSubmatch(o)
		if m == nil {
			t.Fatalf("%s: ping:\n%s", what, o)
		}
		if m[1] != m[2] || strings.Contains(o, "DUP!") {
			t.Errorf("%s: %s of %s answered, duplicates: %v", what, m[2], m[1], strings.Contains(o, "DUP!"))
			srvMAC := strings.TrimSpace(mustSSH(t, hSrv1.vm, "ip netns exec m cat /sys/class/net/bond0/address"))
			t.Logf("srv1 bond0 %s\nsw1:\n%s\nsw2:\n%s\nsrv1:\n%s", srvMAC,
				mustSSH(t, sw1, "swcli -c 'show mclag'; swcli -c 'show lacp interfaces ae1'; bridge fdb show br swbr0 | grep -v permanent; nft list table bridge switchd_mclag 2>&1 | grep -v '^$'; tc filter show dev ens2 ingress; tc filter show dev ae1 ingress; ip -o link show ae1 | cut -c1-100; journalctl -u switchd --since -25s --no-pager -o cat | grep -v 'cli command\\|cli: log'; true"),
				mustSSH(t, sw2Addr, "swcli -c 'show mclag'; swcli -c 'show lacp interfaces ae1'; bridge fdb show br swbr0 | grep -v permanent; nft list table bridge switchd_mclag; tc filter show dev ae1 ingress; ip -o link show ae1 | cut -c1-100; journalctl -u switchd --since -25s --no-pager -o cat | grep -v 'cli command\\|cli: log'; true"),
				mustSSH(t, hSrv1.vm, "ip netns exec m cat /proc/net/bonding/bond0 | grep -E 'Slave Interface|MII|Aggregator ID|port state'; ip -n m -o link show | cut -c1-90; true"))
			return false
		}
		return true
	}
	waitLeg0 := regexp.MustCompile(`ae1 +up +up `)
	for i := 0; !waitLeg0.MatchString(mustSSH(t, sw2Addr, "swcli -c 'show mclag'")); i++ {
		if i == 50 {
			t.Fatal("sw2's leg not up")
		}
		time.Sleep(200 * time.Millisecond)
	}
	ping("both legs", 300)
	if o := mustSSH(t, sw1, "swcli -c 'show mclag'"); !regexp.MustCompile(`ae1 +up +up +on`).MatchString(o) {
		t.Errorf("sw1 show mclag:\n%s", o)
	}
	for _, addr := range []string{sw1, sw2Addr} {
		if o := mustSSH(t, addr, "swcli -c 'show mclag consistency'"); !strings.Contains(o, "ae1: consistent") {
			t.Errorf("%s show mclag consistency:\n%s", addr, o)
		}
	}
	// MAC synchronisation: sw2 knows hSw3 (single-homed on sw1) via the
	// tunnel to sw1 and srv1 on its own leg, whichever leg srv1's frames
	// took; the tunnel between the peers learns nothing.
	h3MAC := strings.TrimSpace(mustSSH(t, hSw3.vm, "ip netns exec h cat /sys/class/net/"+hSw3.nic+"/address"))
	s1MAC := strings.TrimSpace(mustSSH(t, hSrv1.vm, "ip netns exec m cat /sys/class/net/bond0/address"))
	for i := 0; ; i++ {
		fdb := mustSSH(t, sw2Addr, "bridge fdb show br swbr0 | grep -v permanent")
		if regexp.MustCompile(`(?m)^`+h3MAC+` dev swvc1 vlan 10 extern_learn`).MatchString(fdb) &&
			regexp.MustCompile(`(?m)^`+s1MAC+` dev ae1 vlan 10`).MatchString(fdb) {
			break
		}
		if i == 25 {
			t.Fatalf("sw2's address table is not synchronised (hSw3 %s, srv1 %s):\n%s", h3MAC, s1MAC, fdb)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if o := mustSSH(t, sw1, "bridge -d link show dev swvc2"); !strings.Contains(o, "learning off") || !strings.Contains(o, "isolated on") {
		t.Errorf("sw1's tunnel to its peer learns addresses or is not isolated:\n%s", o)
	}

	// Either leg fails: traffic continues through the other member. (A VM
	// link keeps its carrier when the other end goes down, so the switch
	// notices through the LACP timeout, 3 s; the test waits for that.)
	waitLeg := func(addr, state string) {
		t.Helper()
		re := regexp.MustCompile(`ae1 +` + state + ` `)
		for i := 0; ; i++ {
			o := mustSSH(t, addr, "swcli -c 'show mclag'")
			if re.MatchString(o) {
				return
			}
			if i == 50 {
				t.Fatalf("%s: leg not %s:\n%s", addr, state, o)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	for _, leg := range []struct{ nic, sw string }{{"ens19", sw1}, {"ens20", sw2Addr}} {
		mustSSH(t, hSrv1.vm, "ip -n m link set "+leg.nic+" down")
		waitLeg(leg.sw, "down")
		time.Sleep(500 * time.Millisecond)
		pre := mustSSH(t, sw1, "bridge fdb show br swbr0 | grep -i "+s1MAC+" || echo none") + mustSSH(t, sw2Addr, "bridge fdb show br swbr0 | grep -i "+s1MAC+" || echo none")
		if !ping("without "+leg.nic, 200) {
			t.Logf("srv1's address before the ping (sw1, sw2):\n%s", pre)
			t.Logf("where the frames go:\n%s", traceHops(t))
			if os.Getenv("LAB_KEEP") != "" {
				t.FailNow() // keep the state for inspection
			}
		}
		leg := leg.nic
		mustSSH(t, hSrv1.vm, "ip -n m link set "+leg+" up")
		waitLeg(sw1, "up +up")
		waitLeg(sw2Addr, "up +up")
	}

	// A ring cable fails (stk-12, sw1-sw2) under traffic: the tunnel
	// between the peers moves around the ring (via sw3) within the stacking
	// BFD time; nothing is held. (The cut keeps the links up, as a failed
	// media converter would: switchd would put a port that is set down up
	// again.)
	cut := func(addr string, neighbors ...int) (heal string) {
		t.Helper()
		var cmds, heals []string
		for _, p := range stackPortsTo(t, addr, neighbors...) {
			cmds = append(cmds, "tc qdisc replace dev "+p+" root netem loss 100% && (tc qdisc add dev "+p+" clsact 2>/dev/null; true) && "+
				"tc filter add dev "+p+" ingress pref 1 matchall action drop")
			heals = append(heals, "tc qdisc del dev "+p+" root 2>/dev/null; tc filter del dev "+p+" ingress pref 1 2>/dev/null")
		}
		heal = strings.Join(heals, "; ") + "; true"
		t.Cleanup(func() { ssh(addr, heal) })
		mustSSH(t, addr, strings.Join(cmds, " && "))
		return heal
	}
	pingBg := func(n int) <-chan string {
		done := make(chan string, 1)
		go func() {
			o, _ := ssh(hSw3.vm, fmt.Sprintf("ip netns exec h ping -i 0.01 -c %d -W 1 192.168.1.1", n))
			done <- o
		}()
		return done
	}
	lost := func(o string) int {
		m := regexp.MustCompile(`(\d+) packets transmitted, (\d+) received`).FindStringSubmatch(o)
		if m == nil {
			t.Fatalf("ping:\n%s", o)
		}
		tx, _ := strconv.Atoi(m[1])
		rx, _ := strconv.Atoi(m[2])
		return tx - rx
	}
	// Maintenance mode on sw2 under traffic: mastership leaves it, its leg
	// leaves the bundle after srv1 stopped sending on it; nothing is lost.
	// Exit brings the leg back, again without loss.
	bg := pingBg(800)
	time.Sleep(time.Second)
	o, err := ssh(sw2Addr, "printf 'request system maintenance-mode enter\\nyes\\n' | swcli")
	res := <-bg
	if err != nil || !strings.Contains(o, "drained") {
		t.Fatalf("maintenance-mode enter on sw2 (%v):\n%s", err, o)
	}
	if n := lost(res); n > 0 || strings.Contains(res, "DUP!") {
		t.Errorf("maintenance-mode enter: %d of 800 pings lost, duplicates %v", n, strings.Contains(res, "DUP!"))
	}
	vc := vcShow(t, sw1)
	if !vcRow(2, `\S+ +\d+ +voter +maintenance`).MatchString(vc) || vcRow(2, `master`).MatchString(vc) {
		t.Errorf("show virtual-chassis with sw2 in maintenance mode:\n%s", vc)
	}
	if o := mustSSH(t, sw2Addr, "swcli -c 'show mclag'"); !strings.Contains(o, "maintenance mode") {
		t.Errorf("sw2 show mclag in maintenance mode:\n%s", o)
	}
	if o, _ := ssh(sw1, "printf 'request system maintenance-mode enter\\nyes\\n' | swcli"); !strings.Contains(o, "is in maintenance mode") {
		t.Errorf("sw1 entered maintenance mode while its peer is in it:\n%s", o)
	}
	ping("sw2 in maintenance mode", 200)
	bg = pingBg(800)
	time.Sleep(time.Second)
	mustSSH(t, sw2Addr, "swcli -c 'request system maintenance-mode exit'")
	waitLeg(sw2Addr, "up +up")
	res = <-bg
	if n := lost(res); n > 0 || strings.Contains(res, "DUP!") {
		t.Errorf("maintenance-mode exit: %d of 800 pings lost, duplicates %v", n, strings.Contains(res, "DUP!"))
	}
	if vc := vcShow(t, sw1); !vcRow(2, `\S+ +\d+ +voter +present`).MatchString(vc) {
		t.Errorf("show virtual-chassis after exit:\n%s", vc)
	}
	// The master (sw1, which also serves hSw3 alone) drains: mastership
	// moves, hSw3's port is reported as not drained, traffic continues.
	ssh(sw1, "swcli -c 'request chassis routing-engine master switch member 1'") // (fails when it is already)
	time.Sleep(2 * time.Second)
	bg = pingBg(800)
	time.Sleep(time.Second)
	o, err = ssh(sw1, "printf 'request system maintenance-mode enter\\nyes\\n' | swcli")
	res = <-bg
	if err != nil || !strings.Contains(o, "mastership handed on") || !strings.Contains(o, "not drained (only this member serves them): 1/") {
		t.Errorf("maintenance-mode enter on the master (%v):\n%s", err, o)
	}
	if n := lost(res); n > 0 {
		t.Errorf("maintenance-mode enter on the master: %d of 800 pings lost", n)
	}
	if vc := vcShow(t, sw2Addr); vcRow(1, `master`).MatchString(vc) {
		t.Errorf("sw1 still master in maintenance mode:\n%s", vc)
	}
	mustSSH(t, sw1, "swcli -c 'request system maintenance-mode exit'")
	waitLeg(sw1, "up +up")

	bg = pingBg(500)
	time.Sleep(time.Second)
	heal := cut(sw2Addr, 1)
	res = <-bg
	n := lost(res)
	t.Logf("ring cable sw1-sw2 cut under traffic: %d of 500 pings lost (10 ms apart)", n)
	if n > 60 || strings.Contains(res, "DUP!") {
		t.Errorf("ring cable cut: %d lost, duplicates %v", n, strings.Contains(res, "DUP!"))
	}
	if o := mustSSH(t, sw2Addr, "swcli -c 'show mclag'"); !regexp.MustCompile(`ae1 +up +up +\S+ +-`).MatchString(o) {
		t.Errorf("sw2 after the ring cut:\n%s", o)
	}
	ping("ring cable cut", 200)
	mustSSH(t, sw2Addr, heal)
	for i := 0; !regexp.MustCompile(`(?m) up +\S+ +member 1 `).MatchString(mustSSH(t, sw2Addr, "swcli -c 'show virtual-chassis vc-port'")); i++ {
		if i == 60 {
			t.Fatal("the sw1-sw2 stacking link did not return")
		}
		time.Sleep(500 * time.Millisecond)
	}

	// sw2 loses both stacking cables: it is the minority (1 of 3) and holds
	// its leg; srv1 uses sw1 only, traffic continues.
	cut(sw2Addr, 1, 3)
	for i := 0; ; i++ {
		o := mustSSH(t, sw2Addr, "swcli -c 'show mclag'")
		if strings.Contains(o, "minority part of the stack") {
			break
		}
		if i == 50 {
			t.Fatalf("sw2 does not hold its leg:\n%s", o)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// srv1's bond: sw2's port (ens20) has a partner that is not in sync.
	for i := 0; ; i++ {
		b := mustSSH(t, hSrv1.vm, "ip netns exec m cat /proc/net/bonding/bond0")
		m := regexp.MustCompile(`(?s)Slave Interface: ens20.*?details partner lacp pdu:.*?port state: (\d+)`).FindStringSubmatch(b)
		if m != nil {
			if st, _ := strconv.Atoi(m[1]); st&0x08 == 0 {
				break
			}
		}
		if i == 30 {
			t.Fatalf("srv1 still sees sw2's held leg in sync:\n%s", b)
		}
		time.Sleep(200 * time.Millisecond)
	}
	ping("sw2 cut off", 200)
}

// stackPortsTo returns the Linux names of member addr's stacking ports whose
// neighbour is one of the given members.
func stackPortsTo(t *testing.T, addr string, neighbors ...int) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(mustSSH(t, addr, "swcli -c 'show virtual-chassis vc-port'"), "\n") {
		f := strings.Fields(l)
		if len(f) < 6 || f[2] != "up" || f[4] != "member" {
			continue
		}
		for _, n := range neighbors {
			if f[5] == strconv.Itoa(n) {
				out = append(out, f[1])
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s has no stacking link to %v", addr, neighbors)
	}
	return out
}

// traceHops pings srv1 from hSw3 three times while capturing on every hop
// of the MC-LAG test (diagnostics).
func traceHops(t *testing.T) string {
	t.Helper()
	f := `"icmp or (vlan and icmp)"`
	type cap struct{ addr, dev string }
	caps := []cap{{sw1, "ens2"}, {sw1, "ens19"}, {sw1, "ens20"}, {sw1, "ens23"}, {sw2Addr, "ens19"}, {sw2Addr, "ens2"}, {sw2Addr, "ens23"}}
	out := make([]string, len(caps))
	var wg sync.WaitGroup
	for i, c := range caps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, _ := ssh(c.addr, "timeout 4 tcpdump -nn -e -i "+c.dev+" "+f+" 2>/dev/null | grep -o 'echo [a-z]*' | sort | uniq -c")
			out[i] = fmt.Sprintf("%s %s: %s", c.addr, c.dev, strings.Join(strings.Fields(o), " "))
		}()
	}
	time.Sleep(time.Second)
	ssh(hSw3.vm, "ip netns exec h ping -c 3 -i 0.3 -W1 192.168.1.1")
	wg.Wait()
	return strings.Join(out, "\n")
}

// Jumbo frames between members (reference 5.2, stack MTU): srv1-a (sw1)
// and srv1-b (sw2) are hosts in VLAN 10 on different members, so their
// frames cross the stack tunnel. The largest frames the stack carries pass
// with the don't-fragment bit: plain, with a customer VLAN tag inside
// (QinQ) and as a host's own VXLAN; one byte more is refused by the host.
// Hosts use MTU 9000 where the stacking NICs allow it (in the lab they
// carry frames of 9014, so the stack carries data mtu 8956).
func TestStackJumbo(t *testing.T) {
	out := vcShow(t, sw1)
	for id := 1; id <= 2; id++ {
		if !vcRow(id, `\S+ +\d+ +voter +present`).MatchString(out) {
			t.Skipf("sw1 and sw2 are not in one stack:\n%s", out)
		}
	}
	limit := 9014
	for _, addr := range []string{sw1, sw2Addr} {
		o := mustSSH(t, addr, "swcli -c 'show virtual-chassis mtu'")
		m := regexp.MustCompile(`allow data mtu up to (\d+)`).FindStringSubmatch(o)
		if m == nil {
			t.Fatalf("%s show virtual-chassis mtu:\n%s", addr, o)
		}
		if n, _ := strconv.Atoi(m[1]); n < limit {
			limit = n
		}
	}
	host := limit - 14 // host MTU
	// The test host's NICs must also carry the customer tag (host + 4).
	if o := mustSSH(t, hSrv1.vm, "ip -n h -d link show ens19 2>/dev/null || ip -d link show ens19"); true {
		if m := regexp.MustCompile(`maxmtu (\d+)`).FindStringSubmatch(o); m != nil {
			if n, _ := strconv.Atoi(m[1]); n-4 < host {
				t.Logf("srv1's NIC carries at most MTU %d: hosts use MTU %d", n, n-4)
				host = n - 4
			}
		}
	}
	if host < 9000 {
		t.Logf("the lab's stacking NICs carry data mtu %d: hosts use MTU %d instead of 9000", limit, host)
	}
	srv1b := memberPort(t, sw2Addr, "ens23")
	mtu := fmt.Sprintf(" mtu %d\n", limit)
	// VLAN 20 is native (the plain, untagged host traffic); VLAN 10 is
	// tagged, the outer tag of the QinQ case (a native VLAN would strip it).
	trunk := func(port string) string {
		return "set interfaces " + port + " unit 0 family ethernet-switching interface-mode trunk\n" +
			"set interfaces " + port + " native-vlan-id v20\n" +
			"set interfaces " + port + " unit 0 family ethernet-switching vlan members [ v10 v20 ]\n" +
			"set interfaces " + port + mtu
	}
	configure(t, vlans+"set vlans v10 mtu "+strconv.Itoa(limit)+"\nset vlans v20 mtu "+strconv.Itoa(limit)+"\n"+trunk("1/ens23")+trunk(srv1b))
	if o := mustSSH(t, sw1, "swcli -c 'show virtual-chassis mtu'"); !strings.Contains(o, fmt.Sprintf("Largest data mtu in the stack:  %d", limit)) ||
		strings.Contains(o, "too small") {
		t.Errorf("show virtual-chassis mtu:\n%s", o)
	}
	// srv1: ens19 in namespace h (on sw1), ens20 in namespace j (on sw2);
	// untagged (VLAN 20), a customer tag 100 inside tagged VLAN 10 (QinQ),
	// and a VXLAN between the two.
	setup := func(ns, nic string, n int) string {
		return fmt.Sprintf(`ip netns add %[1]s 2>/dev/null; ip link set %[2]s netns %[1]s 2>/dev/null; ip -n %[1]s link set lo up
for l in $(ip -n %[1]s -o link show | grep -o '%[2]s\.[0-9.]*\|vx0' | sort -u -r); do ip -n %[1]s link del $l 2>/dev/null; done
ip -n %[1]s addr flush dev %[2]s; ip -n %[1]s link set %[2]s mtu %[7]d up; ip -n %[1]s addr add 192.168.1.%[4]d/24 dev %[2]s
ip -n %[1]s link add link %[2]s name %[2]s.10 type vlan id 10; ip -n %[1]s link set %[2]s.10 mtu %[7]d up
ip -n %[1]s link add link %[2]s.10 name %[2]s.10.100 type vlan id 100; ip -n %[1]s link set %[2]s.10.100 mtu %[3]d up
ip -n %[1]s addr add 192.168.100.%[4]d/24 dev %[2]s.10.100
ip -n %[1]s link add vx0 type vxlan id 42 dstport 4789 local 192.168.1.%[4]d remote 192.168.1.%[5]d dev %[2]s
ip -n %[1]s link set vx0 mtu %[6]d up; ip -n %[1]s addr add 192.168.42.%[4]d/24 dev vx0
ip -n %[1]s route add 192.168.1.0/24 dev %[2]s mtu %[3]d 2>/dev/null; ip -n %[1]s route replace 192.168.1.0/24 dev %[2]s mtu %[3]d
ip -n %[1]s neigh flush all`, ns, nic, host, n, 3-n, host-50, host+4)
	// The host NIC and its outer VLAN device carry the customer tag on top
	// (4 bytes more than the host MTU, else the host's own NIC drops the
	// double-tagged frame); the untagged route keeps the host MTU.
	}
	mustSSH(t, hSrv1.vm, setup("h", "ens19", 1)+"\n"+setup("j", "ens20", 2))
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("LAB_KEEP") != "" {
			return // leave the setup for inspection
		}
		ssh(hSrv1.vm, "ip -n j link set ens20 netns 1; ip netns del j; ip -n h link del vx0; ip -n h link del ens19.10")
		setupHost(t, hSrv1)
	})
	cases := []struct {
		name, dst string
		mtu       int
	}{
		{"plain", "192.168.1.2", host},
		{"QinQ (customer tag inside VLAN 10)", "192.168.100.2", host},
		{"host VXLAN", "192.168.42.2", host - 50},
	}
	for _, c := range cases {
		// Resolve the neighbour first (large packets queued behind ARP are
		// dropped by the host).
		ssh(hSrv1.vm, "ip netns exec h ping -c 3 -i 0.2 -W 1 "+c.dst)
		max := c.mtu - 28 // ICMP payload of a full-size IPv4 packet
		o, err := ssh(hSrv1.vm, fmt.Sprintf("ip netns exec h ping -M do -s %d -c 20 -i 0.05 -W 1 %s", max, c.dst))
		if err != nil || !strings.Contains(o, " 0% packet loss") {
			t.Errorf("%s: %d-byte packets (frames of %d) do not pass the stack:\n%s", c.name, c.mtu, c.mtu+14, o)
		}
		if o, err := ssh(hSrv1.vm, fmt.Sprintf("ip netns exec h ping -M do -s %d -c 1 -W 1 %s", max+1, c.dst)); err == nil {
			t.Errorf("%s: one byte more than the host MTU was sent:\n%s", c.name, o)
		}
	}
	// Nothing was fragmented on the way: the tunnel's outer packets carry
	// "don't fragment", and no member reassembled anything.
	for _, addr := range []string{sw1, sw2Addr} {
		if o := mustSSH(t, addr, "ip -d link show swvc1 2>/dev/null; ip -d link show swvc2 2>/dev/null; true"); !strings.Contains(o, "df set") {
			t.Errorf("%s: stack tunnel without DF:\n%s", addr, o)
		}
	}
}

// Configuration from different members (reference 3.1, 5.2): every session
// runs on the master, so users on sw2 and sw3 edit one shared candidate,
// are told about each other, cannot start a private session over
// uncommitted shared changes, and cannot commit while someone holds the
// exclusive lock.
func TestConfigAcrossMembers(t *testing.T) {
	out := vcShow(t, sw1)
	for id := 1; id <= 3; id++ {
		if !vcRow(id, `\S+ +\d+ +voter +present`).MatchString(out) {
			t.Skipf("sw1-sw3 are not in one stack:\n%s", out)
		}
	}
	masterSw1(t)
	// A session that stays open for a while: cmds, then a pause, then exit.
	hold := func(addr string, secs int, cmds ...string) <-chan string {
		done := make(chan string, 1)
		go func() {
			script := "(" + strings.Join(cmds, "; ") + "; sleep " + strconv.Itoa(secs) + "; echo rollback; echo yes; echo exit; echo exit) | swcli"
			o, _ := ssh(addr, script)
			done <- o
		}()
		time.Sleep(3 * time.Second) // the session is in configuration mode by now
		return done
	}
	run := func(addr string, cmds ...string) string {
		o, _ := ssh(addr, "(echo "+strings.Join(cmds, "; echo ")+") | swcli")
		return o
	}
	q := func(cmd string) string { return "'" + cmd + "'" }

	// 1. A edits on sw2 (uncommitted); B on sw3 sees it in the shared candidate.
	a := hold(sw2Addr, 12, "echo configure", "echo "+q("set system domain-name lab-a.example"))
	b := run("10.5.176.97", "configure", q("show | compare"), q("set system time-zone Europe/Berlin"), q("show | compare"), q("delete system time-zone"), "exit", "exit")
	for _, want := range []string{"users currently editing the configuration", "the configuration has been changed but not committed",
		"+   domain-name lab-a.example;", "+   time-zone Europe/Berlin;"} {
		if !strings.Contains(b, want) {
			t.Errorf("session on sw3 lacks %q:\n%s", want, b)
		}
	}
	// 2. Private and exclusive sessions are refused while the shared
	// candidate has changes.
	c := run("10.5.176.97", q("configure private"), q("configure exclusive"))
	if !strings.Contains(c, "'configure private' is not possible until those changes are committed or discarded") ||
		!strings.Contains(c, "'configure exclusive' would discard those changes") {
		t.Errorf("private/exclusive over shared changes:\n%s", c)
	}
	<-a
	if o := mustSSH(t, sw1, "swcli -c 'show system commit' | head -3"); o == "" {
		t.Error("no commit history")
	}

	// 3. A holds the exclusive lock on sw2: B on sw3 cannot open a session
	// or commit; the lock ends with A's session.
	a = hold(sw2Addr, 10, "echo "+q("configure exclusive"))
	c = run("10.5.176.97", q("configure"), q("commit"))
	if !strings.Contains(c, "configuration database locked by root (configure exclusive)") {
		t.Errorf("session on sw3 during the exclusive lock:\n%s", c)
	}
	<-a
	if c := run("10.5.176.97", "configure", "exit"); strings.Contains(c, "locked") {
		t.Errorf("the lock outlived the session:\n%s", c)
	}
}

// nicByMAC returns the Linux name of addr's NIC with the given MAC.
func nicByMAC(t *testing.T, addr, mac string) string {
	t.Helper()
	out := mustSSH(t, addr, "ip -o link show | grep -i '"+mac+"' | cut -d: -f2")
	n := strings.TrimSpace(strings.Split(strings.TrimSpace(out), "@")[0])
	if n == "" {
		t.Fatalf("%s has no NIC %s", addr, mac)
	}
	return n
}

// RSTP (reference 5.5): the stack is one bridge. The data links loop-13
// (sw1-sw3) and loop-23 (sw2-sw3) are loops of that bridge: one end of each
// becomes a backup port, nothing storms. When the RSTP owner (sw1) stops,
// sw2 continues from its copy without a port changing state.
func TestRSTP(t *testing.T) {
	const sw3Addr = "10.5.176.97"
	out := vcShow(t, sw1)
	for id := 1; id <= 3; id++ {
		if !vcRow(id, `\S+ +\d+ +voter +present`).MatchString(out) {
			t.Skipf("sw1-sw3 are not in one stack:\n%s", out)
		}
	}
	l13a := memberPort(t, sw1, nicByMAC(t, sw1, "bc:24:11:fe:f0:9f"))
	l13b := memberPort(t, sw3Addr, nicByMAC(t, sw3Addr, "bc:24:11:0b:5f:e7"))
	l23a := memberPort(t, sw2Addr, nicByMAC(t, sw2Addr, "bc:24:11:b4:ce:3a"))
	l23b := memberPort(t, sw3Addr, nicByMAC(t, sw3Addr, "bc:24:11:ac:2c:b3"))
	setupHost(t, hSw3)
	base := vlans + access(hSw3.sw1Port, "v10") + "set protocols rstp\n" +
		"set protocols rstp interface " + hSw3.sw1Port + " edge\n"
	// RSTP first; the loop ports join a bridge that already runs it.
	configure(t, base)
	loops := ""
	for _, p := range []string{l13a, l13b, l23a, l23b} {
		loops += access(p, "v10")
	}
	configure(t, base+loops)
	t.Cleanup(func() { configure(t, vlans) })

	stp := func(addr string) map[string]string {
		t.Helper()
		out := mustSSH(t, addr, "swcli -c 'show spanning-tree interface'")
		m := map[string]string{}
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) >= 4 && f[0] != "Interface" {
				m[f[0]] = f[2] + " " + f[3]
			}
		}
		return m
	}
	settled := func(addr string) map[string]string {
		t.Helper()
		var m map[string]string
		for i := 0; i < 50; i++ {
			m = stp(addr)
			n := 0
			for _, p := range []string{l13a, l13b, l23a, l23b} {
				if m[p] == "designated forwarding" || m[p] == "backup discarding" {
					n++
				}
			}
			if n == 4 {
				return m
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("RSTP did not settle:\n%s", mustSSH(t, addr, "swcli -c 'show spanning-tree interface'; swcli -c 'show spanning-tree bridge'"))
		return nil
	}
	m := settled(sw1)
	for _, pair := range [][2]string{{l13a, l13b}, {l23a, l23b}} {
		a, b := m[pair[0]], m[pair[1]]
		if !((a == "designated forwarding" && b == "backup discarding") || (b == "designated forwarding" && a == "backup discarding")) {
			t.Errorf("loop %s-%s: %q / %q", pair[0], pair[1], a, b)
		}
	}
	if m[hSw3.sw1Port] != "designated forwarding" {
		t.Errorf("edge port %s: %q", hSw3.sw1Port, m[hSw3.sw1Port])
	}
	// Every member shows the same (the owner answers).
	if m3 := stp(sw3Addr); !maps.Equal(m, m3) {
		t.Errorf("sw3 shows another spanning tree:\n%v\n%v", m, m3)
	}
	// Kernel states match on the members that have the ports.
	kstate := func(addr, port string) string {
		t.Helper()
		linux := strings.TrimSpace(mustSSH(t, addr, fmt.Sprintf("swcli -c 'show chassis hardware local' | awk '$1==\"%s\"{print $2}'", port)))
		return strings.TrimSpace(mustSSH(t, addr, "cat /sys/class/net/"+linux+"/brport/state"))
	}
	for _, c := range []struct{ addr, port string }{{sw1, l13a}, {sw3Addr, l13b}, {sw2Addr, l23a}, {sw3Addr, l23b}} {
		want := "3"
		if m[c.port] == "backup discarding" {
			want = "4"
		}
		if got := kstate(c.addr, c.port); got != want {
			t.Errorf("%s kernel state %s, want %s", c.port, got, want)
		}
	}
	// No storm: a broadcast ping gets a sane number of frames through.
	before := strings.TrimSpace(mustSSH(t, sw1, "cat /sys/class/net/swbr0/statistics/rx_packets"))
	time.Sleep(2 * time.Second)
	after := strings.TrimSpace(mustSSH(t, sw1, "cat /sys/class/net/swbr0/statistics/rx_packets"))
	b0, _ := strconv.Atoi(before)
	b1, _ := strconv.Atoi(after)
	if b1-b0 > 2000 {
		t.Errorf("the bridge received %d frames in 2 s: a loop?", b1-b0)
	}

	// The owner (sw1) stops: sw2 takes over from its copy; no port changes
	// state, the loops stay broken.
	if o := mustSSH(t, sw3Addr, "swcli -c 'show spanning-tree bridge'"); !strings.Contains(o, "RSTP owner         member 1") {
		t.Errorf("owner:\n%s", o)
	}
	mustSSH(t, sw1, "systemctl kill -s KILL switchd")
	time.Sleep(3 * time.Second)
	m2 := settled(sw3Addr)
	for _, p := range []string{l13b, l23a, l23b} {
		if m2[p] != m[p] {
			t.Errorf("%s changed after the owner stopped: %q -> %q", p, m[p], m2[p])
		}
	}
	if o := mustSSH(t, sw3Addr, "swcli -c 'show spanning-tree bridge'"); !strings.Contains(o, "RSTP owner         member 2") {
		t.Errorf("owner after sw1 stopped:\n%s", o)
	}
	mustSSH(t, sw1, "systemctl start switchd")
	for i := 0; ; i++ {
		if o, _ := ssh(sw1, "swcli -c 'show spanning-tree bridge'"); strings.Contains(o, "RSTP owner         member 1") {
			break
		}
		if i == 60 {
			t.Fatal("sw1 did not take the spanning tree back")
		}
		time.Sleep(500 * time.Millisecond)
	}
	m3 := settled(sw1)
	for _, p := range []string{l13a, l13b, l23a, l23b} {
		if m3[p] != m[p] {
			t.Errorf("%s after sw1 returned: %q -> %q", p, m[p], m3[p])
		}
	}
}
