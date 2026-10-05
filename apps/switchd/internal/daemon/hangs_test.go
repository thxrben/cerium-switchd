package daemon

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/lib/platform/alarms"
	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
	"github.com/thxrben/cerium-switchd/lib/sys/nlx"
)

// A kernel call that hangs raises an alarm for the CLI sessions and clears
// it when the call returns; switchd lists it meanwhile.
func TestHangAlarm(t *testing.T) {
	notes := make(chan string, 4)
	al := &alarms.Set{}
	announceAlarms(al, 3, slog.New(slog.NewTextHandler(io.Discard, nil)), func(s string) { notes <- s })
	watchHangs(al)
	release := make(chan struct{})
	start := time.Now()
	err := hwio.DoErr(nlx.Resource, "link add ae1", 30*time.Millisecond, func() error { <-release; return nil })
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	select {
	case n := <-notes:
		if !strings.Contains(n, "member 3: ALARM (Major): the kernel's network configuration") || !strings.Contains(n, "link add ae1") {
			t.Fatalf("notice %q", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no alarm")
	}
	if hs := (&ops{}).Hangs(); len(hs) != 1 || hs[0].Call != "link add ae1" {
		t.Fatalf("hangs %+v", hs)
	}
	close(release)
	select {
	case n := <-notes:
		if !strings.HasPrefix(n, "member 3: alarm cleared (Major): ") {
			t.Fatalf("notice %q", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alarm not cleared")
	}
}
