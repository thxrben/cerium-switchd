package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"mclag/internal/config"
	"mclag/internal/schema"
)

// Completion is one possible next word.
type Completion struct {
	Text string
	Help string
	// Placeholder marks a value description such as "<hostname>" that
	// cannot be inserted literally.
	Placeholder bool
	// Kind: '>' container or list, '+' leaf-list, ' ' otherwise.
	Kind byte
}

// enter is shown when the words typed so far form a complete command.
var enter = Completion{Text: "<[Enter]>", Help: "Execute this command", Placeholder: true}

func words(cs ...Completion) func(*Shell, []config.Token, string) []Completion {
	return func(_ *Shell, args []config.Token, partial string) []Completion {
		if len(args) > 0 {
			return nil
		}
		return append([]Completion{enter}, filter(cs, partial)...)
	}
}

func filter(cs []Completion, partial string) []Completion {
	var out []Completion
	for _, c := range cs {
		if c.Placeholder || strings.HasPrefix(c.Text, partial) {
			out = append(out, c)
		}
	}
	return out
}

// Complete returns the possible next words for line (the text up to the
// cursor). The last word is being completed unless line ends in a space.
func (sh *Shell) Complete(line string) (res []Completion) {
	sh.checkSession()
	defer func() {
		if r := recover(); r != nil {
			sh.env.Log.Error("cli: completion panic", "line", line, "panic", r)
			res = nil
		}
	}()
	if len(line) > MaxLineLength {
		return nil
	}
	toks, err := config.Lex(line, config.LexCommand)
	if err != nil {
		return nil // e.g. inside an open quote
	}
	partial := ""
	if len(toks) > 0 && !strings.HasSuffix(line, " ") && !strings.HasSuffix(line, "\t") {
		last := toks[len(toks)-1]
		if !last.Punct {
			partial, toks = last.Text, toks[:len(toks)-1]
		}
	}
	cmdToks, pipes := splitPipes(toks)
	if len(pipes) > 0 {
		return completePipe(pipes[len(pipes)-1], partial)
	}
	cmds := operational
	if sh.sess != nil {
		cmds = configuration
	}
	return sh.completeCmd(cmds, cmdToks, partial)
}

func (sh *Shell) completeCmd(cmds []*command, toks []config.Token, partial string) []Completion {
	for len(toks) > 0 {
		m := lookupCmd(cmds, toks[0].Text)
		if len(m) != 1 || toks[0].Quoted || sh.env.Class > m[0].class {
			return nil
		}
		cmd := m[0]
		toks = toks[1:]
		if len(cmd.sub) > 0 && (cmd.run == nil || len(toks) == 0 || len(lookupCmd(cmd.sub, toks[0].Text)) > 0) {
			if len(toks) == 0 {
				out := sh.cmdList(cmd.sub, partial)
				if cmd.run != nil {
					out = append([]Completion{enter}, out...)
				}
				if cmd.complete != nil {
					out = append(out, cmd.complete(sh, nil, partial)...)
				}
				return out
			}
			cmds = cmd.sub
			continue
		}
		if (cmd.perMember || cmd.stackWide) && sh.env.Stack != nil {
			return sh.completeTarget(cmd, toks, partial)
		}
		if cmd.complete == nil {
			if len(toks) == 0 && partial == "" {
				return []Completion{enter}
			}
			return nil
		}
		return cmd.complete(sh, toks, partial)
	}
	return sh.cmdList(cmds, partial)
}

func (sh *Shell) cmdList(cmds []*command, partial string) []Completion {
	var out []Completion
	for _, c := range cmds {
		if !c.hidden && strings.HasPrefix(c.name, partial) && sh.env.Class <= c.class {
			k := byte(' ')
			if len(c.sub) > 0 {
				k = '>'
			}
			out = append(out, Completion{Text: c.name, Help: c.help, Kind: k})
		}
	}
	return out
}

func completePipe(seg []config.Token, partial string) []Completion {
	var out []Completion
	if len(seg) == 0 {
		for _, p := range pipeCommands {
			if strings.HasPrefix(p.name, partial) {
				out = append(out, Completion{Text: p.name, Help: p.help})
			}
		}
		return out
	}
	switch {
	case prefixOf(seg[0].Text, "display"):
		switch len(seg) {
		case 1:
			return filter([]Completion{{Text: "set", Help: "Show as set commands"}, {Text: "json", Help: "Show as JSON"}}, partial)
		case 2:
			if prefixOf(seg[1].Text, "set") {
				return append([]Completion{enter}, filter([]Completion{{Text: "relative", Help: "Paths relative to the edit level"}}, partial)...)
			}
		}
	case prefixOf(seg[0].Text, "compare"):
		if len(seg) == 1 {
			return append([]Completion{enter}, filter([]Completion{{Text: "rollback", Help: "Compare with a previous revision"}}, partial)...)
		}
		if len(seg) == 2 {
			return []Completion{{Text: "<number>", Help: "Revision number", Placeholder: true}}
		}
	case prefixOf(seg[0].Text, "match"), prefixOf(seg[0].Text, "except"), prefixOf(seg[0].Text, "find"):
		if len(seg) == 1 {
			return []Completion{{Text: "<pattern>", Help: "Regular expression", Placeholder: true}}
		}
	case prefixOf(seg[0].Text, "last"):
		if len(seg) == 1 {
			return []Completion{enter, {Text: "<number>", Help: "Number of lines", Placeholder: true}}
		}
	}
	return nil
}

// Help renders the "?" output for line.
func (sh *Shell) Help(line string) string {
	cs := sh.Complete(line)
	if len(cs) == 0 {
		return "No valid completions\n"
	}
	var b strings.Builder
	b.WriteString("Possible completions:\n")
	for _, c := range cs {
		k := c.Kind
		if k == 0 {
			k = ' '
		}
		fmt.Fprintf(&b, "%c %-22s %s\n", k, c.Text, c.Help)
	}
	return b.String()
}

// ---- path completion ----

func completeActivePath(sh *Shell, args []config.Token, partial string) []Completion {
	return sh.completePath(schema.Root(), nil, sh.env.Engine.Active(), args, partial, config.ModeNav)
}

func completeCandidatePath(mode config.ResolveMode) func(*Shell, []config.Token, string) []Completion {
	return func(sh *Shell, args []config.Token, partial string) []Completion {
		base, prefix := sh.baseSchema(), sh.edit
		if n := len(prefix); n > 0 && prefix[n-1].Schema.Kind == schema.List && !prefix[n-1].HasKey {
			// At a list level the first word is a key: complete "<list> …"
			// from the parent.
			args = append([]config.Token{{Text: prefix[n-1].Schema.Name}}, args...)
			prefix = prefix[:n-1]
			base = schema.Root()
			if n > 1 {
				base = prefix[n-2].Schema
			}
		}
		return sh.completePath(base, prefix, sh.sess.Candidate(), args, partial, mode)
	}
}

// completePath completes a configuration path below base (whose path from
// the root is prefix), using t for existing list keys and references.
func (sh *Shell) completePath(base *schema.Node, prefix []config.Step, t *config.Tree, toks []config.Token, partial string, mode config.ResolveMode) []Completion {
	steps, err := config.Resolve(base, toks, config.ModeDelete)
	if err != nil {
		return nil
	}
	sn := base
	complete := len(toks) > 0 || len(prefix) > 0 || mode != config.ModeSet
	if len(steps) > 0 {
		last := steps[len(steps)-1]
		full := append(append([]config.Step(nil), prefix...), steps...)
		switch last.Schema.Kind {
		case schema.List:
			if !last.HasKey {
				out := sh.keyCompletions(last.Schema, t.Lookup(full[:len(full)-1]), partial)
				if mode != config.ModeSet {
					out = append([]Completion{enter}, out...)
				}
				return out
			}
			complete = true
		case schema.Leaf, schema.LeafList:
			if len(last.Values) > 0 || mode == config.ModeNav {
				if partial == "" {
					return []Completion{enter}
				}
				return nil
			}
			out := sh.valueCompletions(last.Schema.Type, t, partial)
			if mode == config.ModeDelete {
				out = append([]Completion{enter}, out...)
			}
			return out
		case schema.Flag:
			if partial == "" {
				return []Completion{enter}
			}
			return nil
		case schema.Container:
			complete = mode != config.ModeSet || last.Schema.Presence
		}
		sn = last.Schema
	}
	var out []Completion
	if complete && partial == "" && len(toks) > 0 {
		out = append(out, enter)
	}
	if passwordParent(sn.Name) && mode == config.ModeSet && strings.HasPrefix(plainTextPassword, partial) {
		out = append(out, Completion{Text: plainTextPassword, Help: "Prompt for a password (only its hash is stored)"})
	}
	for _, c := range sn.Children {
		if !strings.HasPrefix(c.Name, partial) {
			continue
		}
		k := byte(' ')
		switch c.Kind {
		case schema.Container, schema.List:
			k = '>'
		case schema.LeafList:
			k = '+'
		}
		out = append(out, Completion{Text: c.Name, Help: c.Help, Kind: k})
	}
	return out
}

// keyCompletions lists existing entries of a list plus its value type.
func (sh *Shell) keyCompletions(sn *schema.Node, parent *config.Node, partial string) []Completion {
	var out []Completion
	seen := map[string]bool{}
	for _, e := range parent.Entries(sn.Name) {
		if strings.HasPrefix(e.Key, partial) && !seen[e.Key] {
			seen[e.Key] = true
			out = append(out, Completion{Text: e.Key, Help: sn.Help, Kind: '>'})
		}
	}
	if sn.Type.Ref == "interface" || sn.Type.Ref == "physical-interface" {
		for _, p := range sh.ports() {
			if strings.HasPrefix(p, partial) && !seen[p] {
				seen[p] = true
				out = append(out, Completion{Text: p, Help: "Interface (hardware)", Kind: '>'})
			}
		}
	}
	if sn.Type.Ref == "interface" {
		if strings.HasPrefix("irb", partial) && !seen["irb"] {
			out = append(out, Completion{Text: "irb", Help: "VLAN IP interfaces (irb.<n>, attached with 'vlans <v> l3-interface')", Kind: '>'})
		}
		if strings.HasPrefix("cme", partial) && !seen["cme"] {
			out = append(out, Completion{Text: "cme", Help: "Chassis management interface (the stack's management address, on the master)", Kind: '>'})
		}
		if strings.HasPrefix("ae", partial) || partial == "" {
			out = append(out, Completion{Text: "ae<N>", Help: "Aggregated interface (ae0-ae4095)", Placeholder: true})
		}
		// The generic <interface-name> placeholder adds nothing here.
		return out
	}
	return append(out, typeCompletions(sn.Type, partial)...)
}

func (sh *Shell) ports() []string {
	if sh.env.Ports == nil {
		return nil
	}
	return sh.env.Ports()
}

// valueCompletions lists enum values, referenced names and the type.
func (sh *Shell) valueCompletions(ty *schema.Type, t *config.Tree, partial string) []Completion {
	var out []Completion
	add := func(names []string, help string) {
		sort.Slice(names, func(i, j int) bool { return config.NaturalLess(names[i], names[j]) })
		for _, n := range names {
			if strings.HasPrefix(n, partial) {
				out = append(out, Completion{Text: n, Help: help})
			}
		}
	}
	switch ty.Ref {
	case "irb-unit":
		var names []string
		if irb := t.Root.Entry("interfaces", "irb"); irb != nil {
			for _, u := range irb.Entries("unit") {
				names = append(names, "irb."+u.Key)
			}
		}
		add(names, "VLAN IP interface")
	case "instance":
		var names []string
		for _, e := range t.Root.Entries("routing-instances") {
			names = append(names, e.Key)
		}
		add(names, "Routing instance")
	case "vlan":
		var names []string
		for _, e := range t.Root.Entries("vlans") {
			names = append(names, e.Key)
		}
		add(names, "VLAN")
	case "interface", "physical-interface", "ae-interface":
		seen := map[string]bool{}
		var names []string
		for _, e := range t.Root.Entries("interfaces") {
			seen[e.Key] = true
			names = append(names, e.Key)
		}
		if ty.Ref != "ae-interface" {
			for _, p := range sh.ports() {
				if !seen[p] {
					names = append(names, p)
				}
			}
		}
		var keep []string
		for _, n := range names {
			if (ty.Ref == "ae-interface") == schema.IsAE(n) || ty.Ref == "interface" {
				keep = append(keep, n)
			}
		}
		add(keep, "Interface")
	}
	return append(out, typeCompletions(ty, partial)...)
}

func typeCompletions(ty *schema.Type, partial string) []Completion {
	if len(ty.Enum) > 0 {
		var out []Completion
		for _, e := range ty.Enum {
			if strings.HasPrefix(e.Name, partial) {
				out = append(out, Completion{Text: e.Name, Help: e.Help})
			}
		}
		return out
	}
	return []Completion{{Text: ty.Name, Help: ty.Desc, Placeholder: true}}
}

// ---- argument completion of other commands ----

func completeCommit(_ *Shell, args []config.Token, partial string) []Completion {
	opts := []Completion{
		{Text: "and-quit", Help: "Commit and leave configuration mode"},
		{Text: "check", Help: "Check the candidate without committing"},
		{Text: "comment", Help: "Store a comment with the revision"},
		{Text: "confirmed", Help: "Roll back automatically unless confirmed"},
	}
	if len(args) > 0 {
		last := args[len(args)-1].Text
		if prefixOf(last, "comment") {
			return []Completion{{Text: "<text>", Help: "Comment", Placeholder: true}}
		}
		if prefixOf(last, "check") {
			return []Completion{enter}
		}
		if prefixOf(last, "confirmed") {
			opts = append([]Completion{{Text: "<minutes>", Help: "Minutes until rollback (1..60)", Placeholder: true}}, opts...)
		}
	}
	used := map[string]bool{}
	for _, a := range args {
		for _, o := range opts {
			if prefixOf(a.Text, o.Text) {
				used[o.Text] = true
			}
		}
	}
	var out []Completion
	for _, o := range opts {
		if !used[o.Text] && (len(args) == 0 || o.Text != "check") {
			out = append(out, o)
		}
	}
	return append([]Completion{enter}, filter(out, partial)...)
}

func completeCopy(sh *Shell, args []config.Token, partial string) []Completion {
	n := len(args)
	if n >= 1 && args[n-1].Text == "to" {
		return []Completion{{Text: "<name>", Help: "New name", Placeholder: true}}
	}
	if n >= 2 && args[n-2].Text == "to" {
		return []Completion{enter}
	}
	out := sh.completePath(sh.baseSchema(), sh.edit, sh.sess.Candidate(), args, partial, config.ModeNav)
	if steps, err := config.Resolve(sh.baseSchema(), args, config.ModeNav); err == nil && len(steps) > 0 {
		if l := steps[len(steps)-1]; l.Schema.Kind == schema.List && l.HasKey && strings.HasPrefix("to", partial) {
			out = []Completion{{Text: "to", Help: "Destination name"}}
		}
	}
	var clean []Completion
	for _, c := range out {
		if c != enter {
			clean = append(clean, c)
		}
	}
	return clean
}

func completeLoad(_ *Shell, args []config.Token, partial string) []Completion {
	switch len(args) {
	case 0:
		return filter([]Completion{
			{Text: "merge", Help: "Merge into the candidate"},
			{Text: "override", Help: "Replace the whole candidate"},
			{Text: "replace", Help: "Merge, honouring replace: and delete: tags"},
			{Text: "set", Help: "Execute set/delete/activate/deactivate lines"},
		}, partial)
	case 1:
		return filter([]Completion{
			{Text: "terminal", Help: "Read from the terminal (end with Ctrl-D)"},
			{Text: "<filename>", Help: "File in your home directory", Placeholder: true},
		}, partial)
	}
	return nil
}

func completeRevision(sh *Shell, args []config.Token, partial string) []Completion {
	if len(args) > 0 {
		return nil
	}
	out := []Completion{enter}
	for _, r := range sh.env.Engine.History() {
		n := strconv.Itoa(r.Number)
		if strings.HasPrefix(n, partial) {
			help := r.Time.UTC().Format("2006-01-02 15:04:05") + " by " + r.User
			if r.Comment != "" {
				help += ": " + r.Comment
			}
			out = append(out, Completion{Text: n, Help: help})
		}
	}
	return out
}

func completeRun(sh *Shell, args []config.Token, partial string) []Completion {
	return sh.completeCmd(operational, args, partial)
}
