package daemon

import (
	"fmt"
	"github.com/vishvananda/netlink"
	"log/slog"
	"maps"
	"mclag/internal/inventory"
	"mclag/internal/schema"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

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
	names  *inventory.Naming
	member int
	// started is when switchd started; notify reaches every CLI session.
	started time.Time
	notify  func(string)
	log     *slog.Logger
	dryRun  bool
}

func (o *ops) model() *model.Config {
	cfg, _ := model.Build(o.engine.Active().Active(), nil)
	return cfg
}

// cfgName maps a kernel link to its configuration name ("" = not a
// switch port, e.g. a virtual device).
func (o *ops) cfgName(link string, kind dataplane.Kind) string {
	if kind == dataplane.Bond {
		return link
	}
	n, _ := o.names.Name(link)
	return n
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
		if name == "" {
			continue
		}
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

func (o *ops) Hardware() ([]cli.HardwarePort, error) {
	o.names.Refresh()
	var out []cli.HardwarePort
	for _, p := range o.names.Ports() {
		out = append(out, cli.HardwarePort{Name: p.Name, Linux: p.Linux, Bus: p.Bus, Driver: p.Driver, MAC: p.MAC})
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
	port := iface // ae interfaces: same name in the kernel
	if _, ok := schema.ParsePhysical(iface); ok {
		l, ok := o.names.Linux(iface)
		if !ok {
			return 0, fmt.Errorf("%s does not exist", iface)
		}
		port = l
	}
	return o.kernel.FlushFDB(vlan, port)
}

func (o *ops) Neighbors(ipv6 bool) ([]cli.Neighbor, error) {
	fam := netlink.FAMILY_V4
	if ipv6 {
		fam = netlink.FAMILY_V6
	}
	neighs, err := netlink.NeighList(0, fam)
	if err != nil {
		return nil, err
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	byIndex := map[int]netlink.Link{}
	for _, l := range links {
		byIndex[l.Attrs().Index] = l
	}
	var out []cli.Neighbor
	for _, n := range neighs {
		l := byIndex[n.LinkIndex]
		if l == nil || n.State&(netlink.NUD_NOARP) != 0 || n.IP.IsMulticast() || l.Attrs().Name == "lo" {
			continue
		}
		name := l.Attrs().Name
		if sw, ok := o.names.Name(name); ok {
			name = sw
		}
		inst := "default"
		if m := byIndex[l.Attrs().MasterIndex]; m != nil && m.Type() == "vrf" {
			inst = m.Attrs().Name
		}
		mac := n.HardwareAddr.String()
		if mac == "" {
			mac = "(incomplete)"
		}
		out = append(out, cli.Neighbor{MAC: mac, IP: n.IP.String(), Interface: name, Instance: inst, State: nudState(n.State)})
	}
	return out, nil
}

func nudState(s int) string {
	switch {
	case s&netlink.NUD_PERMANENT != 0:
		return "permanent"
	case s&netlink.NUD_REACHABLE != 0:
		return "reachable"
	case s&netlink.NUD_STALE != 0:
		return "stale"
	case s&netlink.NUD_DELAY != 0, s&netlink.NUD_PROBE != 0:
		return "probing"
	case s&netlink.NUD_FAILED != 0:
		return "failed"
	case s&netlink.NUD_INCOMPLETE != 0:
		return "incomplete"
	}
	return "-"
}

func (o *ops) Uptime() (cli.Uptime, error) {
	u := cli.Uptime{Started: o.started}
	if raw, err := os.ReadFile("/proc/stat"); err == nil {
		for _, l := range strings.Split(string(raw), "\n") {
			if v, ok := strings.CutPrefix(l, "btime "); ok {
				if sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
					u.Booted = time.Unix(sec, 0)
				}
			}
		}
	}
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(raw))
		for i := 0; i < 3 && i < len(f); i++ {
			u.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	return u, nil
}

var powerUnit = map[string]struct{ now, flag string }{
	"reboot": {"reboot", "-r"}, "halt": {"halt", "-H"}, "power-off": {"poweroff", "-P"},
}

func (o *ops) Power(action string, minutes int, user string) error {
	p, ok := powerUnit[action]
	if !ok {
		return fmt.Errorf("unknown action %q", action)
	}
	when := "now"
	if minutes > 0 {
		when = fmt.Sprintf("in %d minutes", minutes)
	}
	o.log.Warn("system "+action+" requested", "facility", "change-log", "user", user, "when", when)
	o.notify(fmt.Sprintf("%s: system %s requested (%s)", user, action, when))
	if o.dryRun {
		return nil
	}
	if minutes == 0 {
		// Give the notice and the reply time to reach the sessions.
		go func() {
			time.Sleep(time.Second)
			if err := command("systemctl", p.now); err != nil {
				o.log.Error("system "+action, "err", err)
			}
		}()
		return nil
	}
	// --no-wall: switchd notifies the CLI sessions itself; wall messages
	// would garble their terminals.
	return command("shutdown", "--no-wall", p.flag, fmt.Sprintf("+%d", minutes))
}

func (o *ops) CancelPower(user string) error {
	if o.dryRun {
		return nil
	}
	if err := command("shutdown", "--no-wall", "-c"); err != nil {
		return err
	}
	o.log.Warn("scheduled system shutdown cancelled", "facility", "change-log", "user", user)
	o.notify(user + ": scheduled reboot/halt/power-off cancelled")
	return nil
}
