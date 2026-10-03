package cli

import (
	"strings"
	"testing"
)

func TestWriteSlots(t *testing.T) {
	var b strings.Builder
	writeSlots(&b, SoftwareMember{Daemon: "idle", BootState: "missing (the boot loader uses its defaults: slot A first, both slots bootable)",
		Slots: []SoftwareSlot{
			{Name: "A", Version: "1.4.0", Active: true, OK: true, Next: true},
			{Name: "B", Version: "1.3.2", OK: true, Error: "unreadable: /dev/sda3: read /dev/sda3: no answer after 5s (the device may hang)"},
		}}, "  ")
	want := "  boot state: missing (the boot loader uses its defaults: slot A first, both slots bootable)\n" +
		"  slot A: 1.4.0            active\n" +
		"  slot B: 1.3.2            backup (unreadable: /dev/sda3: read /dev/sda3: no answer after 5s (the device may hang))\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
	b.Reset()
	writeSlots(&b, SoftwareMember{DaemonErr: "read unix: i/o timeout"}, "")
	if b.String() != "slots: unknown (the update daemon does not answer: read unix: i/o timeout)\n" {
		t.Errorf("no daemon: %q", b.String())
	}
}
