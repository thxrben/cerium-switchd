// Package updated is the update daemon, switchd-update (reference 3.6): it
// installs software on its member, restarts switchd and returns to the
// previous version when the new one does not become healthy. It runs as
// its own systemd unit, so a switchd that fails cannot stop its own
// rollback. switchd talks to it over a unix socket (one JSON request and
// one reply per connection).
package updated

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mclag/internal/software"
)

// DefaultSocket is where the daemon listens.
const DefaultSocket = "/run/switchd-update/sock"

// HealthTimeout: a new version that is not healthy this long after the
// restart is replaced by the previous one.
const HealthTimeout = 3 * time.Minute

// Request is what switchd sends.
type Request struct {
	Op string `json:"op"` // install, rollback, healthy, status
	// install: the verified program, staged by switchd.
	Program string `json:"program,omitempty"`
	Version string `json:"version,omitempty"`
	// Running: the version switchd runs now.
	Running         string `json:"running,omitempty"`
	ExitMaintenance bool   `json:"exit_maintenance,omitempty"`
}

// Reply is the daemon's answer.
type Reply struct {
	Err     string `json:"err,omitempty"`
	State   string `json:"state,omitempty"` // idle, restarting, waiting for switchd <v>, ...
	Version string `json:"version,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Daemon is the update daemon of one member.
type Daemon struct {
	Inst    *software.Installer
	Socket  string
	Version string // the daemon's own version
	// Restart restarts switchd (systemctl restart switchd).
	Restart func() error
	Log     *slog.Logger
	Timeout time.Duration // HealthTimeout if 0
	// Exit ends the daemon (it restarts onto the new program).
	Exit func()

	mu      sync.Mutex
	state   string
	waiting string        // version waited for ("": none)
	healthy chan struct{} // closed when it is healthy
}

func (d *Daemon) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return HealthTimeout
}

func (d *Daemon) setState(s string) {
	d.mu.Lock()
	d.state = s
	d.mu.Unlock()
	d.Log.Info("switchd-update: " + s)
}

// Run serves until ctx ends. A pending update it ran before a restart of
// its own is watched again (with the time already passed).
func (d *Daemon) Run(ctx context.Context) error {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	d.state = "idle"
	if p := d.Inst.Load().Pending; p != nil && p.Daemon {
		left := time.Until(p.Since.Add(d.timeout()))
		go d.watch(ctx, p.Version, max(left, 10*time.Second), false)
	}
	if err := os.MkdirAll(filepath.Dir(d.Socket), 0o700); err != nil {
		return err
	}
	os.Remove(d.Socket)
	l, err := net.Listen("unix", d.Socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(d.Socket, 0o600); err != nil { // root (switchd) only
		l.Close()
		return err
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.serve(ctx, c)
	}
}

func (d *Daemon) serve(ctx context.Context, c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var r Request
	rep := Reply{}
	if err := json.Unmarshal(line, &r); err != nil {
		rep.Err = err.Error()
	} else if err := d.handle(ctx, r, &rep); err != nil {
		rep.Err = err.Error()
	}
	raw, _ := json.Marshal(rep)
	c.Write(append(raw, '\n'))
}

func (d *Daemon) handle(ctx context.Context, r Request, rep *Reply) error {
	switch r.Op {
	case "status":
	case "healthy":
		d.mu.Lock()
		if d.waiting != "" && d.waiting == r.Version && d.healthy != nil {
			close(d.healthy)
			d.healthy = nil
		}
		d.mu.Unlock()
		if p := d.Inst.Load().Pending; p != nil && p.Version == r.Version {
			if _, err := d.Inst.Healthy(r.Version); err != nil {
				return err
			}
		}
	case "install", "rollback":
		d.mu.Lock()
		busy := d.waiting != ""
		d.mu.Unlock()
		if busy {
			return errors.New("an update is in progress on this member")
		}
		to := r.Version
		var err error
		if r.Op == "rollback" {
			to, err = d.Inst.RollbackByDaemon(r.Running, r.ExitMaintenance)
		} else {
			var prog []byte
			if prog, err = os.ReadFile(r.Program); err == nil {
				err = d.Inst.InstallByDaemon(prog, r.Version, r.Running, r.ExitMaintenance)
				os.Remove(r.Program)
			}
		}
		if err != nil {
			return err
		}
		d.Log.Warn("switchd-update: installed; switchd restarts", "facility", "change-log", "version", to, "from", r.Running)
		go func() {
			time.Sleep(time.Second) // switchd answers its caller first
			d.restartAndWatch(ctx, to, false)
		}()
		rep.Version = to
	default:
		return fmt.Errorf("unknown request %q", r.Op)
	}
	d.mu.Lock()
	rep.State = d.state
	d.mu.Unlock()
	rep.Note = d.Inst.Load().Note
	return nil
}

// restartAndWatch restarts switchd onto v and watches it; final: v is
// already the way back (it is not reverted again).
func (d *Daemon) restartAndWatch(ctx context.Context, v string, final bool) {
	d.mu.Lock()
	d.waiting, d.healthy = v, make(chan struct{})
	d.mu.Unlock()
	d.setState("restarting switchd (" + v + ")")
	if err := d.Restart(); err != nil {
		d.Log.Error("switchd-update: restart", "err", err)
	}
	d.watch(ctx, v, d.timeout(), final)
}

// watch waits until switchd reports v healthy; else it returns to the
// previous version (and watches that one).
func (d *Daemon) watch(ctx context.Context, v string, within time.Duration, final bool) {
	d.mu.Lock()
	if d.waiting != v || d.healthy == nil {
		d.waiting, d.healthy = v, make(chan struct{})
	}
	ch := d.healthy
	d.mu.Unlock()
	d.setState("waiting for switchd " + v)
	t := time.NewTimer(within)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-ch:
		d.mu.Lock()
		d.waiting = ""
		d.mu.Unlock()
		d.setState("idle (" + v + " is healthy)")
		if v != d.Version && d.Exit != nil {
			// The program was replaced: continue on the new one.
			d.Log.Info("switchd-update: restarting onto the new program", "version", v)
			d.Exit()
		}
		return
	case <-t.C:
	}
	why := fmt.Sprintf("was not healthy within %s", within.Round(time.Second))
	if final {
		d.mu.Lock()
		d.waiting = ""
		d.mu.Unlock()
		d.setState("failed: the previous version " + v + " " + why + " either")
		d.Log.Error("switchd-update: the previous version "+v+" "+why+" either; giving up", "facility", "change-log")
		return
	}
	d.Log.Error("switchd-update: "+v+" "+why+"; returning to the previous version", "facility", "change-log")
	d.mu.Lock()
	d.waiting = ""
	d.mu.Unlock()
	if err := d.Inst.Revert(why); err != nil {
		d.setState("failed: " + err.Error())
		return
	}
	p := d.Inst.Load().Pending
	if p == nil {
		d.setState("failed: no previous version")
		return
	}
	d.setState("rolled back: " + v + " " + why)
	d.restartAndWatch(ctx, p.Version, true)
}

// Call sends one request to the daemon at socket.
func Call(socket string, r Request) (Reply, error) {
	c, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return Reply{}, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	raw, _ := json.Marshal(r)
	if _, err := c.Write(append(raw, '\n')); err != nil {
		return Reply{}, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return Reply{}, err
	}
	var rep Reply
	if err := json.Unmarshal(line, &rep); err != nil {
		return Reply{}, err
	}
	if rep.Err != "" {
		return rep, errors.New(rep.Err)
	}
	return rep, nil
}
