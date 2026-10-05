//go:build linux

package macsec

import (
	"fmt"
	"sync"

	"github.com/thxrben/cerium-switchd/lib/sys/nlx"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

var (
	famMu sync.Mutex
	fam   *netlink.GenlFamily
)

// family looks the family up once it exists (a failure is not kept: the
// macsec module may be loaded later).
func family() (*netlink.GenlFamily, error) {
	famMu.Lock()
	defer famMu.Unlock()
	if fam != nil {
		return fam, nil
	}
	f, err := nlx.GenlFamilyGet(genlName)
	if err != nil {
		return nil, fmt.Errorf("macsec driver (generic netlink family %q): %w", genlName, err)
	}
	fam = f
	return fam, nil
}

// Status reads every MACsec device of the kernel by interface index (one
// MACSEC_CMD_GET_TXSC dump).
func (Linux) Status() (map[int]DevStatus, error) {
	f, err := family()
	if err != nil {
		return nil, err
	}
	req := nl.NewNetlinkRequest(int(f.ID), unix.NLM_F_DUMP)
	req.AddData(&nl.Genlmsg{Command: cmdGetTxSC, Version: genlVersion})
	msgs, err := nlx.Execute(req, unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, fmt.Errorf("macsec status: %w", err)
	}
	out := make(map[int]DevStatus, len(msgs))
	for _, m := range msgs {
		if len(m) < nl.SizeofGenlmsg {
			continue
		}
		d, err := parseDevice(m[nl.SizeofGenlmsg:])
		if err != nil {
			return nil, fmt.Errorf("macsec status: %w", err)
		}
		out[d.Ifindex] = d
	}
	return out, nil
}
