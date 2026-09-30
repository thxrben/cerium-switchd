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
// arrives over the peer's stack tunnel leaves an MC-LAG bundle whose other
// leg (on the peer) is up. (The peer's tunnel learns no addresses and MAC
// synchronisation points dual-homed devices at the local leg, so unicast
// for them crosses to the peer only while the sender's leg is down, and
// then the filter is lifted.) On the secondary, broadcast and multicast
// from third members' tunnels are left to the primary on bundles whose
// primary leg is up.
const mclagTable = "switchd_mclag"

var (
	splitMu   sync.Mutex
	splitLast = "\x00" // rules installed last ("\x00": unknown, e.g. after a restart)
)

// SplitHorizon is the filter of one member.
type SplitHorizon struct {
	Peer    string   // the peer's stack tunnel
	Bundles []string // bundles whose peer leg is up
	Thirds  []string // stack tunnels to members outside the domain
	DF      []string // bundles whose broadcast/multicast from Thirds the primary delivers
}

func quoteSet(names []string) string {
	names = slices.Clone(names)
	slices.Sort(names)
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = `"` + n + `"`
	}
	return "{ " + strings.Join(q, ", ") + " }"
}

// SyncSplitHorizon installs the split horizon; the zero value removes it.
func SyncSplitHorizon(sh SplitHorizon) error {
	var rules []string
	if sh.Peer != "" && len(sh.Bundles) > 0 {
		rules = append(rules, fmt.Sprintf("iifname %q oifname %s counter drop", sh.Peer, quoteSet(sh.Bundles)))
	}
	if len(sh.Thirds) > 0 && len(sh.DF) > 0 {
		rules = append(rules, fmt.Sprintf("iifname %s oifname %s meta pkttype { broadcast, multicast } counter drop", quoteSet(sh.Thirds), quoteSet(sh.DF)))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "table bridge %s\ndelete table bridge %s\n", mclagTable, mclagTable)
	if len(rules) > 0 {
		fmt.Fprintf(&b, "table bridge %s {\n\tchain forward {\n\t\ttype filter hook forward priority filter; policy accept;\n", mclagTable)
		for _, r := range rules {
			fmt.Fprintf(&b, "\t\t%s\n", r)
		}
		b.WriteString("\t}\n}\n")
	}
	text := b.String()
	splitMu.Lock()
	defer splitMu.Unlock()
	if text == splitLast {
		return nil
	}
	nft, err := exec.LookPath("nft")
	if err != nil {
		return errors.New("nftables (the nft program) is required for MC-LAG; install the nftables package")
	}
	cmd := exec.Command(nft, "-f", "-")
	cmd.Stdin = strings.NewReader(text)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(out.String()))
	}
	splitLast = text
	return nil
}

// FlushLearned removes the MAC addresses the bridge learned on dev (not
// static or externally installed ones). MC-LAG flushes the peer's tunnel when
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
