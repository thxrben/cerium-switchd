package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/usbstore"
)

// USB is implemented by switches with USB sticks (reference 3.4): the
// stick of this member.
type USB interface {
	USBList(dir string) (usbstore.Listing, error)
	USBEject() (usbstore.Stick, error)
}

// usbHere runs a stick command on the member the user is connected to (a
// session relayed to the master runs its commands there): it reports
// whether it forwarded the command.
func (sh *Shell) usbHere(c *call, line string) (bool, error) {
	m := sh.origin()
	if m == 0 {
		return false, nil
	}
	out, err := sh.env.Stack.Exec(c.ctx, m, line, true)
	c.out.WriteString(out)
	return true, err
}

func fileCommand() *command {
	return &command{name: "file", help: "Files on a USB stick", class: commit.Operator, sub: []*command{
		{name: "list", help: "List the files of the USB stick of this switch: usb:[<dir>]", class: commit.Operator, run: (*Shell).fileList,
			complete: words(Completion{Text: "usb:", Help: "The USB stick of the switch you are connected to"})},
	}}
}

func usbCommand() *command {
	return &command{name: "storage", help: "Storage requests", class: commit.Operator, sub: []*command{
		{name: "usb", help: "The USB stick of this switch", class: commit.Operator, sub: []*command{
			{name: "eject", help: "Sync, unmount and power off the USB stick", class: commit.Operator, run: (*Shell).usbEject},
		}},
	}}
}

// fileList is "file list usb:[<dir>]".
func (sh *Shell) fileList(c *call) error {
	if len(c.args) != 1 || !strings.HasPrefix(c.args[0].Text, "usb:") {
		return &posError{pos: c.argPos(0), msg: "expecting usb:[<dir>] (files of the USB stick)"}
	}
	if done, err := sh.usbHere(c, "file list "+c.args[0].Text); done {
		return err
	}
	u, ok := sh.env.Ops.(USB)
	if sh.env.Ops == nil || !ok {
		return errors.New("USB sticks are not available")
	}
	l, err := u.USBList(strings.TrimPrefix(c.args[0].Text, "usb:"))
	if err != nil {
		return err
	}
	in := l.Info
	desc := strings.TrimSpace(in.Vendor + " " + in.Model)
	if desc == "" {
		desc = in.Disk
	}
	label := ""
	if in.Label != "" {
		label = ", label " + in.Label
	}
	fmt.Fprintf(c.out, "USB stick: %s, %s, %s%s (%s)\n", desc, bytesText(in.Size), in.FSType, label, in.Device)
	dir := "usb:"
	if l.Dir != "." {
		dir += l.Dir + "/"
	}
	fmt.Fprintf(c.out, "%s\n", dir)
	if len(l.Entries) == 0 {
		c.out.WriteString("  (empty)\n")
		return nil
	}
	for _, e := range l.Entries {
		name, size := e.Name, fmt.Sprint(e.Size)
		if e.Dir {
			name, size = name+"/", "-"
		}
		fmt.Fprintf(c.out, "  %-40s %12s  %s\n", name, size, e.ModTime.Local().Format("2006-01-02 15:04"))
	}
	return nil
}

// usbEject is "request system storage usb eject".
func (sh *Shell) usbEject(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if done, err := sh.usbHere(c, "request system storage usb eject"); done {
		return err
	}
	u, ok := sh.env.Ops.(USB)
	if sh.env.Ops == nil || !ok {
		return errors.New("USB sticks are not available")
	}
	st, err := u.USBEject()
	if err != nil {
		return err
	}
	sh.env.Log.Info("USB stick ejected", "facility", "interactive-commands", "user", sh.env.User, "disk", st.Disk)
	fmt.Fprintf(c.out, "The USB stick (%s) is powered off and can be removed.\n", strings.TrimSpace(st.Vendor+" "+st.Model))
	return nil
}

// bytesText is a size for people (1000-based, as sticks are sold).
func bytesText(n int64) string {
	switch {
	case n >= 1e12:
		return fmt.Sprintf("%.1f TB", float64(n)/1e12)
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%d bytes", n)
}
