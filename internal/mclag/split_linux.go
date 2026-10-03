//go:build linux

package mclag

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/thxrben/cerium-switchd/pkg/sysexec"
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
	// Draining: bundles whose peer leg leaves for maintenance mode. The
	// peer sends its unicast for them through this member already, but its
	// leg still receives until the partner stops sending, so broadcast and
	// multicast from the peer (flooded frames the partner sent there, BPDUs
	// among them) must not go back to the partner on this leg.
	Draining []string
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
	if sh.Peer != "" && len(sh.Draining) > 0 {
		rules = append(rules, fmt.Sprintf("iifname %q oifname %s meta pkttype { broadcast, multicast } counter drop", sh.Peer, quoteSet(sh.Draining)))
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
	if _, err := sysexec.Command(nft, "-f", "-").WithStdin(strings.NewReader(text)).CombinedOutput(context.Background()); err != nil {
		return err
	}
	splitLast = text
	return nil
}
