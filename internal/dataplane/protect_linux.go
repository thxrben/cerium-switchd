//go:build linux

package dataplane

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/thxrben/cerium-switchd/pkg/sysexec"
)

// protectTable is switchd's nftables table protecting the switch's own
// addresses on data L3 interfaces (reference 1.5): only ping, neighbour
// discovery (ICMPv6) and replies to connections the switch opened reach
// the switch there. ARP is not affected (it is not IP).
const protectTable = "switchd_protect"

// syncProtect installs the protection for the given interfaces (none:
// removes it). The table is replaced atomically on every run, which also
// repairs it if something else flushed the ruleset.
func (k *Netlink) syncProtect(ifs []string, accept []string) (bool, error) {
	slices.Sort(ifs)
	var b strings.Builder
	// Protocols by number (1 ICMP, 58 ICMPv6): nft resolves names such as
	// ipv6-icmp through /etc/protocols, which a minimal system may lack.
	// Adding and then deleting makes the delete succeed whether or not the
	// table exists; the whole file is one transaction.
	fmt.Fprintf(&b, "table inet %s\ndelete table inet %s\n", protectTable, protectTable)
	if len(ifs) > 0 {
		quoted := make([]string, len(ifs))
		for i, n := range ifs {
			quoted[i] = `"` + n + `"`
		}
		fmt.Fprintf(&b, `table inet %s {
	chain input {
		type filter hook input priority filter - 10; policy accept;
		iifname != { %s } accept
		ct state established,related accept
		meta l4proto { 1, 58 } accept
%s		counter drop
	}
}
`, protectTable, strings.Join(quoted, ", "), acceptRules(accept))
	}
	rules := b.String()
	k.protMu.Lock()
	defer k.protMu.Unlock()
	if len(ifs) == 0 && k.protected == "" && k.protInit {
		return false, nil // nothing installed
	}
	k.protInit = true // the first run also removes a table left from before a restart
	nft, err := exec.LookPath("nft")
	if err != nil {
		if len(ifs) == 0 {
			return false, nil
		}
		return false, errors.New("nftables (the nft program) is required to protect routed interfaces; install the nftables package")
	}
	if _, err := sysexec.Command(nft, "-f", "-").WithStdin(strings.NewReader(rules)).CombinedOutput(context.Background()); err != nil {
		return false, err
	}
	changed := k.protected != rules
	k.protected = rules
	if len(ifs) == 0 {
		k.protected = ""
	}
	return changed, nil
}

func acceptRules(rules []string) string {
	var b strings.Builder
	for _, r := range rules {
		fmt.Fprintf(&b, "\t\t%s accept\n", r)
	}
	return b.String()
}
