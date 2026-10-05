// Package macsec drives the kernel's MACsec devices (IEEE 802.1AE) with
// static secure associations: devices, transmit and receive SAs, the
// encoding SA, and a filter that keeps unprotected frames off a secured
// port. Keys never appear on a command line: the commands go to "ip
// -batch -" on its standard input.
package macsec

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Cipher suites.
const (
	GCMAES128    = "gcm-aes-128"
	GCMAES256    = "gcm-aes-256"
	GCMAESXPN128 = "gcm-aes-xpn-128"
	GCMAESXPN256 = "gcm-aes-xpn-256"
)

// XPN: the suite has 64-bit packet numbers (and needs salt and SSCI).
func XPN(cipher string) bool { return strings.Contains(cipher, "-xpn-") }

// KeyLen is the key length of a suite in bytes.
func KeyLen(cipher string) int {
	if strings.HasSuffix(cipher, "-256") {
		return 32
	}
	return 16
}

// SAK is one secure association key with what XPN needs.
type SAK struct {
	AN   uint8  `json:"an"`   // association number 0..3
	ID   string `json:"id"`   // key identifier (32 hex digits)
	Key  string `json:"key"`  // hex
	Salt string `json:"salt"` // 12 bytes hex (XPN)
	SSCI uint32 `json:"ssci"` // short SCI of the transmitter (XPN)
}

// NewSAK makes a random key for association number an.
func NewSAK(cipher string, an uint8, ssci uint32) (SAK, error) {
	key := make([]byte, KeyLen(cipher))
	id := make([]byte, 16)
	salt := make([]byte, 12)
	for _, b := range [][]byte{key, id, salt} {
		if _, err := rand.Read(b); err != nil {
			return SAK{}, err
		}
	}
	return SAK{AN: an, ID: hex.EncodeToString(id), Key: hex.EncodeToString(key), Salt: hex.EncodeToString(salt), SSCI: ssci}, nil
}

// Valid checks a key received from elsewhere.
func (k SAK) Valid(cipher string) error {
	key, err := hex.DecodeString(k.Key)
	if err != nil || len(key) != KeyLen(cipher) {
		return fmt.Errorf("bad key")
	}
	if id, err := hex.DecodeString(k.ID); err != nil || len(id) != 16 {
		return fmt.Errorf("bad key id")
	}
	if XPN(cipher) {
		if salt, err := hex.DecodeString(k.Salt); err != nil || len(salt) != 12 {
			return fmt.Errorf("bad salt")
		}
	}
	if k.AN > 3 {
		return fmt.Errorf("bad association number %d", k.AN)
	}
	return nil
}

// sa is the SA part of an "ip macsec add" command.
func (k SAK) sa(cipher string) string {
	s := fmt.Sprintf("sa %d pn 1 on", k.AN)
	if XPN(cipher) {
		s = fmt.Sprintf("sa %d xpn 1 on salt %s ssci %d", k.AN, k.Salt, k.SSCI)
	}
	return s + " key " + k.ID + " " + k.Key
}

// Kernel runs the commands (a fake in tests).
type Kernel interface {
	// Batch runs "ip -batch -" with the lines.
	Batch(lines []string) error
	// Run runs a tool (tc).
	Run(name string, args ...string) error
}

// Linux is the real kernel.
type Linux struct{}

func (Linux) Batch(lines []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ip", "-batch", "-")
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		// The output names the line, never the key (ip does not echo it).
		return fmt.Errorf("ip -batch: %v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Output runs a tool and returns its output.
func (Linux) Output(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

func (Linux) Run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Device is one MACsec device on a port.
type Device struct {
	Name, Parent string
	Cipher       string
	// Offload is "mac" or "phy" when the NIC encrypts ("": software).
	Offload string
	// ReplayWindow (0: frames in order).
	ReplayWindow int
}

// Create makes the device (send_sci on, encrypt on, validate strict).
func Create(k Kernel, d Device) error {
	line := fmt.Sprintf("link add link %s name %s type macsec port 1 cipher %s icvlen 16 encrypt on send_sci on validate strict",
		d.Parent, d.Name, d.Cipher)
	if d.ReplayWindow > 0 {
		line += fmt.Sprintf(" replay on window %d", d.ReplayWindow)
	}
	if d.Offload != "" {
		line += " offload " + d.Offload
	}
	return k.Batch([]string{line, "link set " + d.Name + " up"})
}

// Delete removes the device (its SAs go with it).
func Delete(k Kernel, name string) error { return k.Batch([]string{"link del " + name}) }

// AddTx installs a transmit SA (not used until SetEncoding).
func AddTx(k Kernel, dev, cipher string, sak SAK) error {
	return k.Batch([]string{"macsec add " + dev + " tx " + sak.sa(cipher)})
}

// SetEncoding makes an SA the one frames are sent with.
func SetEncoding(k Kernel, dev string, an uint8) error {
	return k.Batch([]string{fmt.Sprintf("link set %s type macsec encodingsa %d", dev, an)})
}

// DelTx removes a transmit SA.
func DelTx(k Kernel, dev string, an uint8) error {
	return k.Batch([]string{fmt.Sprintf("macsec del %s tx sa %d", dev, an)})
}

// AddRx installs the receive SC of the peer (by its MAC, port 1) if
// needed and a receive SA in it.
func AddRx(k Kernel, dev, cipher, peerMAC string, sak SAK, newSC bool) error {
	var lines []string
	if newSC {
		lines = append(lines, fmt.Sprintf("macsec add %s rx port 1 address %s on", dev, peerMAC))
	}
	lines = append(lines, fmt.Sprintf("macsec add %s rx port 1 address %s %s", dev, peerMAC, sak.sa(cipher)))
	return k.Batch(lines)
}

// DelRx removes a receive SA.
func DelRx(k Kernel, dev, peerMAC string, an uint8) error {
	return k.Batch([]string{fmt.Sprintf("macsec del %s rx port 1 address %s sa %d", dev, peerMAC, an)})
}

// FilterPlain keeps unprotected frames off a secured port: only MACsec
// (0x88e5) and the stacking protocol (0x88b5, TLS itself) pass the port's
// ingress; on false the filter goes.
func FilterPlain(k Kernel, port string, on bool) error {
	if !on {
		// Only our two filters at their priorities: anything else on the
		// port (none on stacking ports) stays.
		_ = k.Run("tc", "filter", "del", "dev", port, "ingress", "pref", "49151")
		_ = k.Run("tc", "filter", "del", "dev", port, "ingress", "pref", "49152")
		_ = k.Run("tc", "filter", "del", "dev", port, "ingress", "pref", "49153")
		return nil
	}
	_ = k.Run("tc", "qdisc", "add", "dev", port, "clsact") // exists already: fine
	for _, args := range [][]string{
		{"filter", "replace", "dev", port, "ingress", "pref", "49151", "protocol", "0x88e5", "matchall", "action", "pass"},
		{"filter", "replace", "dev", port, "ingress", "pref", "49152", "protocol", "0x88b5", "matchall", "action", "pass"},
		{"filter", "replace", "dev", port, "ingress", "pref", "49153", "protocol", "all", "matchall", "action", "drop"},
	} {
		if err := k.Run("tc", args...); err != nil {
			return err
		}
	}
	return nil
}
