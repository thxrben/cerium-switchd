package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
)

type reloadOps struct {
	fakeOps
	reloads []string
	err     error
}

func (o *reloadOps) Reload(user string) error {
	if o.err != nil {
		return o.err
	}
	o.reloads = append(o.reloads, user)
	return nil
}

func (o *reloadOps) Started(int) (time.Time, error) { return time.Now(), nil }

func TestReload(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "root", commit.SuperUser)
	ops := &reloadOps{}
	ts.sh.env.Ops = ops
	ts.term.answers = []string{"no"}
	ts.ok("request system reload")
	if len(ops.reloads) != 0 {
		t.Fatal("reloaded without yes")
	}
	ts.term.answers = []string{"yes"}
	if out := ts.ok("request system reload"); !strings.Contains(out, "Reload requested") || len(ops.reloads) != 1 || ops.reloads[0] != "root" {
		t.Fatalf("reload: %q %v", out, ops.reloads)
	}
	ops.err = errors.New("the reload is refused while a software update runs")
	ts.term.answers = []string{"yes"}
	if out := ts.run("request system reload"); !strings.Contains(out, "refused while a software update runs") {
		t.Fatalf("refusal: %q", out)
	}
}
