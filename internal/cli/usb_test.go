package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/commit"
)

// Operators reach the USB commands (request is open to them for this),
// not the other requests.
func TestUSBCommands(t *testing.T) {
	ts := newTester(t, newEngine(t), "olga", commit.Operator)
	ts.sh.env.Ops = &allOps{}
	out := ts.sh.Execute(context.Background(), "file list usb:", ts.term).Output
	for _, want := range []string{"USB stick: Kingston DT, 16.0 GB, vfat (/dev/sdb1)", "configs/", "sw1.conf", "1234"} {
		if !strings.Contains(out, want) {
			t.Errorf("file list lacks %q:\n%s", want, out)
		}
	}
	if out := ts.sh.Execute(context.Background(), "request system storage usb eject", ts.term).Output; !strings.Contains(out, "can be removed") {
		t.Errorf("eject: %s", out)
	}
	if out := ts.sh.Execute(context.Background(), "request system reboot", ts.term).Output; !strings.Contains(out, "permission denied") {
		t.Errorf("an operator may reboot: %s", out)
	}
	if out := ts.sh.Execute(context.Background(), "file list /etc", ts.term).Output; !strings.Contains(out, "expecting usb:") {
		t.Errorf("file list of another path: %s", out)
	}
}
