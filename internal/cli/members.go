package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"mclag/internal/config"
)

// Stack targets of operational commands (reference 5.2): member <id>,
// all-members, local.

var targetWords = []Completion{
	{Text: "member", Help: "Run on this stack member"},
	{Text: "all-members", Help: "Run on every stack member"},
	{Text: "local", Help: "Run on this member (default)"},
}

// isTargetWord matches a target keyword (unique prefixes of at least 3
// letters).
func isTargetWord(tok config.Token, word string) bool {
	return !tok.Quoted && len(tok.Text) >= 3 && prefixOf(tok.Text, word)
}

// parseTarget takes a target off the end of toks. targets is nil if there
// is none (or it is "local"); pos is the byte position where it starts.
func (sh *Shell) parseTarget(toks []config.Token) (targets []int, pos int, rest []config.Token, err error) {
	n := len(toks)
	switch {
	case n >= 1 && isTargetWord(toks[n-1], "local"):
		return nil, 0, toks[:n-1], nil
	case n >= 1 && isTargetWord(toks[n-1], "all-members"):
		return sh.env.Stack.Members(), toks[n-1].Pos, toks[:n-1], nil
	case n >= 2 && isTargetWord(toks[n-2], "member"):
		id, err := strconv.Atoi(toks[n-1].Text)
		if err != nil || !slices.Contains(sh.env.Stack.Members(), id) {
			return nil, 0, nil, &posError{pos: toks[n-1].Pos, msg: fmt.Sprintf("expecting a member of this stack %v", sh.env.Stack.Members())}
		}
		if id == sh.env.Stack.Self() {
			return nil, 0, toks[:n-2], nil
		}
		return []int{id}, toks[n-2].Pos, toks[:n-2], nil
	case n >= 1 && isTargetWord(toks[n-1], "member"):
		return nil, 0, nil, &posError{pos: toks[n-1].Pos + len(toks[n-1].Text), msg: "expecting a member id"}
	}
	// The local command itself rejects a misplaced target word.
	return nil, 0, toks, nil
}

// runOnMembers runs cmd (arguments rest) on the targets and prints a section
// per member. The command line up to pos is what runs on the other members.
func (sh *Shell) runOnMembers(c *call, cmd *command, rest []config.Token, targets []int, pos int) error {
	self := sh.env.Stack.Self()
	line := strings.TrimSpace(c.line[:pos])
	if i := strings.IndexByte(line, '|'); i >= 0 { // never: pipes follow the target
		line = strings.TrimSpace(line[:i])
	}
	confirmed := false
	order := slices.Clone(targets)
	if cmd.confirm != "" {
		names := make([]string, len(targets))
		for i, id := range targets {
			names[i] = fmt.Sprintf("member %d", id)
		}
		a, err := c.term.Ask(fmt.Sprintf("%s %s ? [yes,no] (no) ", cmd.confirm, strings.Join(names, ", ")), true)
		if err != nil || !isYes(a) {
			return nil
		}
		confirmed = true
		// The other members first: this one may go away with the command.
		order = slices.DeleteFunc(order, func(id int) bool { return id == self })
		if slices.Contains(targets, self) {
			order = append(order, self)
		}
	}
	out := make(map[int]string, len(order))
	var mu sync.Mutex
	var wg sync.WaitGroup
	runRemote := func(id int) {
		text, err := sh.env.Stack.Exec(c.ctx, id, line, confirmed)
		if err != nil {
			text += fmt.Sprintf("error: %v\n", err)
		}
		mu.Lock()
		out[id] = text
		mu.Unlock()
	}
	for _, id := range order {
		if id == self {
			continue
		}
		if confirmed {
			runRemote(id) // in order
		} else {
			wg.Add(1)
			go func() { defer wg.Done(); runRemote(id) }()
		}
	}
	wg.Wait()
	if slices.Contains(order, self) {
		var local strings.Builder
		saved, savedTerm := c.out, c.term
		c.out, c.args = &local, rest
		if confirmed {
			c.term = yesTerm{c.term}
		}
		if err := cmd.run(sh, c); err != nil {
			fmt.Fprintf(&local, "error: %v\n", err)
		}
		c.out, c.term = saved, savedTerm
		out[self] = local.String()
	}
	for i, id := range targets {
		if i > 0 {
			c.out.WriteString("\n")
		}
		fmt.Fprintf(c.out, "member%d:\n%s\n", id, strings.Repeat("-", 74))
		c.out.WriteString(out[id])
	}
	return nil
}

// yesTerm answers the command's own question: it was asked for all
// targets already.
type yesTerm struct{ Terminal }

func (yesTerm) Ask(string, bool) (string, error) { return "yes", nil }

// completeTarget adds the target words to a per-member command's argument
// completions.
func (sh *Shell) completeTarget(cmd *command, toks []config.Token, partial string) []Completion {
	n := len(toks)
	switch {
	case n >= 1 && isTargetWord(toks[n-1], "member"):
		var out []Completion
		for _, id := range sh.env.Stack.Members() {
			out = append(out, Completion{Text: strconv.Itoa(id), Help: fmt.Sprintf("Member %d", id)})
		}
		return filter(out, partial)
	case n >= 1 && (isTargetWord(toks[n-1], "all-members") || isTargetWord(toks[n-1], "local")),
		n >= 2 && isTargetWord(toks[n-2], "member"):
		if partial == "" {
			return []Completion{enter}
		}
		return nil
	}
	var base []Completion
	if cmd.complete != nil {
		base = cmd.complete(sh, toks, partial)
	} else if n == 0 && partial == "" {
		base = []Completion{enter}
	}
	return append(base, filter(targetWords, partial)...)
}

// markPerMember flags the commands that accept stack targets.
func markPerMember() {
	for _, p := range []struct {
		path    string
		confirm string
	}{
		{"show interfaces", ""}, {"show ethernet-switching table", ""}, {"show vlans", ""},
		{"show chassis hardware", ""}, {"show system uptime", ""}, {"show system ntp", ""}, {"show system offload", ""}, {"show system limits", ""},
		{"show system syslog", ""}, {"show version", ""}, {"show log", ""}, {"show arp", ""},
		{"show ipv6 neighbors", ""}, {"show route", ""}, {"show virtual-chassis vc-port", ""}, {"show virtual-chassis mtu", ""},
		{"show lacp interfaces", ""}, {"show lacp statistics interfaces", ""}, {"show mclag", ""},
		{"clear ethernet-switching table", ""}, {"clear system reboot", ""},
		{"request system reboot", "Reboot"}, {"request system halt", "Halt"}, {"request system power-off", "Power off"},
	} {
		cmd := findCmd(operational, strings.Fields(p.path))
		if cmd == nil {
			panic("per-member command missing: " + p.path)
		}
		cmd.perMember, cmd.confirm = true, p.confirm
	}
}

func findCmd(cmds []*command, path []string) *command {
	for _, c := range cmds {
		if c.name == path[0] {
			if len(path) == 1 {
				return c
			}
			return findCmd(c.sub, path[1:])
		}
	}
	return nil
}
