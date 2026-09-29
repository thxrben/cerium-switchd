// Package daemon wires switchd together: configuration engine, CLI server
// and (later) the data plane.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"log/slog"
	"mclag/internal/osconf"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mclag/internal/access"
	"mclag/internal/cli"
	"mclag/internal/commit"
	"mclag/internal/dataplane"
	"mclag/internal/inventory"
	"mclag/internal/model"
	"mclag/internal/rpc"
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
	// Every record goes to the local buffer, the remote syslog servers and
	// the journal (stderr).
	hub := syslog.NewHub(o.Log.Handler(), 5000)
	hub.VRF = dataplane.MgmtVRF
	defer hub.Close()
	log := slog.New(hub.Handler())
	store, err := commit.OpenFileStore(filepath.Join(o.StateDir, "config"), 50)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	srv := &rpc.Server{Log: log}
	kernel := &dataplane.Netlink{}
	names := &inventory.Naming{SysRoot: "/sys", StateFile: filepath.Join(o.StateDir, "port-numbers.json"), Member: 1}
	if _, err := names.Refresh(); err != nil {
		log.Warn("port numbering", "err", err)
	}
	inv := &kernelInventory{kernel: kernel, names: names, member: 1}
	applier := newKernelApplier(kernel, o.StateDir, o.DryRun, log)
	applier.inv, applier.names = inv, names
	var hostName func() string
	accounts := &access.Manager{Sys: &access.OS{}, StateFile: filepath.Join(o.StateDir, "accounts.json"), Log: log}
	systemctl := func(args ...string) error { return command("systemctl", args...) }
	consoles := &access.Consoles{SysRoot: "/sys", UnitDir: "/etc/systemd/system", ProfileDir: "/etc/profile.d",
		StateFile: filepath.Join(o.StateDir, "consoles.json"), Log: log, Systemctl: systemctl, MainComm: access.SystemdMainComm}
	sshd := &access.SSH{Dir: "/etc/switchd", UnitPath: "/etc/systemd/system/switchd-sshd.service",
		LegacyDropIn: "/etc/ssh/sshd_config.d/switchd.conf", ProcNet: "/proc/net", Log: log, Run: command}
	osHost := &osconf.Host{StateDir: o.StateDir, Log: log, Hostname: os.Hostname,
		SetHostname: func(n string) error { return unix.Sethostname([]byte(n)) }}
	applier.onApplied = func(cfg *model.Config) {
		hub.Configure(syslogHosts(cfg), hostName, cfg.System.LogBuffer)
		if !o.DryRun {
			if err := accounts.Sync(cfg); err != nil {
				log.Error("accounts", "facility", "authorization", "err", err)
			}
			if err := consoles.Sync(cfg); err != nil {
				log.Error("consoles", "err", err)
			}
			if err := sshd.Sync(cfg); err != nil {
				log.Error("ssh", "err", err)
			}
			if err := osHost.Sync(cfg, 1); err != nil {
				log.Error("host name / resolver", "err", err)
			}
		}
	}
	engine, err := commit.New(commit.Options{
		Store: store, Applier: applier, Inventory: inv, Notify: srv.Notify, Log: log,
		Upgrade: upgradeNames(names, 1),
		Checks:  []func(*model.Config) model.Issues{accounts.Check, sshd.Check},
	})
	if err != nil {
		return err
	}
	defer engine.Close()
	engine.Start(ctx)
	go applier.watch(ctx)
	if !o.DryRun {
		go kernel.EnforceMACLimits(ctx, log)
	}

	hostName = func() string {
		root := engine.Active().Active().Root
		if h := root.Leaf("stack", "member", "1", "host-name"); h != "" {
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
	ports := func() []string {
		var out []string
		for _, p := range names.Ports() {
			out = append(out, p.Name)
		}
		return out
	}
	liveOps := &ops{kernel: kernel, engine: engine, names: names, member: 1, started: time.Now(), log: log, dryRun: o.DryRun,
		notify: func(m string) { srv.Notify(context.Background(), m) }}
	srv.Env = func(name string, class commit.Class) cli.Env {
		return cli.Env{Engine: engine, User: name, Class: class, Version: version.Version,
			HostName: hostName, Ports: ports, Ops: liveOps, Logs: logs{hub}, Log: log}
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
