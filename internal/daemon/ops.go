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
	"mclag/internal/lldp"
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
	"mclag/internal/dhcp"
	"mclag/internal/diag"
	"mclag/internal/model"
	"mclag/internal/ntp"
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
	lldp    *lldp.Agent
	updater *updater
	mclag   *mclagCtl
	ntp     *ntp.Client
	maint   *maintCtl
	stp     *rstpCtl
	dhcp    *dhcp.Manager
	// restart ends switchd so that systemd starts it again.
	restart func()
	diag    diag.Collector
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
	if m := dataplane.TunnelMember(link); m > 0 {
		return fmt.Sprintf("vc-%d", m) // stack tunnel (reference 5.2)
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
			MTU: p.MTU + model.EthHeader, SpeedMbps: p.SpeedMbps, MAC: p.MAC,
			TaggedDrops: p.TaggedDrops,
			Counters: cli.IfCounters{
				RxPackets: p.Counters.RxPackets, TxPackets: p.Counters.TxPackets, RxBytes: p.Counters.RxBytes,
				TxBytes: p.Counters.TxBytes, RxErrors: p.Counters.RxErrors, TxErrors: p.Counters.TxErrors,
				RxDropped: p.Counters.RxDropped, TxDropped: p.Counters.TxDropped, RxMulticast: p.Counters.RxMulticast,
			},
		}
		if i != nil {
			s.Description = i.Description
			if i.AE {
				s.Ports, s.Members = i.MemberPorts, []int{o.member}
			}
		}
		switch {
		case o.vc != nil && o.vc.IsPort(p.Name):
			s.Role, s.Configured = "stacking", true
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
		case i.Management:
			s.Role = "management"
			if ln, err := netlink.LinkByName(dataplane.CMEName); err == nil && ln.Attrs().ParentIndex > 0 {
				if par, err := netlink.LinkByIndex(ln.Attrs().ParentIndex); err == nil && par.Attrs().Name == p.Name {
					s.Role += ", active (cme)"
				}
			}
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
		case u.CME():
			linux = dataplane.CMEName
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
		mgmt := u.Instance != "" && u.Instance == cfg.System.MgmtInstance
		if mgmt {
			s.Role = "management (" + u.Instance + ")"
		} else if u.Instance != "" {
			s.Role = "routed (" + u.Instance + ")"
		}
		if u.IRB() && !mgmt {
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
	vteps := vxlanVTEPs() // remote MACs: the VTEP they are behind
	var out []cli.MACEntry
	for _, e := range fdb {
		kind := dataplane.Physical
		if st != nil && st.Links[e.Port] != nil {
			kind = st.Links[e.Port].Kind
		}
		iface := o.cfgName(e.Port, kind)
		if vni := dataplane.VXLANVNI(e.Port); vni > 0 {
			iface = "vtep " + orUnknown(vteps[fmt.Sprintf("%d %s", vni, e.MAC)])
		}
		out = append(out, cli.MACEntry{VLAN: e.VLAN, VLANName: names[e.VLAN], MAC: e.MAC,
			Interface: iface, Static: e.Static, Age: e.AgeSeconds})
	}
	return out, nil
}

func (o *ops) ClearMACTable(vlan int, iface string) (int, error) {
	port := iface // ae interfaces: same name in the kernel
	var m int
	if _, err := fmt.Sscanf(iface, "vc-%d", &m); err == nil && fmt.Sprintf("vc-%d", m) == iface {
		port = dataplane.TunnelName(m)
	}
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

func (o *ops) NTP() (cli.NTPStatus, error) {
	if o.ntp == nil {
		return cli.NTPStatus{}, nil
	}
	st := o.ntp.Status()
	out := cli.NTPStatus{Synced: st.Synced, LastAdjust: st.LastAdjust, LastStep: st.LastStep, Via: st.Via}
	for _, s := range st.Servers {
		out.Servers = append(out.Servers, cli.NTPServerStatus{Host: s.Host, Prefer: s.Prefer, Addr: s.Addr, Stratum: s.Stratum,
			Offset: s.Offset, Delay: s.Delay, LastPoll: s.LastPoll, Reach: s.Reach, Err: s.Err, Selected: s.Selected})
	}
	return out, nil
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
		// Give the notice and the reply time to reach the sessions, then
		// move the traffic away (a scheduled one drains when the system
		// stops).
		go func() {
			time.Sleep(time.Second)
			o.maint.drainForShutdown(action)
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

func (o *ops) Cards() ([]cli.CardStatus, error) {
	o.names.Refresh()
	var out []cli.CardStatus
	for _, c := range o.names.Cards() {
		out = append(out, cli.CardStatus{Member: o.member, Number: c.Number, Key: c.Key, Present: c.Present, Driver: c.Driver, Ports: c.Ports,
			LastSeen: c.LastSeen, Note: c.Note, MovedFrom: c.MovedFrom})
	}
	return out, nil
}

func (o *ops) CardInterfaces(cards ...int) []string {
	var out []string
	for n := range o.model().Interfaces {
		if p, ok := schema.ParsePhysical(n); ok && p.Member == o.member && slices.Contains(cards, p.Card) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (o *ops) CardRenumber(from, to int, user string) error {
	if err := o.names.Renumber(from, to); err != nil {
		return err
	}
	o.log.Warn("card renumbered", "facility", "change-log", "card", from, "to", to, "user", user)
	o.names.Refresh()
	if o.restart != nil {
		// Every part of switchd picks the new names up at once.
		go func() { time.Sleep(500 * time.Millisecond); o.restart() }()
	}
	return nil
}

func (o *ops) CardForget(card int, user string) error {
	if err := o.names.Forget(card); err != nil {
		return err
	}
	o.log.Warn("card number released", "facility", "change-log", "card", card, "user", user)
	return nil
}

func (o *ops) VLANMTUDrops() (map[int]uint64, error) { return dataplane.VLANMTUDrops() }

func (o *ops) DHCPBindings() ([]cli.DHCPBinding, error) {
	if o.dhcp == nil {
		return nil, errors.New("not available (dry-run mode?)")
	}
	var out []cli.DHCPBinding
	for _, b := range o.dhcp.Bindings() {
		cb := cli.DHCPBinding{Unit: b.Unit, State: b.State.String(), Instance: b.VRF}
		if l := b.Lease; l != nil {
			cb.Address, cb.Server, cb.Domain, cb.Lease = l.Addr.String(), l.Server.String(), l.Domain, l.Time
			if l.Router.IsValid() {
				cb.Router = l.Router.String()
			}
			for _, d := range l.DNS {
				cb.DNS = append(cb.DNS, d.String())
			}
			cb.Renew, cb.Expires = l.Acquired.Add(l.T1), l.Expires()
		}
		out = append(out, cb)
	}
	return out, nil
}

func (o *ops) SpanningTree() (cli.STPStatus, error) {
	if o.stp == nil {
		return cli.STPStatus{}, errors.New("not available (dry-run mode?)")
	}
	st, err := o.stp.status()
	if err != nil {
		return cli.STPStatus{}, err
	}
	out := cli.STPStatus{Running: st.Running, Owner: st.Owner, BridgeID: st.Bridge.ID.String(), RootID: st.Root.Root.String(),
		RootCost: st.Root.Cost, RootPort: st.RootPort, HelloTime: st.Times.HelloTime, MaxAge: st.Times.MaxAge,
		ForwardDelay: st.Times.ForwardDelay, Changes: st.Changes}
	for _, p := range st.Ports {
		state := "discarding"
		switch {
		case p.Forwarding:
			state = "forwarding"
		case p.Learning:
			state = "learning"
		}
		out.Ports = append(out.Ports, cli.STPPort{Name: p.Name, Role: p.Role.String(), State: state, Cost: p.Cost,
			PortID: p.ID.String(), DesignatedBridge: p.Designated.Bridge.String(), DesignatedPort: p.Designated.Port.String(),
			Edge: p.Edge, OperEdge: p.OperEdge, P2P: p.P2P, RSTP: p.RSTP, RootInconsistent: p.RootInconsistent,
			Enabled: p.Enabled, Rx: p.RxBPDUs, Tx: p.TxBPDUs})
	}
	return out, nil
}

func (o *ops) Maintenance(enter, force bool, user string) (string, error) {
	if o.maint == nil {
		return "", errors.New("not available (dry-run mode?)")
	}
	if enter {
		o.notify(user + ": this member enters maintenance mode")
		return o.maint.enter(force, true, user)
	}
	return o.maint.exit(user)
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
	if instance == dataplane.StackVRF {
		return nil, fmt.Errorf("routing instance %s does not exist on this member", instance) // internal (reference 5.2)
	}
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
		// Full interface names: this member's port, and the neighbour's
		// port with its member id.
		vp := cli.VCPort{Port: strconv.Itoa(st.Member) + "/" + p.Port, Linux: p.Linux, State: p.State, Neighbor: p.Neighbor,
			PeerPort: p.PeerPort, UpSince: p.UpSince, LastError: p.LastError}
		if p.PeerPort != "" && p.NeighborID > 0 {
			vp.PeerPort = strconv.Itoa(p.NeighborID) + "/" + p.PeerPort
		}
		if raw, err := os.ReadFile("/sys/class/net/" + p.Linux + "/speed"); err == nil && p.State == "up" {
			if v, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && v > 0 {
				vp.SpeedMbps = v
			}
		}
		st.Ports = append(st.Ports, vp)
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
			st.Maintenance = m.Draining()
		}
	}
	return st, nil
}

// Limits gathers the hardware facts of "show system limits".
func (o *ops) Limits() (cli.LimitsStatus, error) {
	st := cli.LimitsStatus{Member: o.member}
	ks, err := o.kernel.Read()
	if err != nil {
		return st, err
	}
	for _, p := range o.names.Ports() {
		st.Ports++
		if o.vc != nil && o.vc.IsPort(p.Linux) {
			st.StackPorts++
		}
		if l := ks.Links[p.Linux]; l != nil && l.MaxMTU > 0 {
			f := l.MaxMTU + model.EthHeader
			if st.LowestMaxMTU == 0 || f < st.LowestMaxMTU {
				st.LowestMaxMTU, st.LowestMaxPort = f, p.Name
			}
			if f > st.HighestMaxMTU {
				st.HighestMaxMTU, st.HighestMaxPort = f, p.Name
			}
		}
		if c := inventory.ReadCaps("/sys", p.Linux); c.MaxSpeedMbps > st.FastestMbps {
			st.FastestMbps, st.FastestPort = c.MaxSpeedMbps, p.Name
		}
	}
	if fdb, err := o.kernel.FDB(); err == nil {
		for _, e := range fdb {
			if !e.Static {
				st.MACEntries++
			}
		}
	}
	return st, nil
}

// StackMTU is "show virtual-chassis mtu" of this member.
func (o *ops) StackMTU() (cli.StackMTUStatus, error) {
	cfg := o.model()
	if cfg == nil {
		return cli.StackMTUStatus{}, errors.New("no valid configuration")
	}
	mtu, where := cfg.MaxDataMTU()
	st := cli.StackMTUStatus{Member: o.member, DataMTU: mtu, Where: where, Stack: len(cfg.SwitchMembers()) > 1}
	ks, err := o.kernel.Read()
	if err != nil {
		return st, err
	}
	for _, p := range o.vc.Ports() {
		l := ks.Links[p.Linux]
		if l == nil {
			continue
		}
		sp := cli.StackMTUPort{Port: fmt.Sprintf("%d/%s", o.member, p.Port), MTU: l.MTU + model.EthHeader, PathMTU: p.PathMTU}
		if l.MaxMTU > 0 {
			sp.MaxMTU = min(l.MaxMTU, model.MaxStackPortMTU) + model.EthHeader
		}
		st.Ports = append(st.Ports, sp)
	}
	return st, nil
}

func (o *ops) SoftwareStart(r cli.SoftwareRequest) error {
	if o.updater == nil {
		return errors.New("software updates are not available")
	}
	return o.updater.Start(r)
}

func (o *ops) Software() (cli.SoftwareStatus, error) {
	if o.updater == nil {
		return cli.SoftwareStatus{}, errors.New("software updates are not available")
	}
	return o.updater.Status()
}

func (o *ops) LLDP() (cli.LLDPStatus, error) {
	if o.lldp == nil {
		return cli.LLDPStatus{}, nil
	}
	sys, ports, stats := o.lldp.Status()
	return cli.LLDPStatus{Running: len(ports) > 0, System: sys, Ports: ports, Stats: stats, Neighbors: o.lldp.Neighbors()}, nil
}

func (o *ops) LACP() ([]lacp.BundleStatus, error) {
	if o.lacp == nil {
		return nil, errors.New("LACP is not running (dry-run mode?)")
	}
	return o.lacp.Status(), nil
}

func (o *ops) MCLAG() ([]cli.MCLAGStatus, error) {
	if o.mclag == nil {
		return nil, errors.New("MC-LAG is not running (dry-run mode?)")
	}
	return o.mclag.status()
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

func (o *ops) ForceMaster(user string) error {
	n := o.vc.Control
	if n == nil {
		return errors.New("stack control is not running")
	}
	if err := n.ForceMaster(); err != nil {
		return err
	}
	o.log.Warn("force-master: this member continues without the others (split-brain risk)", "facility", "change-log", "user", user)
	go func() {
		time.Sleep(time.Second) // let the CLI answer first
		o.restart()
	}()
	return nil
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
	// The member drains first (its MC-LAG partners move to the peer); one
	// that cannot be reached carries nothing anyway.
	if id == o.member {
		o.maint.drainForShutdown("the removal from the stack")
	} else if _, err := n.Call(id, "drain", "the removal from the stack", maintDrainWait+5*time.Second); err != nil {
		o.log.Info("virtual chassis member removal: member not drained", "member", id, "err", err)
	}
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

// Multicast is this member's IGMP/MLD snooping (reference 5.5).
func (o *ops) Multicast() ([]cli.McastStatus, error) {
	cfg := o.model()
	st := cli.McastStatus{Member: o.member}
	es, rs, err := dataplane.McastGroups()
	if err != nil {
		return nil, err
	}
	name := func(k string) string {
		if n := o.cfgName(k, dataplane.Physical); n != "" {
			return n
		}
		if m := dataplane.TunnelMember(k); m > 0 {
			return fmt.Sprintf("vc-%d", m)
		}
		return k // ae, VXLAN ports
	}
	for _, e := range es {
		st.Groups = append(st.Groups, cli.McastGroup{VLAN: e.VID, Group: e.Group, Interface: name(e.Port), Static: e.Permanent,
			Mode: e.Mode, Sources: e.Sources, Expires: e.Expires})
	}
	for _, r := range rs {
		st.Routers = append(st.Routers, cli.McastRouter{VLAN: r.VID, Interface: name(r.Port), Permanent: r.Permanent, Expires: r.Expires})
	}
	desired, _ := dataplane.Compute(cfg, o.member, o.names.Linux)
	mc := dataplane.ComputeMulticast(cfg, desired, o.names.Linux)
	for _, vid := range slices.Sorted(maps.Keys(mc.VLANs)) {
		v := mc.VLANs[vid]
		st.VLANs = append(st.VLANs, cli.McastVLAN{VLAN: int(vid), Snooping: v.Snooping, Querier: v.Snooping && v.Querier})
	}
	return []cli.McastStatus{st}, nil
}

// Bottlenecks is "show system bottlenecks" of this member (reference 3.5.2).
func (o *ops) Bottlenecks() ([]diag.Finding, error) {
	o.names.Refresh()
	var ports []diag.PortRef
	for _, p := range o.names.Ports() {
		ports = append(ports, diag.PortRef{Name: p.Name, Linux: p.Linux})
	}
	return diag.Analyze(o.diag.Collect(ports)), nil
}

// VXLAN is this member's VXLAN ports and how it reaches the remote VTEPs
// (reference 5.7).
func (o *ops) VXLAN() ([]cli.VXLANStatus, error) {
	cfg := o.model()
	st := cli.VXLANStatus{Member: o.member, Source: cfg.Switch.VTEPSource}
	if st.Source == "" {
		return []cli.VXLANStatus{st}, nil
	}
	remotes := dataplane.VXLANRemotes(cfg)
	for _, name := range slices.Sorted(maps.Keys(cfg.VLANs)) {
		v := cfg.VLANs[name]
		if v.VNI == 0 {
			continue
		}
		n := dataplane.VXLANName(v.VNI)
		p := cli.VXLANPort{VNI: v.VNI, VLAN: v.ID, Port: n}
		for _, r := range remotes[n] {
			p.Remotes = append(p.Remotes, r.String())
		}
		if l, err := netlink.LinkByName(n); err == nil {
			p.Up = l.Attrs().Flags&net.FlagUp != 0
			if s := l.Attrs().Statistics; s != nil {
				p.RxPackets, p.TxPackets = s.RxPackets, s.TxPackets
			}
			if ns, err := netlink.NeighList(l.Attrs().Index, unix.AF_BRIDGE); err == nil {
				for _, e := range ns {
					if e.MasterIndex != 0 && e.Vlan != 0 && e.State&netlink.NUD_PERMANENT == 0 {
						p.RemoteMACs++
					}
				}
			}
		}
		st.Ports = append(st.Ports, p)
	}
	for _, r := range slices.Sorted(maps.Keys(cfg.Switch.RemoteVTEPs)) {
		ip := net.ParseIP(r)
		vr := cli.VTEPRoute{VTEP: r}
		// As the VXLAN ports send: from the stack's VTEP address (an
		// unspecified source follows the management origin rules).
		rs, err := netlink.RouteGetWithOptions(ip, &netlink.RouteGetOptions{SrcAddr: net.ParseIP(st.Source)})
		if err != nil || len(rs) == 0 || rs[0].Type == unix.RTN_UNREACHABLE {
			vr.NoRoute = true
		} else {
			if rs[0].Gw != nil {
				vr.Via = rs[0].Gw.String()
			}
			if l, err := netlink.LinkByIndex(rs[0].LinkIndex); err == nil {
				vr.Interface = l.Attrs().Name
				if n, ok := o.names.Name(vr.Interface); ok {
					vr.Interface = n
				}
			}
		}
		st.Routes = append(st.Routes, vr)
	}
	return []cli.VXLANStatus{st}, nil
}

// vxlanVTEPs maps "<vni> <mac>" to the remote VTEP of the VXLAN ports'
// own tables.
func vxlanVTEPs() map[string]string {
	out := map[string]string{}
	links, err := netlink.LinkList()
	if err != nil {
		return out
	}
	for _, l := range links {
		vni := dataplane.VXLANVNI(l.Attrs().Name)
		if vni == 0 {
			continue
		}
		ns, _ := netlink.NeighList(l.Attrs().Index, unix.AF_BRIDGE)
		for _, n := range ns {
			if n.MasterIndex == 0 && n.IP != nil && len(n.HardwareAddr) == 6 {
				out[fmt.Sprintf("%d %s", vni, n.HardwareAddr)] = n.IP.String()
			}
		}
	}
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
