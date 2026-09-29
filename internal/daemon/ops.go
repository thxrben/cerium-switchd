package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"mclag/internal/cli"
	"mclag/internal/commit"
	"mclag/internal/dataplane"
	"mclag/internal/model"
)

// ops implements cli.Operational from the kernel and the active
// configuration of this member.
type ops struct {
	kernel *dataplane.Netlink
	engine *commit.Engine
	member int
}

func (o *ops) model() *model.Config {
	cfg, _ := model.Build(o.engine.Active().Active(), nil)
	return cfg
}

// cfgName maps a kernel link to its configuration name.
func (o *ops) cfgName(link string, kind dataplane.Kind) string {
	if kind == dataplane.Bond {
		return link
	}
	return fmt.Sprintf("%d/%s", o.member, link)
}

func (o *ops) Interfaces() ([]cli.IfStatus, error) {
	st, err := o.kernel.Status()
	if err != nil {
		return nil, err
	}
	cfg := o.model()
	vlanName := map[int]string{}
	for _, v := range cfg.VLANs {
		vlanName[v.ID] = v.Name
	}
	var out []cli.IfStatus
	for _, p := range st {
		if p.Kind != dataplane.Physical && p.Kind != dataplane.Bond {
			continue
		}
		name := o.cfgName(p.Name, p.Kind)
		i := cfg.Interfaces[name]
		s := cli.IfStatus{
			Name: name, Linux: p.Name, Configured: i != nil, AdminUp: p.AdminUp, OperUp: p.OperUp,
			MTU: p.MTU + model.EthHeader, SpeedMbps: p.SpeedMbps, Description: p.Alias, MAC: p.MAC,
			TaggedDrops: p.TaggedDrops,
			Counters: cli.IfCounters{
				RxPackets: p.Counters.RxPackets, TxPackets: p.Counters.TxPackets, RxBytes: p.Counters.RxBytes,
				TxBytes: p.Counters.TxBytes, RxErrors: p.Counters.RxErrors, TxErrors: p.Counters.TxErrors,
				RxDropped: p.Counters.RxDropped, TxDropped: p.Counters.TxDropped, RxMulticast: p.Counters.RxMulticast,
			},
		}
		switch {
		case i == nil:
		case i.Parent != "":
			s.Role = "member of " + i.Parent
		case i.Switching && i.Mode == "trunk":
			s.Role = "trunk"
			if i.NativeVLAN != 0 {
				s.Role += " native " + vlanName[i.NativeVLAN]
			}
		case i.Switching:
			s.Role = "access " + vlanName[i.AccessVLAN]
		default:
			s.Role = "plain"
		}
		for _, vid := range slices.Sorted(maps.Keys(p.VLANs)) {
			f := p.VLANs[vid]
			d := fmt.Sprintf("%s (%d", vlanName[int(vid)], vid)
			if f.Untagged {
				d += ", untagged"
			}
			s.VLANs = append(s.VLANs, strings.TrimPrefix(d, " ")+")")
		}
		out = append(out, s)
	}
	return out, nil
}

func (o *ops) MACTable() ([]cli.MACEntry, error) {
	fdb, err := o.kernel.FDB()
	if err != nil {
		return nil, err
	}
	cfg := o.model()
	names := map[int]string{}
	for _, v := range cfg.VLANs {
		names[v.ID] = v.Name
	}
	st, _ := o.kernel.Read()
	var out []cli.MACEntry
	for _, e := range fdb {
		kind := dataplane.Physical
		if st != nil && st.Links[e.Port] != nil {
			kind = st.Links[e.Port].Kind
		}
		out = append(out, cli.MACEntry{VLAN: e.VLAN, VLANName: names[e.VLAN], MAC: e.MAC,
			Interface: o.cfgName(e.Port, kind), Static: e.Static, Age: e.AgeSeconds})
	}
	return out, nil
}

func (o *ops) ClearMACTable(vlan int, iface string) (int, error) {
	port := ""
	if iface != "" {
		port = iface
		if _, l, ok := strings.Cut(iface, "/"); ok {
			port = l
		}
	}
	return o.kernel.FlushFDB(vlan, port)
}
