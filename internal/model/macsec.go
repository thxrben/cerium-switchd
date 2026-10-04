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
	// StackDisabled: virtual-chassis macsec disable (the stacking links
	// are encrypted by default).
	StackDisabled bool
	CAs           map[string]*MACsecCA
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

// StackOverheadOf is what a frame between members needs on a stacking
// link (reference 5.2): the tunnel and tags, and MACsec unless disabled.
func (c *Config) StackOverheadOf() int {
	if c == nil || !c.MACsec.StackDisabled {
		return StackOverhead + MACsecOverhead
	}
	return StackOverhead
}

// Bits256: the cipher suite has a 256-bit key.
func (ca *MACsecCA) Bits256() bool { return strings.HasSuffix(ca.Cipher, "-256") }

// XPN: the cipher suite has 64-bit packet numbers.
func (ca *MACsecCA) XPN() bool { return strings.Contains(ca.Cipher, "-xpn-") }

func (b *builder) buildMACsec() {
	c := b.cfg
	r := b.root
	c.MACsec = MACsec{StackDisabled: r.Get("virtual-chassis", "macsec").Has("disable"), CAs: map[string]*MACsecCA{},
		Ports: map[string]string{}}
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
	if len(c.MACsec.Ports) > 0 {
		b.warnf("security macsec interfaces", "MACsec on switch and routed ports is not implemented yet: the ports carry their traffic unencrypted (the stacking links are encrypted)")
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
		case i.Parent != "":
			b.errorf(at, "%s is a member of %s: MACsec on bundle members is not supported yet", port, i.Parent)
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
