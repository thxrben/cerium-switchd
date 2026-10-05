//go:build linux

package netdev

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/thxrben/cerium-switchd/lib/sys/nlx"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// LACP bundles are team devices (loadbalance mode): unlike a bond, a team
// lets userspace decide per port whether it carries traffic ("enabled")
// without touching the port's link, and a disabled port still delivers
// LACPDUs to the LACP socket. The link state the team reports to the
// bridge follows "user_linkup" of its ports.

// Team generic netlink API (include/uapi/linux/if_team.h).
const (
	teamCmdOptionsSet = 1
	teamCmdOptionsGet = 2

	teamAttrTeamIfindex = 1
	teamAttrListOption  = 2
	teamAttrItemOption  = 1

	teamAttrOptionName        = 1
	teamAttrOptionType        = 3
	teamAttrOptionData        = 4
	teamAttrOptionPortIfindex = 6

	nlaU32    = 3
	nlaString = 5
	nlaFlag   = 6
	nlaBinary = 11
)

var (
	teamFamOnce sync.Once
	teamFam     *netlink.GenlFamily
	teamFamErr  error
)

func teamFamily() (*netlink.GenlFamily, error) {
	teamFamOnce.Do(func() {
		teamFam, teamFamErr = nlx.GenlFamilyGet("team")
		if teamFamErr != nil {
			teamFamErr = fmt.Errorf("team driver (generic netlink family \"team\"): %w", teamFamErr)
		}
	})
	return teamFam, teamFamErr
}

// teamOption is one option to set.
type teamOption struct {
	name string
	port int    // per-port option: the port's ifindex
	typ  uint8  // nlaU32, nlaString, nlaFlag, nlaBinary
	data []byte // nil with nlaFlag: false
}

// teamSetOptions sets options in order, one request each (with several
// options in one request, only the first took effect on Linux 6.12).
func teamSetOptions(team int, opts ...teamOption) error {
	for _, o := range opts {
		if err := teamSetOption(team, o); err != nil {
			return fmt.Errorf("team option %s: %w", o.name, err)
		}
	}
	return nil
}

func teamSetOption(team int, o teamOption) error {
	fam, err := teamFamily()
	if err != nil {
		return err
	}
	req := nl.NewNetlinkRequest(int(fam.ID), unix.NLM_F_ACK)
	req.AddData(&nl.Genlmsg{Command: teamCmdOptionsSet, Version: 1})
	req.AddData(nl.NewRtAttr(teamAttrTeamIfindex, nl.Uint32Attr(uint32(team))))
	list := nl.NewRtAttr(teamAttrListOption, nil)
	{
		item := list.AddRtAttr(teamAttrItemOption, nil)
		item.AddRtAttr(teamAttrOptionName, nl.ZeroTerminated(o.name))
		item.AddRtAttr(teamAttrOptionType, []byte{o.typ})
		if o.typ != nlaFlag || o.data != nil {
			item.AddRtAttr(teamAttrOptionData, o.data)
		}
		if o.port != 0 {
			item.AddRtAttr(teamAttrOptionPortIfindex, nl.Uint32Attr(uint32(o.port)))
		}
	}
	req.AddData(list)
	// The driver announces every change on its "change_event" group and
	// passes on ESRCH when nobody listens there: the options are set.
	if _, err := nlx.Execute(req, unix.NETLINK_GENERIC, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}

func flag(v bool) []byte {
	if v {
		return []byte{}
	}
	return nil
}

// teamOptionValue is an option as read back.
type teamOptionValue struct {
	port int
	data []byte
	set  bool // present (for flags: true)
}

// teamGetOptions returns the options by name (per-port options once per
// port).
func teamGetOptions(team int) (map[string][]teamOptionValue, error) {
	fam, err := teamFamily()
	if err != nil {
		return nil, err
	}
	req := nl.NewNetlinkRequest(int(fam.ID), unix.NLM_F_ACK)
	req.AddData(&nl.Genlmsg{Command: teamCmdOptionsGet, Version: 1})
	req.AddData(nl.NewRtAttr(teamAttrTeamIfindex, nl.Uint32Attr(uint32(team))))
	msgs, err := nlx.Execute(req, unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, fmt.Errorf("team options: %w", err)
	}
	out := map[string][]teamOptionValue{}
	for _, m := range msgs {
		if len(m) < nl.SizeofGenlmsg {
			continue
		}
		attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
		if err != nil {
			continue
		}
		for _, a := range attrs {
			if a.Attr.Type&nl.NLA_TYPE_MASK != teamAttrListOption {
				continue
			}
			items, _ := nl.ParseRouteAttr(a.Value)
			for _, it := range items {
				fields, _ := nl.ParseRouteAttr(it.Value)
				var name string
				var v teamOptionValue
				for _, f := range fields {
					switch f.Attr.Type & nl.NLA_TYPE_MASK {
					case teamAttrOptionName:
						name = string(trimNul(f.Value))
					case teamAttrOptionData:
						v.data, v.set = f.Value, true
					case teamAttrOptionPortIfindex:
						if len(f.Value) >= 4 {
							v.port = int(binary.NativeEndian.Uint32(f.Value))
						}
					}
				}
				if name != "" {
					out[name] = append(out[name], v)
				}
			}
		}
	}
	return out, nil
}

func trimNul(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

// createTeam creates an LACP bundle device.
func CreateTeam(name, hashPolicy string) error {
	if err := nlx.LinkAdd(&netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: name}, LinkType: "team"}); err != nil {
		return fmt.Errorf("%s: creating team device: %w", name, err)
	}
	l, err := nlx.LinkByName(name)
	if err != nil {
		return err
	}
	if err := teamSetOptions(l.Attrs().Index, teamOption{name: "mode", typ: nlaString, data: nl.ZeroTerminated("loadbalance")}); err != nil {
		nlx.LinkDel(l)
		return err
	}
	return SetTeamHash(l.Attrs().Index, hashPolicy)
}

func SetTeamHash(team int, policy string) error {
	prog, err := hashProgramBytes(policy)
	if err != nil {
		return err
	}
	return teamSetOptions(team,
		teamOption{name: "lb_tx_method", typ: nlaString, data: nl.ZeroTerminated("hash")},
		teamOption{name: "bpf_hash_func", typ: nlaBinary, data: prog})
}

// teamHashPolicy reads the hash policy back ("" if it is not ours).
func TeamHashPolicy(team int) string {
	opts, err := teamGetOptions(team)
	if err != nil {
		return ""
	}
	for _, v := range opts["bpf_hash_func"] {
		return hashPolicyOf(v.data)
	}
	return ""
}

// teamPortInit makes a port that was just added to a team carry nothing
// until LACP says so (it is still down when this runs).
func TeamPortInit(team, port int) error {
	return teamSetOptions(team,
		teamOption{name: "enabled", port: port, typ: nlaFlag, data: flag(false)},
		teamOption{name: "user_linkup_enabled", port: port, typ: nlaFlag, data: flag(true)},
		teamOption{name: "user_linkup", port: port, typ: nlaFlag, data: flag(false)})
}

// SetTeamPort lets a port of an LACP bundle carry traffic or not (LACP
// collecting and distributing). The bundle's link is up while at least
// one port carries traffic.
func SetTeamPort(teamName, portName string, on bool) error {
	t, err := nlx.LinkByName(teamName)
	if err != nil {
		return err
	}
	p, err := nlx.LinkByName(portName)
	if err != nil {
		return err
	}
	// Link state first when enabling (the bridge forwards once it is up),
	// last when disabling.
	link := teamOption{name: "user_linkup", port: p.Attrs().Index, typ: nlaFlag, data: flag(on)}
	en := teamOption{name: "enabled", port: p.Attrs().Index, typ: nlaFlag, data: flag(on)}
	if on {
		return teamSetOptions(t.Attrs().Index, en, link)
	}
	return teamSetOptions(t.Attrs().Index, link, en)
}

// TeamPortsEnabled reports which ports of a team carry traffic.
func TeamPortsEnabled(teamName string) (map[string]bool, error) {
	t, err := nlx.LinkByName(teamName)
	if err != nil {
		return nil, err
	}
	opts, err := teamGetOptions(t.Attrs().Index)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, v := range opts["enabled"] {
		if l, err := nlx.LinkByIndex(v.port); err == nil {
			out[l.Attrs().Name] = v.set
		}
	}
	return out, nil
}
