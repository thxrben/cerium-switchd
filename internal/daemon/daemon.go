// Package daemon wires switchd together: configuration engine, CLI server
// and (later) the data plane.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"log/slog"
	"mclag/internal/config"
	"mclag/internal/osconf"
	"mclag/internal/stack"
	"mclag/internal/stack/control"
	"mclag/internal/stack/pki"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"mclag/internal/access"
	"mclag/internal/cli"
	"mclag/internal/commit"
	"mclag/internal/dataplane"
	"mclag/internal/dhcp"
	"mclag/internal/inventory"
	"mclag/internal/lacp"
	"mclag/internal/lldp"
	"mclag/internal/model"
	"mclag/internal/ntp"
	"mclag/internal/rpc"
	"mclag/internal/software"
	"mclag/internal/syslog"
	"mclag/internal/version"
)

// Options configure the daemon.
type Options struct {
	StateDir string // persistent state (revisions)
	Socket   string // CLI socket
	// DryRun applies nothing to the kernel; changes are only logged.
	DryRun bool
	Log    *slog.Logger
}

// Run serves until ctx is cancelled.
func Run(ctx context.Context, o Options) error {
	// restart ends Run so that systemd starts switchd again (after joining a
	// virtual chassis, the member id changes).
	ctx, restart := context.WithCancel(ctx)
	defer restart()
	// When the machine shuts down, the member drains before switchd stops
	// (a reboot from the shell or a scheduled one).
	var maint atomic.Pointer[maintCtl]
	outer := ctx
	ctx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRun()
	go func() {
		select {
		case <-outer.Done():
			if m := maint.Load(); m != nil && systemStopping() {
				m.drainForShutdown("the system stops")
			}
			stopRun()
		case <-ctx.Done():
		}
	}()
	// Every record goes to the local buffer, the remote syslog servers and
	// the journal (stderr).
	hub := syslog.NewHub(o.Log.Handler(), 5000)
	defer hub.Close()
	log := slog.New(hub.Handler())
	// A new version that does not come up returns to the previous one
	// (reference 3.6).
	inst := &software.Installer{StateFile: filepath.Join(o.StateDir, "software.json")}
	if exe, err := os.Executable(); err == nil {
		inst.Program = exe
	}
	if !o.DryRun {
		if back, err := inst.Start(version.Version); err != nil {
			log.Error("software", "err", err)
		} else if back {
			log.Error("software: "+inst.Load().Note+"; starting the previous version", "facility", "change-log")
			return errors.New("returning to the previous version")
		}
	}
	store, err := commit.OpenFileStore(filepath.Join(o.StateDir, "config"), 50)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	srv := &rpc.Server{Log: log}
	kernel := &dataplane.Netlink{StateDir: o.StateDir}
	// The stack identity decides this switch's member id (interface names
	// <member>/<card>/<port>).
	var hostName func() string
	var engine *commit.Engine
	vc := &stack.Manager{Dir: filepath.Join(o.StateDir, "stack"), Log: log,
		HostName: func() string { return hostName() },
		ActiveConfig: func() json.RawMessage {
			raw, _ := json.Marshal(config.ToJSON(engine.Active().Root))
			return raw
		},
		OnJoined: func(member int, cfg json.RawMessage) error {
			if err := replaceConfig(o.StateDir, member, cfg); err != nil {
				return err
			}
			log.Warn("joined a virtual chassis; switchd restarts as member "+strconv.Itoa(member), "facility", "change-log")
			go func() {
				time.Sleep(time.Second) // let the CLI answer first
				restart()
			}()
			return nil
		}}
	member := 1
	if !o.DryRun {
		if err := vc.Load(); err != nil {
			log.Error("stack keys", "err", err)
		} else {
			member = vc.Member()
			kernel.GatewayMAC = dataplane.GatewayMAC(vc.StackID())
			kernel.MemberMAC = dataplane.MemberMAC(vc.StackID(), member)
		}
	}
	names := &inventory.Naming{SysRoot: "/sys", StateFile: filepath.Join(o.StateDir, "port-numbers.json"), Member: member}
	if _, err := names.Refresh(); err != nil {
		log.Warn("port numbering", "err", err)
	}
	vc.Linux = func(local string) (string, bool) { return names.Linux(strconv.Itoa(member) + "/" + local) }
	inv := &kernelInventory{kernel: kernel, names: names, member: member, vc: vc}
	applier := newKernelApplier(kernel, o.StateDir, o.DryRun, log)
	applier.member = member
	applier.inv, applier.names = inv, names
	// family inet dhcp (reference 5.3.2): the clients follow the data
	// plane's interfaces, a lease change reconciles it.
	dhcpMgr := &dhcp.Manager{Log: log, HostName: func() string {
		if hostName != nil {
			return hostName()
		}
		return ""
	}, OnChange: func() { applier.reconcile("dhcp lease") }}
	if !o.DryRun {
		kernel.DHCP = func(ifs []dataplane.DHCPIf) map[string]dataplane.DHCPLease {
			var want []dhcp.Iface
			for _, i := range ifs {
				want = append(want, dhcp.Iface{Name: i.Name, Unit: i.Unit, VRF: i.VRF})
			}
			dhcpMgr.Sync(want)
			out := map[string]dataplane.DHCPLease{}
			for n, l := range dhcpMgr.Leases() {
				out[n] = dataplane.DHCPLease{Addr: l.Addr, Router: l.Router}
			}
			return out
		}
	}
	accounts := &access.Manager{Sys: &access.OS{}, StateFile: filepath.Join(o.StateDir, "accounts.json"), Log: log}
	systemctl := func(args ...string) error { return command("systemctl", args...) }
	consoles := &access.Consoles{SysRoot: "/sys", UnitDir: "/etc/systemd/system", ProfileDir: "/etc/profile.d",
		StateFile: filepath.Join(o.StateDir, "consoles.json"), Log: log, Systemctl: systemctl, MainComm: access.SystemdMainComm}
	sshd := &access.SSH{Dir: "/etc/switchd", UnitPath: "/etc/systemd/system/switchd-sshd.service",
		LegacyDropIn: "/etc/ssh/sshd_config.d/switchd.conf", ProcNet: "/proc/net", Log: log, Run: command}
	osHost := &osconf.Host{StateDir: o.StateDir, Log: log, Hostname: os.Hostname,
		SetHostname: func(n string) error { return unix.Sethostname([]byte(n)) }}
	var mclagRef atomic.Pointer[mclagCtl]
	lacpRT := &lacp.Runtime{Kernel: teamKernel{}, StateFile: filepath.Join(o.StateDir, "lacp.json"), Log: log,
		BeforeLeave: func(b string) {
			if m := mclagRef.Load(); m != nil {
				m.beforeLeave(b)
			}
		}}
	sysMAC := lacpSystemMAC(vc.StackID())
	ntpClient := &ntp.Client{Clock: ntp.SystemClock{}, Log: log}
	var mclag *mclagCtl // set once the stack control runs
	var stp *rstpCtl
	lldpAgent := &lldp.Agent{Log: log, Carrier: dataplane.Carrier, Aggregated: func(linux string) bool {
		// LACP bundles: LACP has the port in; static bundles: it has a link.
		if on, known := lacpRT.PortEnabled(linux); known {
			return on
		}
		return dataplane.Carrier(linux)
	}}
	chassisMAC := dataplane.ChassisMAC(vc.StackID())
	applier.afterApply = func(cfg *model.Config) {
		// LACP bundles: after the data plane created their devices.
		lacpRT.Sync(lacpSpecs(cfg, member, names.Linux, sysMAC))
		if !o.DryRun {
			lldpAgent.Sync(lldpConfig(cfg, member, names, chassisMAC, vc.IsPort))
		}
		if mclag != nil {
			mclag.setConfig(cfg)
		}
		if stp != nil {
			stp.setConfig(cfg)
		}
	}
	// The management services run on the master (reference 1.8).
	mgmt := &mgmtCtl{member: member, log: log, hub: hub, ntp: ntpClient, sshd: sshd, clock: ntp.SystemClock{}, dryRun: o.DryRun}
	hub.Configure(nil, func() string {
		if hostName != nil {
			return hostName()
		}
		h, _ := os.Hostname()
		return h
	}, 0)
	applier.isMaster = mgmt.master
	applier.stackPort = vc.IsPort
	if !o.DryRun {
		applier.cmeMAC = dataplane.CMEMAC(vc.StackID())
	}
	applier.onApplied = func(cfg *model.Config) {
		mgmt.sync(cfg)
		if !o.DryRun {
			if err := accounts.Sync(cfg); err != nil {
				log.Error("accounts", "facility", "authorization", "err", err)
			}
			if err := consoles.Sync(cfg); err != nil {
				log.Error("consoles", "err", err)
			}
			if err := osHost.Sync(cfg, member); err != nil {
				log.Error("host name / resolver", "err", err)
			}
		}
	}
	// The stack control replicates the configuration store; without it
	// (dry run, or it cannot start) the store is local.
	var ctl *stackCtl
	engOpts := commit.Options{
		Store: store, Applier: applier, Inventory: inv, Notify: srv.Notify, Log: log,
		Upgrade: newUpgrader(names, 1, log).Upgrade,
		Checks:  []func(*model.Config) model.Issues{accounts.Check, sshd.Check, checkNTP},
	}
	if !o.DryRun && vc.Mesh() != nil {
		if ctl = startControl(o.StateDir, store, vc, applier, member, log, restart); ctl != nil {
			defer ctl.node.Close()
			ctl.inv, ctl.checks = inv, engOpts.Checks
			engOpts.Store, engOpts.Applier, engOpts.Writable = ctl.node.EngineStore(), ctl, ctl.writable
			engOpts.StackCheck = ctl.stackCheck
		}
	}
	engine, err = commit.New(engOpts)
	if err != nil {
		return err
	}
	defer engine.Close()
	if ctl != nil {
		ctl.setEngine(engine)
		mgmt.ctl = ctl
		ctl.onLeader = func(bool) {
			// The management address and services follow mastership.
			go func() {
				applier.reconcile("mastership")
				mgmt.sync(nil)
			}()
		}
		go ctl.run(ctx)
	}
	mgmt.start(ctx)
	if !o.DryRun {
		mclag = newMCLAG(member, lacpRT, ctl, log)
		mclagRef.Store(mclag)
		if cfg, _ := model.Build(engine.Active().Active(), nil); cfg != nil {
			mclag.setConfig(cfg)
		}
		go mclag.run(ctx)
		var node *control.Node
		if ctl != nil {
			node = ctl.node
		}
		stp = newRSTP(member, node, vc.Mesh(), names, vc.StackID, o.StateDir, log)
		stp.legs = lacpRT.Legs
		if cfg, _ := model.Build(engine.Active().Active(), nil); cfg != nil {
			stp.setConfig(cfg)
		}
		go stp.run(ctx)
	}
	engine.Start(ctx)
	go applier.watch(ctx)
	if !o.DryRun {
		go lacpRT.Run(ctx)
		go lldpAgent.Run(ctx)
	}
	if !o.DryRun {
		go kernel.EnforceMACLimits(ctx, log)
	}

	hostName = func() string {
		root := engine.Active().Active().Root
		if h := root.Leaf("virtual-chassis", "member", strconv.Itoa(member), "host-name"); h != "" {
			return h
		}
		if h := root.Leaf("system", "host-name"); h != "" {
			return h
		}
		if h, err := os.Hostname(); err == nil {
			return h
		}
		return "switch"
	}
	localPorts := func() []string {
		var out []string
		for _, p := range names.Ports() {
			out = append(out, p.Name)
		}
		return out
	}
	ports := localPorts
	if ctl != nil {
		ctl.servePorts(localPorts)
		ports = func() []string { return append(localPorts(), ctl.remotePorts()...) }
	}
	liveOps := &ops{restart: restart, kernel: kernel, engine: engine, names: names, member: member, vc: vc, hostName: hostName, started: time.Now(), log: log, dryRun: o.DryRun,
		notify: func(m string) { srv.Notify(context.Background(), m) }}
	if !o.DryRun {
		liveOps.lacp, liveOps.mclag = lacpRT, mclag
		liveOps.lldp = lldpAgent
		liveOps.ntp = ntpClient
		var node *control.Node
		if ctl != nil {
			node = ctl.node
		}
		liveOps.maint = newMaint(o.StateDir, member, vc.Mesh(), node, mclag, liveOps.model, log)
		maint.Store(liveOps.maint)
		liveOps.stp = stp
		liveOps.dhcp = dhcpMgr
		upd := &updater{member: member, dir: filepath.Join(o.StateDir, "software"), inst: inst, vc: vc, ctl: ctl, log: log, restart: restart,
			engine: func() *commit.Engine { return engine }, maint: func() *maintCtl { return liveOps.maint },
			mgmtVRF: func() string {
				if cfg, _ := model.Build(engine.Active().Active(), nil); cfg != nil {
					return cfg.System.MgmtInstance
				}
				return ""
			}}
		liveOps.updater = upd
		upd.start(ctx)
		// Healthy: the configuration is applied and the stack state is
		// current; a pending update is done then.
		go func() {
			for ctx.Err() == nil {
				applier.mu.Lock()
				applied := applier.last != nil
				applier.mu.Unlock()
				if applied && (ctl == nil || ctl.node.Current()) {
					upd.healthy()
					return
				}
				time.Sleep(2 * time.Second)
			}
		}()
	}
	// Stacking sessions start once everything they use (host name, active
	// configuration) is set up.
	if !o.DryRun {
		if pki.KeyLogging() {
			log.Warn("stack: the session keys of stacking TLS sessions are written to $"+pki.KeyLogEnv+" (debugging)", "file", os.Getenv(pki.KeyLogEnv))
		}
		if err := vc.Start(ctx); err != nil {
			log.Error("stack", "err", err)
		} else {
			go runStackNet(ctx, vc, func() int {
				cfg, _ := model.Build(engine.Active().Active(), nil)
				if cfg == nil || len(cfg.SwitchMembers()) < 2 {
					return 0
				}
				mtu, _ := cfg.MaxDataMTU()
				return mtu + model.StackOverhead
			}, log)
		}
	}
	srv.Env = func(name string, class commit.Class) cli.Env {
		env := cli.Env{Engine: engine, User: name, Class: class, Version: version.Version, Built: version.Date,
			HostName: hostName, Ports: ports, Ops: liveOps, Logs: logs{hub}, Log: log}
		if ctl != nil {
			env.Role = ctl.role
			env.Stack = sessionStack{s: ctl, user: name, class: class}
			// The members work as one switch: listings cover them all.
			env.Ops = &stackOps{ops: liveOps, ctl: ctl}
		}
		return env
	}
	if ctl != nil {
		ctl.serveOps(liveOps)
		ctl.serveExec(func(name string, class commit.Class) cli.Env {
			// A command run for another member reports this member only.
			e := srv.Env(name, class)
			e.Ops = liveOps
			return e
		})
		srv.Synced = ctl.synced
		// Every CLI session runs on the master (reference 1.8).
		srv.Member = member
		srv.Relay = func() (net.Conn, error) {
			// An election (e.g. after a stacking cable failed) takes a few
			// seconds at most.
			deadline := time.Now().Add(3 * time.Second)
			for {
				if ctl.node.MasterReady() {
					return nil, nil
				}
				if m := ctl.node.Master(); m != 0 && m != member {
					return vc.Mesh().Dial(m, "cli", 5*time.Second)
				}
				if time.Now().After(deadline) {
					return nil, fmt.Errorf("%w", ctl.writable())
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		cliL := vc.Mesh().Listen("cli")
		go func() {
			<-ctx.Done()
			cliL.Close()
		}()
		go srv.ServeRemote(cliL)
	}
	srv.Authorize = func(uid int, name string) (commit.Class, error) {
		if uid == 0 {
			return commit.SuperUser, nil
		}
		u := engine.Active().Active().Root.Get("system", "login", "user", name)
		if u == nil {
			return 0, fmt.Errorf("user %s is not configured in 'system login'", name)
		}
		return commit.ParseClass(u.Leaf("class")), nil
	}

	l, err := listen(o.Socket)
	if err != nil {
		return err
	}
	if !o.DryRun {
		sh := &shells{member: member, vc: vc, authorize: srv.Authorize, log: log}
		go func() {
			if err := sh.run(ctx, o.Socket); err != nil {
				log.Error("shell socket", "err", err)
			}
		}()
	}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	log.Info("switchd started", "version", version.Version, "dry_run", o.DryRun, "socket", o.Socket)
	err = srv.Serve(l)
	log.Info("switchd stopped")
	return err
}

// listen creates the CLI socket. Every local user may connect; switchd
// authorises each connection by its kernel credentials.
func listen(path string) (*net.UnixListener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// command runs a system tool and returns its output in the error.
func command(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// replaceConfig makes the stack's configuration this member's configuration
// after joining: the previous one is kept in config.pre-join-<time>.
func replaceConfig(stateDir string, member int, cfg json.RawMessage) error {
	return replaceConfigAs(stateDir, "config.pre-join-", fmt.Sprintf("joined the virtual chassis as member %d", member), cfg)
}

// replaceConfigAs keeps the old configuration store as <stateDir>/<backup><time>
// and starts a new one with cfg as its first revision.
func replaceConfigAs(stateDir, backup, comment string, cfg json.RawMessage) error {
	dir := filepath.Join(stateDir, "config")
	keep := filepath.Join(stateDir, backup+time.Now().UTC().Format("20060102T150405"))
	if err := os.Rename(dir, keep); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	st, err := commit.OpenFileStore(dir, 50)
	if err != nil {
		return err
	}
	tree := config.New()
	if len(cfg) > 0 {
		if tree, err = config.FromJSON(cfg); err != nil {
			return fmt.Errorf("configuration from the stack: %w", err)
		}
	}
	raw, _ := json.Marshal(config.ToJSON(tree.Root))
	return st.Put(&commit.Revision{Seq: 1, Time: time.Now().UTC(), User: "system",
		Comment: comment, Config: raw}, 0)
}
