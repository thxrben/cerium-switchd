package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mclag/internal/commit"
	"mclag/internal/config"
)

// SoftwareRequest is "request system software add|rollback" (reference 3.6).
type SoftwareRequest struct {
	Source, SHA256, Password string
	Member                   int // 0: every member
	NoValidate, Rollback     bool
	// Force updates a member even if it is the only stacking path to others.
	Force bool
}

// SoftwareRun is an update started on the master.
type SoftwareRun struct {
	Request SoftwareRequest
	Started time.Time
	Lines   []string
	Done    bool
	Failed  string
}

// SoftwareMember is one member's software.
type SoftwareMember struct {
	Member                                  int
	Version, Built, Previous, Pending, Note string
	Maintenance                             bool
	Error                                   string
	Daemon                                  string // the update daemon's state ("": not running)
}

// SoftwareStatus is "show system software".
type SoftwareStatus struct {
	Members []SoftwareMember
	Run     *SoftwareRun
}

// Software is what the software commands need; Ops implements it when the
// daemon can update.
type Software interface {
	SoftwareStart(SoftwareRequest) error
	Software() (SoftwareStatus, error)
}

func (sh *Shell) software() (Software, error) {
	if s, ok := sh.env.Ops.(Software); ok && sh.env.Ops != nil {
		return s, nil
	}
	return nil, errors.New("software updates are not available")
}

func softwareCommands() (request, show *command) {
	request = &command{name: "software", help: "Update the stack's software", class: commit.SuperUser, sub: []*command{
		{name: "add", help: "Update every member from a package: http(s)://, ftp://, sftp://, usb:<file> or a file", class: commit.SuperUser,
			run: (*Shell).softwareAdd, complete: completeSoftwareAdd},
		{name: "rollback", help: "Return the members to their previous version", class: commit.SuperUser, run: (*Shell).softwareRollback,
			complete: words(Completion{Text: "member", Help: "Only this member"})},
	}}
	show = &command{name: "software", help: "Show the software of every member and a running update", class: commit.ReadOnly, run: (*Shell).showSoftware}
	return request, show
}

func completeSoftwareAdd(_ *Shell, args []config.Token, partial string) []Completion {
	if len(args) == 0 {
		return []Completion{{Text: "<source>", Help: "http(s)://…, ftp://…, sftp://user@host/path, usb:<file>, /path", Placeholder: true}}
	}
	return append([]Completion{enter}, filter([]Completion{
		{Text: "sha256", Help: "Expected SHA-256 of the package"},
		{Text: "member", Help: "Only this member"},
		{Text: "no-validate", Help: "Update even if the new version rejects the configuration"},
		{Text: "force", Help: "Update a member even if it is the only stacking path to others"},
	}, partial)...)
}

// softwareOptions parses "[sha256 <hex>] [member <id>] [no-validate]".
func softwareOptions(c *call, args []config.Token, req *SoftwareRequest) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case prefixOf(a.Text, "no-validate") && !req.Rollback:
			req.NoValidate = true
		case a.Text == "force":
			req.Force = true
		case (prefixOf(a.Text, "sha256") && !req.Rollback || prefixOf(a.Text, "member")) && i+1 < len(args):
			v := args[i+1].Text
			if prefixOf(a.Text, "member") {
				id, err := strconv.Atoi(v)
				if err != nil || id < 1 || id > 16 {
					return &posError{pos: args[i+1].Pos, msg: "expecting a member id"}
				}
				req.Member = id
			} else {
				if len(v) != 64 {
					return &posError{pos: args[i+1].Pos, msg: "expecting a SHA-256 (64 hex digits)"}
				}
				req.SHA256 = strings.ToLower(v)
			}
			i++
		default:
			return &posError{pos: a.Pos, msg: "syntax error"}
		}
	}
	return nil
}

func (sh *Shell) softwareAdd(c *call) error {
	sw, err := sh.software()
	if err != nil {
		return err
	}
	if len(c.args) == 0 {
		return &posError{pos: c.argPos(0), msg: "expecting the package: http(s)://…, ftp://…, sftp://user@host/path, usb:<file> or a file"}
	}
	req := SoftwareRequest{Source: c.args[0].Text}
	if err := softwareOptions(c, c.args[1:], &req); err != nil {
		return err
	}
	if u := req.Source; (strings.HasPrefix(u, "sftp://") || strings.HasPrefix(u, "ftp://")) && strings.Contains(u, "@") {
		host := strings.SplitN(strings.SplitN(u, "://", 2)[1], "/", 2)[0]
		if !strings.Contains(strings.SplitN(host, "@", 2)[0], ":") {
			p, err := c.term.Ask("Password for "+host+" (empty: use the key): ", false)
			if err != nil {
				return nil
			}
			req.Password = p
		}
	}
	return sh.startSoftware(c, sw, req)
}

func (sh *Shell) softwareRollback(c *call) error {
	sw, err := sh.software()
	if err != nil {
		return err
	}
	req := SoftwareRequest{Rollback: true}
	if err := softwareOptions(c, c.args, &req); err != nil {
		return err
	}
	a, err := c.term.Ask("Return the members to their previous version, one by one? [yes,no] (no) ", true)
	if err != nil || !isYes(a) {
		return nil
	}
	return sh.startSoftware(c, sw, req)
}

// startSoftware starts the update and follows it until it is done or the
// user interrupts (the update goes on).
func (sh *Shell) startSoftware(c *call, sw Software, req SoftwareRequest) error {
	if err := sw.SoftwareStart(req); err != nil {
		return err
	}
	shown := 0
	for {
		st, err := sw.Software()
		if err != nil || st.Run == nil {
			return err
		}
		for _, l := range st.Run.Lines[min(shown, len(st.Run.Lines)):] {
			fmt.Fprintln(c.out, l)
		}
		shown = len(st.Run.Lines)
		if p, ok := c.term.(interface{ Print(string) error }); ok && c.out.Len() > 0 {
			if p.Print(c.out.String()) == nil {
				c.out.Reset()
			}
		}
		if st.Run.Done {
			if st.Run.Failed != "" {
				return errors.New(st.Run.Failed)
			}
			return nil
		}
		select {
		case <-c.ctx.Done():
			fmt.Fprintln(c.out, "(the update continues; show system software)")
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (sh *Shell) showSoftware(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	sw, err := sh.software()
	if err != nil {
		return err
	}
	st, err := sw.Software()
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%-7s %-16s %-17s %-16s %s\n", "Member", "Version", "Built", "Previous", "State")
	for _, m := range st.Members {
		if m.Error != "" {
			fmt.Fprintf(c.out, "%-7d %s\n", m.Member, "not reachable: "+m.Error)
			continue
		}
		built := "-"
		if t, err := time.Parse(time.RFC3339, m.Built); err == nil {
			built = t.Local().Format("2006-01-02 15:04")
		}
		state := "running"
		switch {
		case m.Pending != "":
			state = "updating to " + m.Pending
		case m.Maintenance:
			state = "maintenance"
		}
		fmt.Fprintf(c.out, "%-7d %-16s %-17s %-16s %s\n", m.Member, m.Version, built, orDash(m.Previous), state)
		if m.Note != "" {
			fmt.Fprintf(c.out, "        %s\n", m.Note)
		}
		if m.Daemon == "" {
			c.out.WriteString("        update daemon: not running (switchd installs itself)\n")
		} else if !strings.HasPrefix(m.Daemon, "idle") {
			fmt.Fprintf(c.out, "        update daemon: %s\n", m.Daemon)
		}
	}
	if r := st.Run; r != nil {
		what := "update from " + r.Request.Source
		if r.Request.Rollback {
			what = "rollback"
		}
		state := "running"
		switch {
		case r.Done && r.Failed != "":
			state = "failed: " + r.Failed
		case r.Done:
			state = "done"
		}
		fmt.Fprintf(c.out, "\nLast %s, started %s: %s\n", what, r.Started.Local().Format("2006-01-02 15:04:05"), state)
		for _, l := range r.Lines {
			fmt.Fprintf(c.out, "  %s\n", l)
		}
	}
	return nil
}
