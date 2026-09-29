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
	_, err := ssh(from.vm, fmt.Sprintf("ip netns exec h ping -c2 -i0.2 -W1 192.168.%d.%d", net, to.n))
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
