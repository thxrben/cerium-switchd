package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/lib/conf/config"
	"github.com/thxrben/cerium-switchd/lib/conf/model"
	"github.com/thxrben/cerium-switchd/lib/platform/alarms"
	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
)

// archiver sends copies of the active configuration to the archive sites
// (system archival configuration, reference 5.1), on the master only.
type archiver struct {
	// active returns the active configuration and its revision.
	active   func() (*config.Tree, uint64)
	isMaster func() bool
	hostName func() string
	alarms   *alarms.Set
	log      *slog.Logger
	// run runs a command (curl; tests replace it); now is the clock.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
	now func() time.Time

	seq      uint64
	cfg      *model.Config
	lastSent time.Time
	lastTry  time.Time
	failing  bool
	pending  bool // a commit not sent yet
}

const archivalAlarm = "switchd/archival"

func (a *archiver) loop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		a.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// step sends a copy when one is due.
func (a *archiver) step(ctx context.Context) {
	tree, seq := a.active()
	if seq != a.seq || a.cfg == nil {
		first := a.cfg == nil
		a.seq = seq
		a.cfg, _ = model.Build(tree, nil)
		if !first {
			a.pending = true // a commit (or rollback)
		}
	}
	ar := a.cfg.System.Archival
	if ar == nil || len(ar.Sites) == 0 {
		a.pending = false
		if a.failing {
			a.failing = false
			a.alarms.Clear(archivalAlarm)
		}
		return
	}
	if !a.isMaster() {
		return
	}
	now := a.now()
	due := (ar.OnCommit && a.pending) ||
		(ar.Interval > 0 && now.Sub(a.lastSent) >= time.Duration(ar.Interval)*time.Minute) ||
		(a.failing && now.Sub(a.lastTry) >= 15*time.Minute)
	if !due || (a.failing && now.Sub(a.lastTry) < time.Minute) {
		return
	}
	a.lastTry = now
	if err := a.send(ctx, tree, ar, now); err != nil {
		a.log.Warn("configuration archival failed", "err", err)
		if !a.failing {
			a.failing = true
			a.alarms.Raise(archivalAlarm, alarms.Minor, "configuration archival failed: "+err.Error())
		}
		return
	}
	a.pending, a.lastSent = false, now
	if a.failing {
		a.failing = false
		a.alarms.Clear(archivalAlarm)
	}
}

// archiveName is the file name of a copy.
func archiveName(host string, t time.Time) string {
	if host == "" {
		host = "switch"
	}
	return host + "_ceros.conf.gz_" + t.UTC().Format("20060102_150405")
}

// send uploads one copy, trying the sites in order.
func (a *archiver) send(ctx context.Context, tree *config.Tree, ar *model.Archival, now time.Time) error {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(config.FormatCurly(tree.Root)))
	zw.Close()
	tmp, err := hwio.CreateTemp("", "ceros-archive-*")
	if err != nil {
		return err
	}
	defer hwio.Remove(tmp.Name())
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	name := archiveName(a.hostName(), now)
	vrf := a.cfg.System.MgmtInstance
	var errs []string
	for _, s := range ar.Sites {
		u, err := url.Parse(s.URL)
		if err != nil {
			continue
		}
		args := []string{"-fsS", "--connect-timeout", "15", "--max-time", "300", "-T", tmp.Name()}
		password := s.Password
		if u.User != nil {
			if p, ok := u.User.Password(); ok {
				password = p
			}
			args = append(args, "-u", u.User.Username()+":"+password)
			u.User = nil
		}
		if u.Scheme == "sftp" || u.Scheme == "scp" {
			args = append(args, "--insecure") // no known_hosts on a switch
			if password == "" {
				for _, k := range []string{"/root/.ssh/id_ed25519", "/root/.ssh/id_rsa"} {
					if _, err := hwio.Stat(k); err == nil {
						args = append(args, "--key", k)
						break
					}
				}
			}
		}
		if !strings.HasSuffix(u.Path, "/") {
			u.Path += "/"
		}
		u.Path += name
		args = append(args, u.String())
		cmd := "curl"
		if vrf != "" {
			args = append([]string{"vrf", "exec", vrf, "curl"}, args...)
			cmd = "ip"
		}
		out, err := a.run(ctx, cmd, args...)
		if err == nil {
			a.log.Info("configuration archived", "site", u.Redacted(), "revision", a.seq)
			return nil
		}
		errs = append(errs, fmt.Sprintf("%s: %s", u.Redacted(), strings.TrimSpace(string(out))))
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
