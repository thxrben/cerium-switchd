//go:build linux

package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/thxrben/cerium-switchd/lib/sys/sysexec"
)

// vlanMTUTable drops frames larger than their VLAN's mtu when they are
// received (reference 4.x, vlans <v> mtu): switched on (forward) or
// delivered to the switch (input). In the bridge family, meta length is the
// frame without its Ethernet header and VLAN tag, so the limit is the
// VLAN's frame size minus 14. Each VLAN has a named counter.
const vlanMTUTable = "switchd_vlanmtu"

var (
	vlanMTUMu   sync.Mutex
	vlanMTULast = "\x00"
)

// SyncVLANMTU installs the per-VLAN MTU filters (vid -> frame size; empty:
// none). The table is replaced in one transaction.
func (k *Netlink) SyncVLANMTU(mtus map[int]int) (bool, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "table bridge %s\ndelete table bridge %s\n", vlanMTUTable, vlanMTUTable)
	if len(mtus) > 0 {
		fmt.Fprintf(&b, "table bridge %s {\n", vlanMTUTable)
		vids := slices.Sorted(maps.Keys(mtus))
		for _, v := range vids {
			fmt.Fprintf(&b, "\tcounter vlan%d {}\n", v)
		}
		for _, hook := range []string{"forward", "input"} {
			fmt.Fprintf(&b, "\tchain %s {\n\t\ttype filter hook %s priority filter - 10; policy accept;\n", hook, hook)
			for _, v := range vids {
				// Tagged frames carry the VLAN; untagged ones (access ports,
				// native VLANs) belong to their ingress port's PVID.
				fmt.Fprintf(&b, "\t\tvlan id %d meta length > %d counter name \"vlan%d\" drop\n", v, mtus[v]-14, v)
				fmt.Fprintf(&b, "\t\tether type != 8021q meta ibrpvid %d meta length > %d counter name \"vlan%d\" drop\n", v, mtus[v]-14, v)
			}
			b.WriteString("\t}\n")
		}
		b.WriteString("}\n")
	}
	text := b.String()
	vlanMTUMu.Lock()
	defer vlanMTUMu.Unlock()
	if text == vlanMTULast {
		return false, nil
	}
	if err := nftRun(text); err != nil {
		return false, err
	}
	vlanMTULast = text
	return true, nil
}

func nftRun(text string) error {
	nft, err := exec.LookPath("nft")
	if err != nil {
		return errors.New("nftables (the nft program) is required; install the nftables package")
	}
	if _, err := sysexec.Command(nft, "-f", "-").WithStdin(strings.NewReader(text)).CombinedOutput(context.Background()); err != nil {
		return err
	}
	return nil
}

// VLANMTUDrops returns the frames dropped per VLAN for exceeding its mtu.
func VLANMTUDrops() (map[int]uint64, error) {
	out, err := sysexec.Output("nft", "-j", "list", "counters", "table", "bridge", vlanMTUTable)
	if err != nil {
		return map[int]uint64{}, nil // no table: no VLAN has an mtu
	}
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	res := map[int]uint64{}
	for _, e := range doc.Nftables {
		raw, ok := e["counter"]
		if !ok {
			continue
		}
		var c struct {
			Name    string `json:"name"`
			Packets uint64 `json:"packets"`
		}
		if json.Unmarshal(raw, &c) == nil && strings.HasPrefix(c.Name, "vlan") {
			if v, err := strconv.Atoi(strings.TrimPrefix(c.Name, "vlan")); err == nil {
				res[v] = c.Packets
			}
		}
	}
	return res, nil
}
