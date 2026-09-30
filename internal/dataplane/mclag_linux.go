//go:build linux

package dataplane

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// mclagTable holds the MC-LAG split horizon (reference 5.6): nothing that
// arrives over the peer-link leaves an MC-LAG bundle whose other leg (on
// the peer) is up. (The peer-link learns no addresses and MAC
// synchronisation points dual-homed devices at the local leg, so unicast
// for them crosses the peer-link only while the sender's leg is down, and
// then the filter is lifted.)
const mclagTable = "switchd_mclag"

var (
	splitMu   sync.Mutex
	splitLast = "\x00" // rules installed last ("\x00": unknown, e.g. after a restart)
)

// SyncSplitHorizon installs the split horizon: nothing from peerLink goes
// out of bundles. An empty peerLink or no bundles removes it.
func SyncSplitHorizon(peerLink string, bundles []string) error {
	bundles = slices.Clone(bundles)
	slices.Sort(bundles)
	var b strings.Builder
	fmt.Fprintf(&b, "table bridge %s\ndelete table bridge %s\n", mclagTable, mclagTable)
	if peerLink != "" && len(bundles) > 0 {
		quoted := make([]string, len(bundles))
		for i, n := range bundles {
			quoted[i] = `"` + n + `"`
		}
		fmt.Fprintf(&b, `table bridge %s {
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "%s" oifname { %s } counter drop
	}
}
`, mclagTable, peerLink, strings.Join(quoted, ", "))
	}
	rules := b.String()
	splitMu.Lock()
	defer splitMu.Unlock()
	if rules == splitLast {
		return nil
	}
	nft, err := exec.LookPath("nft")
	if err != nil {
		return errors.New("nftables (the nft program) is required for MC-LAG; install the nftables package")
	}
	cmd := exec.Command(nft, "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(out.String()))
	}
	splitLast = rules
	return nil
}

// FlushLearned removes the MAC addresses the bridge learned on dev (not
// static or externally installed ones). MC-LAG flushes the peer-link when
// a leg changes: addresses behind a leg that went away were learned there
// and are found again by flooding.
func FlushLearned(dev string) (int, error) {
	l, err := netlink.LinkByName(dev)
	if err != nil {
		return 0, err
	}
	neighs, err := netlink.NeighList(l.Attrs().Index, unix.AF_BRIDGE)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range neighs {
		// Bridge entries name the bridge as master (dumps carry no
		// NTF_MASTER flag); learned ones are neither local, static nor
		// installed from outside.
		if e.MasterIndex == 0 || e.State&(unix.NUD_PERMANENT|unix.NUD_NOARP) != 0 || e.Flags&netlink.NTF_EXT_LEARNED != 0 {
			continue
		}
		e.Flags |= netlink.NTF_MASTER // delete from the bridge's table
		if err := netlink.NeighDel(&e); err == nil {
			n++
		}
	}
	return n, nil
}
