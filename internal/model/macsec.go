package model

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/internal/schema"
)

// MACsec is security macsec and virtual-chassis macsec (reference 5.15,
// 5.2).
type MACsec struct {
	// StackMode is virtual-chassis macsec mode (auto, on, off); StackPorts
	// the per-port modes by interface name.
	StackMode  string
	StackPorts map[string]string
	CAs        map[string]*MACsecCA
	// Ports: the secured ports and their CA.
	Ports map[string]string
}

// MACsecCA is a connectivity association.
type MACsecCA struct {
	Name              string
	Cipher            string // gcm-aes-128, gcm-aes-256
	CKN, CAK          string // hex
	KeyServerPriority int
	ReplayWindow      int
}

// MACsecOverhead is what MACsec adds to a frame: the SecTAG with the SCI
// (16) and the ICV (16).
const MACsecOverhead = 32

// StackMACsecMode is the MACsec mode of a stacking port (its interface
// name, e.g. 2/0/1): its own setting, else the stack-wide one (reference
// 5.2; default auto).
func (c *Config) StackMACsecMode(port string) string {
	if c == nil {
		return "auto"
	}
	if m := c.MACsec.StackPorts[port]; m != "" {
		return m
	}
	if c.MACsec.StackMode != "" {
		return c.MACsec.StackMode
	}
	return "auto"
}

// StackPortOverhead is what a frame between members needs on a stacking
// port (reference 5.2): the tunnel and tags, and MACsec where the port may
// encrypt (on, or auto with an offloading NIC).
func (c *Config) StackPortOverhead(port string, offload bool) int {
	switch c.StackMACsecMode(port) {
	case "on":
		return StackOverhead + MACsecOverhead
	case "auto":
		if offload {
			return StackOverhead + MACsecOverhead
		}
	}
	return StackOverhead
}

// StackLinkMACsec decides whether a stacking link between local and peer
// (interface names) is encrypted, from both ends' modes and NIC offload
// (reference 5.2). Both members decide the same way. why explains a plain
// link ("" when encrypted).
func (c *Config) StackLinkMACsec(local, peer string, localOffload, peerOffload bool) (encrypt bool, why string) {
	lm, pm := c.StackMACsecMode(local), c.StackMACsecMode(peer)
	switch {
	case lm == "off" && pm == "off":
		return false, "off"
	case lm == "off":
		return false, "off on " + local
	case pm == "off":
		return false, "off on " + peer
	case lm == "on" || pm == "on":
		return true, ""
	case localOffload && peerOffload:
		return true, ""
	case !localOffload && !peerOffload:
		return false, "auto: neither NIC can offload"
	case !localOffload:
		return false, "auto: " + local + " cannot offload"
	}
	return false, "auto: " + peer + " cannot offload"
}

// Bits256: the cipher suite has a 256-bit key.
func (ca *MACsecCA) Bits256() bool { return strings.HasSuffix(ca.Cipher, "-256") }

// XPN: the cipher suite has 64-bit packet numbers.
func (ca *MACsecCA) XPN() bool { return strings.Contains(ca.Cipher, "-xpn-") }

func (b *builder) buildMACsec() {
	c := b.cfg
	r := b.root
	vm := r.Get("virtual-chassis", "macsec")
	c.MACsec = MACsec{StackMode: orDefault(vm.Leaf("mode"), "auto"), StackPorts: map[string]string{},
		CAs: map[string]*MACsecCA{}, Ports: map[string]string{}}
	for _, e := range vm.Entries("interface") {
		c.MACsec.StackPorts[e.Key] = orDefault(e.Leaf("mode"), "auto")
	}
	m := r.Get("security", "macsec")
	for _, e := range m.Entries("connectivity-association") {
		ca := &MACsecCA{Name: e.Key, Cipher: orDefault(e.Leaf("cipher-suite"), "gcm-aes-128"),
			CKN: strings.ToLower(e.Leaf("pre-shared-key", "ckn")), CAK: strings.ToLower(e.Leaf("pre-shared-key", "cak")),
			KeyServerPriority: atoi(e.Leaf("mka", "key-server-priority"), 16),
			ReplayWindow:      atoi(e.Leaf("replay-protect", "replay-window-size"), 0)}
		c.MACsec.CAs[e.Key] = ca
		at := "security macsec connectivity-association " + e.Key
		switch {
		case ca.CKN == "" || ca.CAK == "":
			b.errorf(at+" pre-shared-key", "ckn and cak are required")
		case ca.Bits256() && len(ca.CAK) != 64:
			b.errorf(at+" pre-shared-key cak", "%s needs a 256-bit cak (64 hex digits), this one has %d", ca.Cipher, len(ca.CAK))
		case ca.Bits256():
			b.errorf(at+" cipher-suite", "%s: the MKA of this release (wpa_supplicant 2.10) offers gcm-aes-128 only", ca.Cipher)
		case !ca.Bits256() && len(ca.CAK) != 32:
			b.errorf(at+" pre-shared-key cak", "%s needs a 128-bit cak (32 hex digits), this one has %d", ca.Cipher, len(ca.CAK))
		}
	}
	for _, e := range m.Entries("interfaces") {
		c.MACsec.Ports[e.Key] = e.Leaf("connectivity-association")
	}
}

// validateMACsec checks the secured ports (reference 5.15).
func (b *builder) validateMACsec() {
	c := b.cfg
	for _, port := range sortedKeys(c.MACsec.StackPorts) {
		at := "virtual-chassis macsec interface " + port
		pp, ok := schema.ParsePhysical(port)
		if !ok {
			b.errorf(at, "%s is not a physical port", port)
			continue
		}
		if ports, known := b.portsOf(pp.Member); known {
			if info, present := ports[port]; present && !info.StackPort {
				b.warnf(at, "%s is not a stacking port (the setting applies once it is designated: request virtual-chassis vc-port set)", port)
			}
		}
	}
	// A bundle's ports are all secured or none (reference 5.15): its
	// traffic must not leave both protected and not.
	secured, plain := map[string][]string{}, map[string][]string{}
	for _, name := range sortedKeys(c.Interfaces) {
		if i := c.Interfaces[name]; i.Parent != "" {
			if c.MACsec.Ports[name] != "" {
				secured[i.Parent] = append(secured[i.Parent], name)
			} else {
				plain[i.Parent] = append(plain[i.Parent], name)
			}
		}
	}
	for _, ae := range sortedKeys(secured) {
		if len(plain[ae]) > 0 {
			b.errorf("security macsec interfaces "+secured[ae][0], "%s: either every port of the bundle is secured or none (%s secured, %s not)",
				ae, strings.Join(secured[ae], ", "), strings.Join(plain[ae], ", "))
		}
	}
	for _, port := range sortedKeys(c.MACsec.Ports) {
		ca := c.MACsec.Ports[port]
		at := "security macsec interfaces " + port
		if _, ok := schema.ParsePhysical(port); !ok {
			b.errorf(at, "%s is not a physical port", port)
			continue
		}
		if ca == "" {
			b.errorf(at, "connectivity-association is required")
		} else if c.MACsec.CAs[ca] == nil {
			b.errorf(at+" connectivity-association", "connectivity-association %s is not configured", ca)
		}
		i := c.Interfaces[port]
		switch {
		case i == nil:
			b.errorf(at, "%s is not configured under interfaces", port)
		case i.Management:
			b.errorf(at, "%s is a management port", port)
		}
		pp, _ := schema.ParsePhysical(port)
		if info, present, _ := b.port(pp.Member, port); present && info.StackPort {
			b.errorf(at, "%s is a stacking port: stacking links have their own MACsec (virtual-chassis macsec)", port)
		}
	}
}

// MACsecPort is the CA of a secured port (nil: none).
func (c *Config) MACsecPort(port string) *MACsecCA {
	if ca, ok := c.MACsec.Ports[port]; ok {
		return c.MACsec.CAs[ca]
	}
	return nil
}

func (ca *MACsecCA) String() string {
	return fmt.Sprintf("%s (%s, ckn %s, priority %s)", ca.Name, ca.Cipher, ca.CKN, strconv.Itoa(ca.KeyServerPriority))
}
