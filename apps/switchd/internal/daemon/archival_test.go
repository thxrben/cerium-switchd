package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/lib/conf/config"
	"github.com/thxrben/cerium-switchd/lib/platform/alarms"
)

// Archival: a commit sends a copy (first site failing, the second taking
// it); all sites failing raises an alarm, the next success clears it.
func TestArchival(t *testing.T) {
	tree, _ := config.ParseSet("set system archival configuration transfer-on-commit\n" +
		"set system archival configuration archive-sites ftp://a.example/cfg/\n" +
		"set system archival configuration archive-sites sftp://bak@b.example/x password pw\n")
	seq := uint64(1)
	var calls []string
	fail := map[string]bool{"a.example": true}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	al := &alarms.Set{}
	a := &archiver{active: func() (*config.Tree, uint64) { return tree, seq }, isMaster: func() bool { return true },
		hostName: func() string { return "sw1" }, alarms: al, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now: func() time.Time { return now },
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			url := args[len(args)-1]
			calls = append(calls, url)
			for h := range fail {
				if strings.Contains(url, h) {
					return []byte("refused"), errors.New("exit 7")
				}
			}
			return nil, nil
		}}
	a.step(context.Background()) // start: nothing to send
	if len(calls) != 0 {
		t.Fatalf("sent at start: %v", calls)
	}
	seq = 2 // a commit
	a.step(context.Background())
	if len(calls) != 2 || calls[1] != "sftp://b.example/x/sw1_ceros.conf.gz_20261004_120000" || len(al.List()) != 0 {
		t.Fatalf("calls %v alarms %v", calls, al.List())
	}
	fail["b.example"] = true
	seq = 3
	now = now.Add(time.Hour)
	a.step(context.Background())
	if l := al.List(); len(l) != 1 || l[0].Class != alarms.Minor {
		t.Fatalf("alarms %+v", l)
	}
	delete(fail, "b.example")
	now = now.Add(16 * time.Minute) // retried after 15 minutes
	a.step(context.Background())
	if len(al.List()) != 0 {
		t.Fatalf("alarm not cleared: %+v", al.List())
	}
}
