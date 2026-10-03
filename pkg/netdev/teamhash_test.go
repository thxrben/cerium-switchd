package netdev

import (
	"encoding/binary"
	"testing"

	"golang.org/x/net/bpf"
)

func frame4(src, dst [4]byte, proto byte, sport, dport uint16, fragOff uint16) []byte {
	f := make([]byte, 14+20+8+20)
	copy(f[0:6], []byte{2, 0, 0, 0, 0, 2})
	copy(f[6:12], []byte{2, 0, 0, 0, 0, 1})
	binary.BigEndian.PutUint16(f[12:], 0x0800)
	ip := f[14:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[6:], fragOff)
	ip[9] = proto
	copy(ip[12:16], src[:])
	copy(ip[16:20], dst[:])
	binary.BigEndian.PutUint16(ip[20:], sport)
	binary.BigEndian.PutUint16(ip[22:], dport)
	return f
}

func frame6(last byte, proto byte, sport, dport uint16) []byte {
	f := make([]byte, 14+40+8+10)
	binary.BigEndian.PutUint16(f[12:], 0x86dd)
	ip := f[14:]
	ip[0] = 0x60
	ip[6] = proto
	ip[8], ip[23] = 0xfd, last // src fd00::last
	ip[24], ip[39] = 0xfd, 1   // dst fd00::1
	binary.BigEndian.PutUint16(ip[40:], sport)
	binary.BigEndian.PutUint16(ip[42:], dport)
	return f
}

func runHash(t *testing.T, policy string, f []byte) uint32 {
	t.Helper()
	prog, err := hashProgram(policy)
	if err != nil {
		t.Fatal(err)
	}
	vm, err := bpf.NewVM(prog)
	if err != nil {
		t.Fatalf("%s: %v", policy, err)
	}
	h, err := vm.Run(f)
	if err != nil {
		t.Fatal(err)
	}
	return uint32(h)
}

func TestTeamHash(t *testing.T) {
	a, b := [4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}
	// layer3+4: ports matter, the same flow hashes the same, both
	// directions hash the same.
	h1 := runHash(t, "layer3+4", frame4(a, b, ipProtoTCP, 40000, 80, 0))
	if h1 != runHash(t, "layer3+4", frame4(a, b, ipProtoTCP, 40000, 80, 0)) {
		t.Error("same flow, different hash")
	}
	if h1 != runHash(t, "layer3+4", frame4(b, a, ipProtoTCP, 80, 40000, 0)) {
		t.Error("reverse direction, different hash")
	}
	seen := map[uint32]bool{}
	for p := uint16(40000); p < 40064; p++ {
		seen[runHash(t, "layer3+4", frame4(a, b, ipProtoUDP, p, 53, 0))%4] = true
	}
	if len(seen) != 4 {
		t.Errorf("64 UDP flows use only %d of 4 ports", len(seen))
	}
	// A fragment (offset set) is hashed without ports, like its first part
	// would be... without ports: all fragments of a packet stay together.
	if runHash(t, "layer3+4", frame4(a, b, ipProtoUDP, 1, 2, 100)) != runHash(t, "layer3+4", frame4(a, b, ipProtoUDP, 3, 4, 100)) {
		t.Error("fragments hashed by (garbage) ports")
	}
	// IPv6 with ports.
	seen = map[uint32]bool{}
	for p := uint16(1000); p < 1064; p++ {
		seen[runHash(t, "layer3+4", frame6(5, ipProtoTCP, p, 443))%4] = true
	}
	if len(seen) != 4 {
		t.Errorf("64 IPv6 flows use only %d of 4 ports", len(seen))
	}
	// layer2+3 ignores ports, layer2 ignores IP.
	if runHash(t, "layer2+3", frame4(a, b, ipProtoTCP, 1, 2, 0)) != runHash(t, "layer2+3", frame4(a, b, ipProtoTCP, 3, 4, 0)) {
		t.Error("layer2+3 uses ports")
	}
	if runHash(t, "layer2", frame4(a, b, ipProtoTCP, 1, 2, 0)) != runHash(t, "layer2", frame4(a, [4]byte{9, 9, 9, 9}, ipProtoTCP, 1, 2, 0)) {
		t.Error("layer2 uses IP addresses")
	}
	// Non-IP: layer 2.
	arp := make([]byte, 60)
	copy(arp, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 2, 0, 0, 0, 0, 7, 0x08, 0x06})
	arp2 := append([]byte(nil), arp...)
	arp2[11] = 8
	if runHash(t, "layer3+4", arp) == runHash(t, "layer3+4", arp2) {
		t.Error("non-IP frames of different hosts hash the same")
	}
	// The kernel form is recognised again.
	for _, p := range []string{"layer2", "layer2+3", "layer3+4"} {
		b, err := hashProgramBytes(p)
		if err != nil || len(b)%8 != 0 || hashPolicyOf(b) != p {
			t.Errorf("%s: %d bytes, recognised as %q (%v)", p, len(b), hashPolicyOf(b), err)
		}
	}
}
