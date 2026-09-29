// Package daemon wires switchd together: configuration engine, CLI server
// and (later) the data plane.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"mclag/internal/cli"
	"mclag/internal/commit"
	"mclag/internal/config"
	"mclag/internal/inventory"
	"mclag/internal/rpc"
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
	if !o.DryRun {
		return errors.New("the data plane is not implemented yet; run with --dry-run")
	}
	log := o.Log
	store, err := commit.OpenFileStore(filepath.Join(o.StateDir, "config"), 50)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	srv := &rpc.Server{Log: log}
	engine, err := commit.New(commit.Options{
		Store: store, Applier: &dryRunApplier{log: log}, Notify: srv.Notify, Log: log,
	})
	if err != nil {
		return err
	}
	defer engine.Close()
	engine.Start(ctx)

	hostName := func() string {
		if h := engine.Active().Active().Root.Leaf("system", "host-name"); h != "" {
			return h
		}
		if h, err := os.Hostname(); err == nil {
			return h
		}
		return "switch"
	}
	ports := func() []string {
		var out []string
		for _, p := range inventory.PhysicalPorts("/sys") {
			out = append(out, "1/"+p)
		}
		return out
	}
	srv.Env = func(name string, class commit.Class) cli.Env {
		return cli.Env{Engine: engine, User: name, Class: class, Version: version.Version,
			HostName: hostName, Ports: ports, Log: log}
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

// dryRunApplier logs what would change instead of touching the kernel.
type dryRunApplier struct{ log *slog.Logger }

func (a *dryRunApplier) Apply(_ context.Context, from, to *config.Tree) []commit.MemberResult {
	var diff string
	if from == nil {
		diff = config.FormatSet(to.Active())
	} else {
		diff = config.Diff(from.Active(), to.Active())
	}
	a.log.Info("dry-run apply", "changes", strings.Count(diff, "\n"), "diff", diff)
	return []commit.MemberResult{{Member: "member1"}}
}
