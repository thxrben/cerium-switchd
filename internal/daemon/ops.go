package daemon

import (
	"errors"
	"fmt"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"log/slog"
	"maps"
	"mclag/internal/inventory"
	"mclag/internal/lacp"
	"mclag/internal/schema"
	"mclag/internal/stack"
	"net"
	"os"
	"slices"
	"sort"
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
	kernel   *dataplane.Netlink
	engine   *commit.Engine
	names    *inventory.Naming
	member   int
	vc       *stack.Manager
	hostName func() string
	// started is when switchd started; notify reaches every CLI session.
	started time.Time
	notify  func(string)
	log     *slog.Logger
	dryRun  bool
	lacp    *lacp.Runtime
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
		case cfg.L3[name+".0"] != nil && cfg.L3[name+".0"].Instance == model.MgmtInstance:
			s.Role = "management"
		case cfg.L3[name+".0"] != nil || i.VlanTagging:
			s.Role = "routed"
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
	out = append(out, o.l3Units(cfg)...)
	return out, nil
}

// l3Units reports the routed units of this member ("irb.10", "1/0/6.100",
// "1/0/5.0").
func (o *ops) l3Units(cfg *model.Config) []cli.IfStatus {
	var out []cli.IfStatus
	for _, name := range slices.Sorted(maps.Keys(cfg.L3)) {
		u := cfg.L3[name]
		if !u.OnMember(o.member) {
			continue
		}
		var linux string
		switch {
		case u.IRB():
			linux = name
		case u.Tag != 0:
			linux = dataplane.SubifName(u.Parent, u.Unit)
		case schema.IsAE(u.Parent):
			linux = u.Parent
		default:
			if u.Member != o.member {
				continue
			}
			linux, _ = o.names.Linux(u.Parent)
		}
		l, err := netlink.LinkByName(linux)
		if linux == "" || err != nil {
			continue
		}
		a := l.Attrs()
		s := cli.IfStatus{Name: name, Linux: linux, Configured: true, AdminUp: a.Flags&net.FlagUp != 0,
			OperUp: a.OperState == netlink.OperUp || a.OperState == netlink.OperUnknown && a.Flags&net.FlagUp != 0,
			MTU:    a.MTU + model.EthHeader, Description: u.Description, MAC: a.HardwareAddr.String()}
		for _, p := range u.AddrsOn(o.member) {
			s.Addrs = append(s.Addrs, p.String())
		}
		s.Role = "routed"
		if u.Instance == model.MgmtInstance {
			s.Role = "management"
		} else if u.Instance != "" {
			s.Role = "routed (" + u.Instance + ")"
		}
		if u.IRB() && u.Instance != model.MgmtInstance {
			s.Role = "irb vlan " + strconv.Itoa(u.VLAN)
			if v := cfg.VLANByID[u.VLAN]; v != nil {
				s.Role = "irb " + v.Name
			}
		}
		if len(s.Addrs) > 0 {
			s.Role += " " + s.Addrs[0]
			if len(s.Addrs) > 1 {
				s.Role += fmt.Sprintf(" (+%d)", len(s.Addrs)-1)
			}
		}
		out = append(out, s)
	}
	return out
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

func (o *ops) Offload() ([]cli.OffloadPort, error) {
	o.names.Refresh()
	feat := func(fs map[string]string, names ...string) string {
		for _, n := range names {
			if v, ok := fs[n]; ok {
				return v
			}
		}
		return "-"
	}
	var out []cli.OffloadPort
	for _, p := range o.names.Ports() {
		c := inventory.ReadCaps("/sys", p.Linux)
		pause := map[inventory.Tristate]string{inventory.Yes: "yes", inventory.No: "no", inventory.Unknown: "-"}[c.Pause]
		out = append(out, cli.OffloadPort{Name: p.Name, Linux: p.Linux, Driver: p.Driver, MaxSpeedMbps: c.MaxSpeedMbps,
			Pause: pause, Switchdev: c.Switchdev,
			TC:         feat(c.Features, "hw-tc-offload"),
			VLANFilter: feat(c.Features, "rx-vlan-filter"),
			Csum:       feat(c.Features, "tx-checksum-ip-generic", "tx-checksum-ipv4"),
			TSO:        feat(c.Features, "tx-tcp-segmentation"),
			GRO:        feat(c.Features, "rx-gro"),
		})
	}
	return out, nil
}

func (o *ops) Routes(instance string) ([]cli.Route, error) {
	table := unix.RT_TABLE_MAIN
	if instance != "" {
		l, err := netlink.LinkByName(instance)
		v, ok := l.(*netlink.Vrf)
		if err != nil || !ok {
			return nil, fmt.Errorf("routing instance %s does not exist on this member", instance)
		}
		table = int(v.Table)
	}
	rs, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, err
	}
	links, _ := netlink.LinkList()
	byIndex := map[int]string{}
	for _, l := range links {
		byIndex[l.Attrs().Index] = l.Attrs().Name
	}
	ifName := func(i int) string {
		n := byIndex[i]
		if sw, ok := o.names.Name(n); ok {
			return sw
		}
		return n
	}
	var out []cli.Route
	for _, r := range rs {
		if r.Type != unix.RTN_UNICAST && r.Type != unix.RTN_BLACKHOLE {
			continue // local/broadcast entries of the kernel
		}
		dst := "default"
		if r.Dst != nil {
			if ones, bits := r.Dst.Mask.Size(); !(ones == 0 && bits > 0) {
				dst = r.Dst.String()
			}
		}
		if dst == "default" && r.Family == netlink.FAMILY_V6 {
			dst = "::/0"
		} else if dst == "default" {
			dst = "0.0.0.0/0"
		}
		e := cli.Route{Dest: dst, Proto: routeProto(r.Protocol), Metric: r.Priority}
		switch {
		case r.Type == unix.RTN_BLACKHOLE:
			e.Via = "discard"
		case len(r.MultiPath) > 0:
			for _, h := range r.MultiPath {
				e.Via += fmt.Sprintf("%s via %s, ", h.Gw, ifName(h.LinkIndex))
			}
			e.Via = strings.TrimSuffix(e.Via, ", ")
		case r.Gw != nil:
			e.Via = fmt.Sprintf("%s via %s", r.Gw, ifName(r.LinkIndex))
		default:
			e.Via = ifName(r.LinkIndex)
		}
		out = append(out, e)
	}
	return out, nil
}

func routeProto(p netlink.RouteProtocol) string {
	switch int(p) {
	case dataplane.RouteProto:
		return "static"
	case unix.RTPROT_KERNEL:
		return "direct"
	case unix.RTPROT_BOOT, unix.RTPROT_STATIC:
		return "os"
	case unix.RTPROT_DHCP:
		return "dhcp"
	case unix.RTPROT_RA:
		return "ra"
	}
	return strconv.Itoa(int(p))
}

func (o *ops) VirtualChassis() (cli.VCStatus, error) {
	st := cli.VCStatus{StackID: o.vc.StackID(), Member: o.vc.Member(), HostName: o.hostName()}
	if st.StackID == "" {
		return st, fmt.Errorf("the stack is not running (dry-run mode?)")
	}
	for _, p := range o.vc.Ports() {
		st.Ports = append(st.Ports, cli.VCPort{Port: p.Port, Linux: p.Linux, State: p.State, Neighbor: p.Neighbor,
			PeerPort: p.PeerPort, UpSince: p.UpSince, LastError: p.LastError})
	}
	if n := o.vc.Control; n != nil {
		st.Control, st.Master = true, n.Master()
		for id := range n.Members() {
			st.Members = append(st.Members, id)
		}
		sort.Ints(st.Members)
		for _, s := range n.Servers() {
			if s.Voter {
				st.Voters = append(st.Voters, s.Member)
			}
		}
		if m := o.vc.Mesh(); m != nil {
			st.Reachable = m.Reachable()
		}
	}
	return st, nil
}

func (o *ops) LACP() ([]lacp.BundleStatus, error) {
	if o.lacp == nil {
		return nil, errors.New("LACP is not running (dry-run mode?)")
	}
	return o.lacp.Status(), nil
}

func (o *ops) SwitchMaster(to int, user string) error {
	n := o.vc.Control
	if n == nil {
		return errors.New("stack control is not running")
	}
	if to != 0 && to == n.Master() {
		return fmt.Errorf("member %d is already master", to)
	}
	o.log.Warn("mastership switch requested", "facility", "change-log", "to", to, "user", user)
	return n.Transfer(to)
}

func (o *ops) RemoveVCMember(id int, user string) error {
	n := o.vc.Control
	if n == nil {
		return errors.New("stack control is not running")
	}
	if len(n.Members()) <= 1 {
		return errors.New("the last member cannot be removed")
	}
	o.log.Warn("virtual chassis member removal", "facility", "change-log", "member", id, "user", user)
	return n.RemoveMember(id)
}

func (o *ops) SetVCPort(local string, add bool, user string) error {
	if err := o.vc.SetPort(local, add); err != nil {
		return err
	}
	what := "set"
	if !add {
		what = "deleted"
	}
	o.log.Info("VC port "+what, "facility", "change-log", "port", local, "user", user)
	return nil
}

func (o *ops) AddVCMember(id int, user string) (string, error) {
	tok, err := o.vc.AddMember(id)
	if err == nil {
		o.log.Info("virtual chassis join token issued", "facility", "change-log", "member", id, "user", user)
	}
	return tok, err
}

func (o *ops) JoinVC(token, user string) (int, error) {
	o.log.Warn("joining a virtual chassis", "facility", "change-log", "user", user)
	return o.vc.Join(token)
}
