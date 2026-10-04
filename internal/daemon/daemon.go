// Package daemon wires switchd together: configuration engine, CLI server
// and (later) the data plane.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/osconf"
	"github.com/thxrben/cerium-switchd/internal/stack"
	"github.com/thxrben/cerium-switchd/internal/stack/control"
	"github.com/thxrben/cerium-switchd/internal/stack/pki"
	"github.com/thxrben/cerium-switchd/internal/stp"

	"golang.org/x/sys/unix"

	"github.com/thxrben/cerium-switchd/internal/access"
	"github.com/thxrben/cerium-switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/dataplane"
	"github.com/thxrben/cerium-switchd/internal/inventory"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/rpcserver"
	"github.com/thxrben/cerium-switchd/internal/supervise"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/internal/version"
	"github.com/thxrben/cerium-switchd/packaging"
	"github.com/thxrben/cerium-switchd/pkg/dhcp"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"github.com/thxrben/cerium-switchd/pkg/lldp"
	"github.com/thxrben/cerium-switchd/pkg/nlx"
	"github.com/thxrben/cerium-switchd/pkg/sdnotify"
	"github.com/thxrben/cerium-switchd/pkg/sysexec"
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
	// When switchd is stopped (the system shuts down, or its unit is
	// stopped), it stops the daemons in order after draining (reference
	// 1.9); a restart of switchd leaves them running.
	var supRef atomic.Pointer[supervise.Supervisor]
	outer := ctx
	ctx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRun()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-outer.Done():
			stopping := systemStopping()
			if m := maint.Load(); m != nil && stopping {
				m.drainForShutdown("the system stops")
			}
			if sup := supRef.Load(); sup != nil && (stopping || unitStopping("switchd.service")) {
				o.Log.Info("switchd stops: stopping the daemons")
				sctx, cancel := context.WithTimeout(context.Background(), daemonsStopTime)
				sup.Shutdown(sctx)
				cancel()
			}
			stopRun()
		case <-ctx.Done():
		}
	}()
	defer func() {
		if outer.Err() != nil {
			<-shutdownDone
		}
	}()
	// Every record goes to the journal; cer-syslogd keeps the buffer for
	// show log and forwards (reference 1.9).
	log := o.Log
	exe, _ := os.Executable()
	// Kernel requests and disk access have deadlines (hwio); the netlink
	// sockets as well.
	nlx.SetSocketTimeout()
	// switchd's loops feed the systemd watchdog: a loop that hangs (a bug,
	// a deadlock) gets switchd restarted, the daemons keep running. A
	// hanging device does not: its calls fail at their deadline (alarm).
	live := &sdnotify.Liveness{Max: loopMax}
	go live.Run(ctx, func(late []string) {
		if len(late) > 0 {
			log.Error("switchd: loops make no progress; the watchdog restarts switchd", "loops", strings.Join(late, ", "))
		}
	})
	if !o.DryRun {
		// The operating system's network configuration is switchd's
		// (reference 1.4), and its unit is the one this version brings.
		// Only an installed switchd that systemd started: a program run by
		// hand (a test build) must not become the unit's program.
		if _, err := hwio.Stat(unitPath); err == nil && os.Getenv("INVOCATION_ID") != "" {
			reload := func() error { return command("systemctl", "daemon-reload") }
			ensureUnit(unitPath, packaging.Unit, exe, reload, log)
			// The update daemon (reference 3.6) runs beside switchd.
			ensureUnit(updateUnitPath, updateUnit(exe), exe, reload, log)
			if state, _ := systemctlOutput("is-active", "switchd-update.service"); strings.TrimSpace(state) != "active" {
				if err := command("systemctl", "enable", "--now", "switchd-update.service"); err != nil {
					log.Warn("update daemon: not started", "err", err)
				}
			}
		}
		takeOverOSNetwork(systemctlOutput, "/proc", log)
	}
	store, err := commit.OpenFileStore(filepath.Join(o.StateDir, "config"), 50)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	srv := &rpcserver.Server{Log: log}
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
	applier.live = live
	applier.inv, applier.names = inv, names
	// The cer- daemons' service (reference 1.9): it exists from the start,
	// so that the data plane's first apply can use it; it serves once
	// switchd is set up (below).
	services := newService(member, nil, nil, nil, log)
	// family inet dhcp (reference 5.3.2): cer-dhcpcd runs the clients of the
	// data plane's DHCP interfaces and reports the leases; switchd adds the
	// addresses and default routes. A lease change reconciles.
	dhcpLeases := services.follow("cer-dhcpcd", svc.TopicLeases, func(map[string]json.RawMessage) {
		go applier.reconcile("dhcp lease")
	})
	// bpdu-block (reference 5.5): cer-rstpd decides, switchd keeps the
	// blocked ports down.
	bpduBlocked := services.follow("cer-rstpd", stp.TopicBPDUBlocked, func(map[string]json.RawMessage) {
		go applier.reconcile("bpdu-block")
	})
	applier.blocked = func() map[string]bool {
		out := map[string]bool{}
		for n := range bpduBlocked.State() {
			out[n] = true
		}
		return out
	}
	started := time.Now()
	if !o.DryRun {
		kernel.DHCP = func(ifs []dataplane.DHCPIf) map[string]dataplane.DHCPLease {
			var want []dhcp.Iface
			for _, i := range ifs {
				want = append(want, dhcp.Iface{Name: i.Name, Unit: i.Unit, VRF: i.VRF})
			}
			host := ""
			if hostName != nil {
				host = hostName()
			}
			services.setConfig("cer-dhcpcd", dhcp.Config{Ifaces: want, HostName: host})
			// After a start of switchd the daemon still holds its leases:
			// wait for them (briefly) rather than apply without them, which
			// would remove the addresses.
			if wait := 5*time.Second - time.Since(started); len(want) > 0 && wait > 0 {
				select {
				case <-dhcpLeases.Ready():
				case <-time.After(wait):
				}
			}
			return dhcpLeasesOf(dhcpLeases.State())
		}
	}
	accounts := &access.Manager{Sys: &access.OS{}, StateFile: filepath.Join(o.StateDir, "accounts.json"), Log: log}
	systemctl := func(args ...string) error { return command("systemctl", args...) }
	consoles := &access.Consoles{SysRoot: "/sys", UnitDir: "/etc/systemd/system", ProfileDir: "/etc/profile.d",
		StateFile: filepath.Join(o.StateDir, "consoles.json"), Log: log, Systemctl: systemctl, MainComm: access.SystemdMainComm}
	sshd := &access.SSH{Dir: "/etc/switchd", UnitPath: "/etc/systemd/system/switchd-sshd.service",
		LegacyDropIn: "/etc/ssh/sshd_config.d/switchd.conf", ProcNet: "/proc/net", PrivsepDir: "/run/sshd", Log: log, Run: command}
	osHost := &osconf.Host{StateDir: o.StateDir, Log: log, Hostname: os.Hostname,
		SetHostname: func(n string) error { return unix.Sethostname([]byte(n)) }}
	// LACP runs in cer-lacpd, MC-LAG in cer-mclagd (reference 1.9); switchd
	// passes LACP's port states on to cer-lldpd.
	republishLACPPorts(services)
	mclag := mclagClient{services}
	sysMAC := lacpSystemMAC(vc.StackID())
	chassisMAC := dataplane.ChassisMAC(vc.StackID())
	applier.afterApply = func(cfg *model.Config) {
		setTimeouts(cfg.System.Timeouts) // reference 5.1
		// Routing (reference 5.8): cer-ribd has the routing table and
		// installs every route; the management instance only on the master
		// (after mastership changes, a reconcile runs).
		if !o.DryRun {
			services.setConfig("cer-ribd", ribConfig(cfg, member, names.Linux, applier.master(), dhcpLeasesOf(dhcpLeases.State())))
		}
		// LACP bundles: after the data plane created their devices.
		if !o.DryRun {
			services.setConfig("cer-lacpd", svc.LACPConfig{Bundles: lacpSpecs(cfg, member, names.Linux, sysMAC), Hooks: "cer-mclagd"})
		}
		if !o.DryRun {
			sys, ports := lldpConfig(cfg, member, names, chassisMAC, vc.IsPort)
			services.setConfig("cer-lldpd", lldp.Config{System: sys, Ports: ports})
		}
		if !o.DryRun {
			services.setConfig("cer-mclagd", mclagConfig(cfg, member))
		}
		if !o.DryRun {
			services.setConfig("cer-rstpd", stpConfig(cfg, member, names.Linux, vc.StackID()))
		}
		// OSPF and OSPFv3 (reference 5.13): cer-ospfd runs where configured
		// (the protocol itself on the master only).
		if !o.DryRun {
			services.setConfig("cer-ospfd", ospfConfig(cfg, names.Linux))
		}
		// BGP (reference 5.14): cer-bgpd runs where configured (the
		// protocol on the master only).
		if !o.DryRun {
			services.setConfig("cer-bgpd", bgpConfig(cfg))
		}
	}
	// The management services run on the master (reference 1.8).
	mgmt := &mgmtCtl{member: member, log: log, sshd: sshd, dryRun: o.DryRun,
		publish: func(daemon string, v any) {
			services.setConfig(daemon, v)
		}}
	applier.isMaster = mgmt.master
	applier.gatewayMAC = kernel.GatewayMAC
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
		ctl.live = live
		applier.masterID = ctl.node.Master
		go ctl.run(ctx)
		// A new master: the protocol frames for the irbs go to it at once
		// (every member, not only the old and the new master).
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			last := ctl.node.Master()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				if m := ctl.node.Master(); m != last {
					last = m
					applier.reconcile("master changed")
				}
			}
		}()
	}
	// Notices for the CLI sessions of the whole stack: the sessions run on
	// the master (reference 1.8).
	notifyStack := func(text string) {
		if ctl != nil && !ctl.node.IsMaster() {
			if m := ctl.node.Master(); m != 0 {
				if _, err := ctl.node.Call(m, "notice", text, 5*time.Second); err == nil {
					return
				}
			}
		}
		srv.Notify(context.Background(), text)
	}
	watchHangs(member, log, notifyStack)
	if ctl != nil {
		ctl.node.Handle("notice", func(_ int, req json.RawMessage) (any, error) {
			var text string
			if err := json.Unmarshal(req, &text); err != nil {
				return nil, err
			}
			srv.Notify(context.Background(), text)
			return nil, nil
		})
	}
	// The cer- daemons (reference 1.9): switchd's service socket, and the
	// supervisor that starts and watches them (only for a switchd that
	// systemd runs: a program started by hand leaves the system alone).
	services.ctl, services.notify = ctl, notifyStack
	services.role = func() svc.Role {
		return roleOf(member, ctl, func() *model.Config {
			cfg, _ := model.Build(engine.Active().Active(), nil)
			return cfg
		}, hostName, vc.StackID())
	}
	var sup *supervise.Supervisor
	if !o.DryRun {
		if err := services.start(ctx, ""); err != nil {
			log.Error("service socket", "err", err)
		}
		if os.Getenv("INVOCATION_ID") != "" && exe != "" {
			sup = &supervise.Supervisor{Backend: &supervise.Systemd{UnitDir: "/etc/systemd/system"}, Log: log,
				Dir: filepath.Dir(exe), Args: []string{"-member", strconv.Itoa(member), "-state-dir", o.StateDir},
				Member: member, Notify: notifyStack, Wanted: func() map[string]bool { return wantedDaemons(engine) },
				Beat: func() { live.Beat("daemon supervisor") },
				// request daemon stop lasts until the reboot (/run is a tmpfs).
				StoppedFile: "/run/switchd/stopped-daemons"}
			supRef.Store(sup)
			go sup.Run(ctx)
		}
	}
	if !o.DryRun {
		if ctl != nil {
			// Remote MACs of VXLAN learned by one member, for all (5.7).
			go newVXLANSync(member, ctl, log).run(ctx)
		}
	}
	engine.Start(ctx)
	go applier.watch(ctx)
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
		liveOps.mclag = &mclag
		liveOps.svc = services
		var node *control.Node
		if ctl != nil {
			node = ctl.node
		}
		liveOps.maint = newMaint(o.StateDir, member, vc.Mesh(), node, mclag, liveOps.model, log)
		maint.Store(liveOps.maint)
		liveOps.sup = sup
		upd := &updater{member: member, dir: softwareDir, vc: vc, ctl: ctl, log: log,
			engine: func() *commit.Engine { return engine }, maint: func() *maintCtl { return liveOps.maint },
			mgmtVRF: func() string {
				if cfg, _ := model.Build(engine.Active().Active(), nil); cfg != nil {
					return cfg.System.MgmtInstance
				}
				return ""
			}}
		liveOps.updater = upd
		upd.start(ctx)
		upd.started()
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
			HostName: hostName, Ports: ports, Ops: liveOps, Logs: logs{services}, Log: log}
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
	if err := hwio.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := hwio.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := hwio.Chmod(path, 0o666); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// command runs a system tool and returns its output in the error.
func command(name string, args ...string) error {
	if _, err := sysexec.CombinedOutput(name, args...); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
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
	if err := hwio.Rename(dir, keep); err != nil && !errors.Is(err, os.ErrNotExist) {
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

// wantedDaemons reports which daemons that do not always run are needed by
// the active configuration (reference 1.9).
func wantedDaemons(engine *commit.Engine) map[string]bool {
	cfg, _ := model.Build(engine.Active().Active(), nil)
	return wantedBy(cfg)
}

// wantedBy reports the daemons a configuration needs that do not always
// run: the routing protocols that are configured, BFD when one of them
// uses it.
func wantedBy(cfg *model.Config) map[string]bool {
	out := map[string]bool{}
	if cfg == nil {
		return out
	}
	for _, r := range cfg.AllRouting() {
		for _, o := range []*model.OSPF{r.OSPF, r.OSPF3} {
			if o == nil || o.Disabled {
				continue
			}
			out["cer-ospfd"] = true
			for _, a := range o.Areas {
				for _, i := range a.Interfaces {
					if i.BFD != nil {
						out["cer-bfdd"] = true
					}
				}
			}
		}
		if r.BGP != nil && !r.BGP.Disabled {
			out["cer-bgpd"] = true
			for _, g := range r.BGP.Groups {
				for _, n := range g.Neighbors {
					if n.BFD != nil && !n.Disabled {
						out["cer-bfdd"] = true
					}
				}
			}
		}
	}
	return out
}

// dhcpLeasesOf converts cer-dhcpcd's leases for the data plane.
func dhcpLeasesOf(st map[string]json.RawMessage) map[string]dataplane.DHCPLease {
	out := map[string]dataplane.DHCPLease{}
	for dev, raw := range st {
		var l svc.Lease
		if json.Unmarshal(raw, &l) != nil {
			continue
		}
		p, err := netip.ParsePrefix(l.Addr)
		if err != nil {
			continue
		}
		dl := dataplane.DHCPLease{Addr: p}
		dl.Router, _ = netip.ParseAddr(l.Router)
		out[dev] = dl
	}
	return out
}
