//go:build linux

package netdev

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"github.com/thxrben/cerium-switchd/pkg/nlx"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// Bridge port STP states (IFLA_BRPORT_STATE).
const (
	PortDisabled   = 0
	PortListening  = 1
	PortLearning   = 2
	PortForwarding = 3
	PortBlocking   = 4
)

// STPHelper is the program the kernel runs when STP is switched on for a
// bridge: exit status 0 hands the bridge to user space (switchd runs RSTP,
// the kernel consumes BPDUs and leaves port states alone), anything else
// makes the kernel run its own STP.
const STPHelper = "/sbin/bridge-stp" // the kernel's fixed path

// stpHelperMarker marks the helper as switchd's (unchanged: helpers written
// by earlier versions are recognised).
func stpHelperMarker(bridge string) string { return "# switchd: RSTP runs in switchd for " + bridge }

func stpHelperText(bridge string) string {
	return "#!/bin/sh\n" + stpHelperMarker(bridge) + "\n" +
		"[ \"$1\" = \"" + bridge + "\" ] && exit 0\nexit 1\n"
}

// EnsureSTPHelper installs the helper that hands bridge to user space.
func EnsureSTPHelper(bridge string) error {
	cur, err := hwio.ReadFile(STPHelper)
	switch {
	case err == nil && string(cur) == stpHelperText(bridge):
		return nil
	case err == nil && !bytes.Contains(cur, []byte(stpHelperMarker(bridge))):
		return fmt.Errorf("%s belongs to another program; it must exit 0 for %s", STPHelper, bridge)
	}
	tmp := STPHelper + ".tmp"
	if err := hwio.WriteFile(tmp, []byte(stpHelperText(bridge)), 0o755); err != nil {
		return err
	}
	return hwio.Rename(tmp, STPHelper)
}

// BridgeSTP reports whether user-space STP is on for the bridge.
func BridgeSTP(bridge string) (bool, error) {
	b, err := hwio.ReadFile(filepath.Join("/sys/class/net", bridge, "bridge", "stp_state"))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(b)) == "2", nil
}

// SetBridgeSTP switches user-space STP on or off. Off, every port forwards
// (and BPDUs are flooded like other frames).
func SetBridgeSTP(bridge string, on bool) error {
	v := "0"
	if on {
		if err := EnsureSTPHelper(bridge); err != nil {
			return err
		}
		v = "1"
	}
	p := filepath.Join("/sys/class/net", bridge, "bridge", "stp_state")
	cur, err := hwio.ReadFile(p)
	if err != nil {
		return err
	}
	c := strings.TrimSpace(string(cur))
	if (on && c == "2") || (!on && c == "0") {
		return nil
	}
	if on && c == "1" {
		return fmt.Errorf("the kernel runs its own STP on %s (is %s missing?)", bridge, STPHelper)
	}
	if err := hwio.WriteFile(p, []byte(v), 0o644); err != nil {
		return err
	}
	if on {
		if st, _ := BridgeSTP(bridge); !st {
			return fmt.Errorf("user-space STP did not start on %s (%s)", bridge, STPHelper)
		}
	}
	return nil
}

// BridgePorts lists the bridge's ports.
func BridgePorts(bridge string) ([]string, error) {
	ents, err := hwio.ReadDir(filepath.Join("/sys/class/net", bridge, "brif"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out, nil
}

// PortSTPState reads a bridge port's state.
func PortSTPState(dev string) (int, error) {
	b, err := hwio.ReadFile(filepath.Join("/sys/class/net", dev, "brport", "state"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// SetPortSTPState sets a bridge port's state (user-space STP only).
func SetPortSTPState(dev string, state int) error {
	l, err := nlx.LinkByName(dev)
	if err != nil {
		return err
	}
	req := nl.NewNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_BRIDGE)
	msg.Index = int32(l.Attrs().Index)
	req.AddData(msg)
	pi := nl.NewRtAttr(unix.IFLA_PROTINFO|unix.NLA_F_NESTED, nil)
	pi.AddRtAttr(nl.IFLA_BRPORT_STATE, []byte{byte(state)})
	req.AddData(pi)
	_, err = nlx.Execute(req, unix.NETLINK_ROUTE, 0)
	return err
}
