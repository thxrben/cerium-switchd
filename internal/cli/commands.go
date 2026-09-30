package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mclag/internal/access"
	"mclag/internal/commit"
	"mclag/internal/config"
	"mclag/internal/schema"
)

// command is one word of the command grammar.
type command struct {
	name, help string
	sub        []*command
	// run executes the command with the remaining tokens in c.args. A
	// command with sub-commands may also have run (e.g. "commit" alone).
	run func(sh *Shell, c *call) error
	// complete returns completions for the arguments (nil: none).
	complete func(sh *Shell, args []config.Token, partial string) []Completion
	// class is the least privileged class allowed to run the command.
	class commit.Class
	// display allows "| display" and "| compare".
	display bool
	hidden  bool
	// perMember: the command reports or changes one member's state and
	// accepts a target (member <id> | all-members | local) in a stack.
	perMember bool
	// confirm is the question asked before running it on other members
	// ("Reboot"); the command itself then does not ask again.
	confirm string
}

var operational, configuration []*command

func init() {
	showConfig := &command{name: "configuration", help: "Show the active configuration", display: true, class: commit.ReadOnly,
		run: (*Shell).opShowConfiguration, complete: completeActivePath}
	showCommit := &command{name: "commit", help: "Show the commit history", class: commit.ReadOnly, run: (*Shell).showCommitHistory}
	operational = []*command{
		{name: "configure", help: "Enter configuration mode", class: commit.Operator, run: (*Shell).configure,
			complete: words(Completion{Text: "private", Help: "Edit a private copy of the configuration"},
				Completion{Text: "exclusive", Help: "Lock the configuration while editing"})},
		{name: "confirm", help: "Confirm a commit that is pending confirmation", class: commit.Operator, run: (*Shell).confirm},
		{name: "exit", help: "Leave the CLI", class: commit.ReadOnly, run: (*Shell).leaveCLI},
		{name: "quit", help: "Leave the CLI", class: commit.ReadOnly, run: (*Shell).leaveCLI, hidden: true},
		{name: "start", help: "Start a program", class: commit.SuperUser, sub: []*command{
			{name: "shell", help: "Start a Linux shell as your user (exit returns to the CLI)", class: commit.SuperUser, run: (*Shell).startShell},
		}},
		{name: "show", help: "Show information about the switch", class: commit.ReadOnly, sub: []*command{
			showConfig,
			{name: "system", help: "Show system information", class: commit.ReadOnly, sub: []*command{showCommit,
				{name: "rollback", help: "Show a previous configuration, or the changes between two", display: true,
					class: commit.ReadOnly, run: (*Shell).showRollback, complete: completeRollback},
			}},
			{name: "version", help: "Show the software version", class: commit.ReadOnly, run: (*Shell).showVersion},
		}},
	}
	configuration = []*command{
		{name: "activate", help: "Remove the inactive tag from a statement", class: commit.Operator,
			run: (*Shell).cfgActivate, complete: completeCandidatePath(config.ModeNav)},
		{name: "commit", help: "Commit the candidate configuration", class: commit.Operator, run: (*Shell).cfgCommit,
			complete: completeCommit},
		{name: "confirm", help: "Confirm a commit that is pending confirmation", class: commit.Operator, run: (*Shell).confirm},
		{name: "copy", help: "Copy a list entry: copy <path> to <name>", class: commit.Operator,
			run: (*Shell).cfgCopy, complete: completeCopy},
		{name: "deactivate", help: "Add the inactive tag to a statement", class: commit.Operator,
			run: (*Shell).cfgDeactivate, complete: completeCandidatePath(config.ModeNav)},
		{name: "delete", help: "Delete a statement or a value", class: commit.Operator,
			run: (*Shell).cfgDelete, complete: completeCandidatePath(config.ModeDelete)},
		{name: "edit", help: "Move to a level of the configuration hierarchy", class: commit.Operator,
			run: (*Shell).cfgEdit, complete: completeCandidatePath(config.ModeNav)},
		{name: "exit", help: "Leave the current level or configuration mode", class: commit.Operator, run: (*Shell).cfgExit,
			complete: words(Completion{Text: "configuration-mode", Help: "Leave configuration mode from any level"})},
		{name: "load", help: "Load configuration text into the candidate", class: commit.Operator, run: (*Shell).cfgLoad,
			complete: completeLoad},
		{name: "quit", help: "Leave the current level or configuration mode", class: commit.Operator, run: (*Shell).cfgExit, hidden: true},
		{name: "rename", help: "Rename a list entry: rename <path> to <name>", class: commit.Operator,
			run: (*Shell).cfgRename, complete: completeCopy},
		{name: "rollback", help: "Load a previous revision into the candidate", class: commit.Operator, run: (*Shell).cfgRollback,
			complete: completeRevision},
		{name: "run", help: "Run an operational-mode command", class: commit.ReadOnly, run: (*Shell).cfgRun, complete: completeRun},
		{name: "save", help: "Save the candidate at this level to a file", class: commit.Operator, run: (*Shell).cfgSave,
			complete: words(Completion{Text: "<filename>", Help: "File in your home directory", Placeholder: true})},
		{name: "set", help: "Set a statement or a value", class: commit.Operator,
			run: (*Shell).cfgSet, complete: completeCandidatePath(config.ModeSet)},
		{name: "show", help: "Show the candidate configuration", class: commit.Operator, display: true,
			run: (*Shell).cfgShow, complete: completeCandidatePath(config.ModeNav)},
		{name: "status", help: "Show the users editing the configuration", class: commit.Operator, run: (*Shell).cfgStatus},
		{name: "top", help: "Move to the top of the hierarchy", class: commit.Operator, run: (*Shell).cfgTop},
		{name: "up", help: "Move up one or more levels", class: commit.Operator, run: (*Shell).cfgUp,
			complete: words(Completion{Text: "<number>", Help: "Number of levels", Placeholder: true})},
		{name: "update", help: "Rebase a private candidate onto the latest commit", class: commit.Operator, run: (*Shell).cfgUpdate},
	}
	registerOperational()
}

func noArgs(c *call) error {
	if len(c.args) > 0 {
		return &posError{pos: c.args[0].Pos, msg: "syntax error"}
	}
	return nil
}

// ---- operational mode ----

func (sh *Shell) leaveCLI(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	c.reply.Exit = true
	return nil
}

func (sh *Shell) startShell(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	c.reply.Shell = true
	return nil
}

func (sh *Shell) showVersion(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Hostname: %s\nmclag switchd %s\n", sh.env.HostName(), sh.env.Version)
	return nil
}

func (sh *Shell) configure(c *call) error {
	mode := commit.Shared
	switch {
	case len(c.args) == 0:
	case len(c.args) == 1 && prefixOf(c.args[0].Text, "private"):
		mode = commit.Private
	case len(c.args) == 1 && prefixOf(c.args[0].Text, "exclusive"):
		mode = commit.Exclusive
	default:
		return &posError{pos: c.args[0].Pos, msg: "syntax error, expecting 'private' or 'exclusive'"}
	}
	s, msgs, err := sh.env.Engine.Configure(sh.env.User, sh.env.Class, mode)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		fmt.Fprintf(c.out, "warning: %s\n", m)
	}
	if mode != commit.Shared {
		fmt.Fprintf(c.out, "Entering configuration mode (%s)\n", mode)
	} else {
		c.out.WriteString("Entering configuration mode\n")
	}
	sh.sess, sh.edit, sh.levels = s, nil, nil
	return nil
}

func (sh *Shell) confirm(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if err := sh.env.Engine.Confirm(c.ctx, sh.env.User); err != nil {
		return err
	}
	c.out.WriteString("commit confirmed\n")
	return nil
}

func (sh *Shell) opShowConfiguration(c *call) error {
	steps, err := config.Resolve(schema.Root(), c.args, config.ModeNav)
	if err != nil {
		return c.pathErr(c.args, err)
	}
	return sh.show(c, sh.env.Engine.Active(), steps, nil)
}

// showRollback implements "show system rollback <n> [compare <m>]": the
// complete configuration of revision n, or the changes from revision n to
// revision m.
func (sh *Shell) showRollback(c *call) error {
	num := func(i int) (int, error) {
		if i >= len(c.args) {
			return 0, &posError{pos: c.argPos(i), msg: "missing argument: revision number (see 'show system commit')"}
		}
		n, err := strconv.Atoi(c.args[i].Text)
		if err != nil || n < 0 {
			return 0, &posError{pos: c.argPos(i), msg: "expecting a revision number"}
		}
		return n, nil
	}
	n, err := num(0)
	if err != nil {
		return err
	}
	old, err := sh.env.Engine.Revision(n)
	if err != nil {
		return &posError{pos: c.argPos(0), msg: fmt.Sprintf("revision %d does not exist", n)}
	}
	switch {
	case len(c.args) == 1:
	case len(c.args) == 3 && prefixOf(c.args[1].Text, "compare"):
		m, err := num(2)
		if err != nil {
			return err
		}
		cur, err := sh.env.Engine.Revision(m)
		if err != nil {
			return &posError{pos: c.argPos(2), msg: fmt.Sprintf("revision %d does not exist", m)}
		}
		if c.pipes.display != "" || c.pipes.compare {
			return errors.New("'compare' shows changes; it cannot be combined with '| display' or '| compare'")
		}
		c.out.WriteString(config.Diff(old, cur))
		return nil
	default:
		return &posError{pos: c.argPos(1), msg: "syntax error, expecting 'compare <n>'"}
	}
	if c.pipes.display == "" && !c.pipes.compare {
		for _, r := range sh.env.Engine.History() {
			if r.Number == n {
				fmt.Fprintf(c.out, "## Revision %d: %s by %s", n, r.Time.UTC().Format("2006-01-02 15:04:05 UTC"), r.User)
				if r.Comment != "" {
					fmt.Fprintf(c.out, ": %s", r.Comment)
				}
				c.out.WriteString("\n")
			}
		}
	}
	return sh.show(c, old, nil, nil)
}

func completeRollback(sh *Shell, args []config.Token, partial string) []Completion {
	var out []Completion
	switch len(args) {
	case 0, 2:
		for _, r := range sh.env.Engine.History() {
			help := r.Time.UTC().Format("2006-01-02 15:04:05") + " by " + r.User
			if r.Comment != "" {
				help += ": " + r.Comment
			}
			out = append(out, Completion{Text: strconv.Itoa(r.Number), Help: help})
		}
	case 1:
		out = []Completion{enter, {Text: "compare", Help: "Show the changes to another revision"}}
	default:
		out = []Completion{enter}
	}
	return filter(out, partial)
}

func (sh *Shell) showCommitHistory(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	for _, r := range sh.env.Engine.History() {
		state := ""
		if !r.Confirmed {
			state = "  (pending confirmation)"
		}
		fmt.Fprintf(c.out, "%-3d %s by %s%s\n", r.Number, r.Time.UTC().Format("2006-01-02 15:04:05 UTC"), r.User, state)
		if r.Comment != "" {
			fmt.Fprintf(c.out, "    %s\n", r.Comment)
		}
	}
	return nil
}

// ---- showing configuration ----

// show renders the statement at steps of t according to the pipes. rel is
// the edit level for "display set relative".
func (sh *Shell) show(c *call, t *config.Tree, steps []config.Step, rel []config.Step) error {
	p := c.pipes
	if p.compare {
		old, err := sh.env.Engine.Revision(p.compareRev)
		if err != nil {
			return err
		}
		if len(steps) > 0 && steps[len(steps)-1].Schema.Kind == schema.List && !steps[len(steps)-1].HasKey {
			steps = steps[:len(steps)-1]
		}
		c.out.WriteString(config.DiffNodes(pathWords(steps), old.Lookup(steps), t.Lookup(steps)))
		return nil
	}
	// The nodes to show and the path words of their parent.
	var nodes []*config.Node
	var parent []config.Step
	switch {
	case len(steps) == 0:
		nodes, parent = t.Root.Kids, nil
	case steps[len(steps)-1].Schema.Kind == schema.List && !steps[len(steps)-1].HasKey:
		parent = steps[:len(steps)-1]
		nodes = t.Lookup(parent).Entries(steps[len(steps)-1].Schema.Name)
	default:
		parent = steps[:len(steps)-1]
		if n := t.Lookup(steps); n != nil {
			nodes = []*config.Node{n}
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	wrapper := &config.Node{Schema: schema.Root(), Kids: nodes}
	switch p.display {
	case "set":
		prefix := ""
		if p.relative && len(rel) > 0 {
			prefix = strings.Join(pathWords(rel), " ") + " "
		}
		for _, l := range config.SetLines(wrapper, pathWords(parent)) {
			verb, rest, _ := strings.Cut(l, " ")
			c.out.WriteString(verb + " " + strings.TrimPrefix(rest, prefix) + "\n")
		}
	case "json":
		obj := config.ToJSON(wrapper)
		if len(steps) > 0 && len(nodes) == 1 && nodes[0].Schema.HasChildren() && steps[len(steps)-1].Schema == nodes[0].Schema {
			obj = config.ToJSON(nodes[0])
		}
		raw, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			return err
		}
		c.out.Write(raw)
		c.out.WriteByte('\n')
	default:
		if len(nodes) == 1 && len(steps) > 0 && nodes[0].Schema.HasChildren() && nodes[0] == t.Lookup(steps) {
			c.out.WriteString(config.FormatCurly(nodes[0]))
		} else if len(steps) == 0 {
			c.out.WriteString(config.FormatCurly(t.Root))
		} else {
			for _, n := range nodes {
				c.out.WriteString(config.FormatNode(n))
			}
		}
	}
	return nil
}

func pathWords(steps []config.Step) []string {
	var w []string
	for _, s := range steps {
		w = append(w, s.Words()...)
	}
	return w
}

// ---- configuration mode ----

func (sh *Shell) cfgShow(c *call) error {
	steps, err := sh.resolve(c, c.args, config.ModeNav)
	if err != nil {
		return err
	}
	return sh.show(c, sh.sess.Candidate(), steps, sh.edit)
}

func (sh *Shell) cfgSet(c *call) error {
	if len(c.args) == 0 {
		return &posError{pos: len(c.line), msg: "missing statement"}
	}
	if last := c.args[len(c.args)-1]; !last.Quoted && last.Text == plainTextPassword {
		return sh.setPlainTextPassword(c)
	}
	steps, err := sh.resolve(c, c.args, config.ModeSet)
	if err != nil {
		return err
	}
	return sh.sess.Modify(func(t *config.Tree) error { return t.Set(steps) })
}

// plainTextPassword is a CLI-only statement below "authentication": it
// prompts for a password and stores only its hash in encrypted-password.
const plainTextPassword = "plain-text-password"

func (sh *Shell) setPlainTextPassword(c *call) error {
	toks := append(append([]config.Token(nil), c.args[:len(c.args)-1]...), config.Token{Text: "encrypted-password", Pos: c.args[len(c.args)-1].Pos})
	steps, err := sh.resolve(c, toks, config.ModeDelete)
	if err != nil {
		return err
	}
	last := steps[len(steps)-1]
	if last.Schema.Name != "encrypted-password" || len(steps) < 2 || steps[len(steps)-2].Schema.Name != "authentication" {
		return &posError{pos: c.args[len(c.args)-1].Pos, msg: "plain-text-password is only valid below 'system login user <name> authentication'"}
	}
	pw, err := c.term.Ask("New password: ", false)
	if err != nil {
		return err
	}
	again, err := c.term.Ask("Retype new password: ", false)
	if err != nil {
		return err
	}
	switch {
	case pw != again:
		return errors.New("passwords do not match; nothing changed")
	case len(pw) < 8:
		return errors.New("the password must have at least 8 characters; nothing changed")
	}
	hash, err := access.HashPassword(pw)
	if err != nil {
		return err
	}
	steps[len(steps)-1].Values = []string{hash}
	return sh.sess.Modify(func(t *config.Tree) error { return t.Set(steps) })
}

func (sh *Shell) cfgDelete(c *call) error {
	if len(c.args) == 0 {
		a, err := c.term.Ask("Delete everything under this level? [yes,no] (no) ", true)
		if err != nil || !isYes(a) {
			return nil
		}
		return sh.sess.Modify(func(t *config.Tree) error {
			t.ClearBelow(sh.edit)
			return nil
		})
	}
	steps, err := sh.resolve(c, c.args, config.ModeDelete)
	if err != nil {
		return err
	}
	err = sh.sess.Modify(func(t *config.Tree) error { return t.Delete(steps) })
	if errors.Is(err, config.ErrNotFound) {
		c.out.WriteString("warning: statement not found\n")
		return nil
	}
	return err
}

func isYes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}

func (sh *Shell) cfgEdit(c *call) error {
	if len(c.args) == 0 {
		return &posError{pos: len(c.line), msg: "missing statement"}
	}
	steps, err := sh.resolve(c, c.args, config.ModeNav)
	if err != nil {
		return err
	}
	last := steps[len(steps)-1]
	switch {
	case last.Schema.Kind == schema.List && !last.HasKey && !last.Schema.Wrapped:
		return &posError{pos: len(c.line), msg: "missing " + last.Schema.Type.Name}
	case !last.Schema.HasChildren():
		return &posError{pos: c.args[len(c.args)-1].Pos, msg: "syntax error, 'edit' needs a container or list entry"}
	}
	sh.levels = append(sh.levels, sh.edit)
	sh.edit = steps
	return nil
}

func (sh *Shell) cfgUp(c *call) error {
	n := 1
	if len(c.args) > 1 {
		return &posError{pos: c.args[1].Pos, msg: "syntax error"}
	}
	if len(c.args) == 1 {
		v, err := strconv.Atoi(c.args[0].Text)
		if err != nil || v < 1 {
			return &posError{pos: c.args[0].Pos, msg: "expecting a number of levels"}
		}
		n = v
	}
	if len(sh.edit) == 0 {
		c.out.WriteString("warning: already at the top of the hierarchy\n")
		return nil
	}
	if n > len(sh.edit) {
		n = len(sh.edit)
	}
	sh.levels = append(sh.levels, sh.edit)
	sh.edit = sh.edit[:len(sh.edit)-n]
	return nil
}

func (sh *Shell) cfgTop(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if len(sh.edit) > 0 {
		sh.levels = append(sh.levels, sh.edit)
	}
	sh.edit = nil
	return nil
}

func (sh *Shell) cfgExit(c *call) error {
	all := false
	switch {
	case len(c.args) == 0:
	case len(c.args) == 1 && prefixOf(c.args[0].Text, "configuration-mode"):
		all = true
	default:
		return &posError{pos: c.args[0].Pos, msg: "syntax error, expecting 'configuration-mode'"}
	}
	if !all && len(sh.levels) > 0 {
		sh.edit = sh.levels[len(sh.levels)-1]
		sh.levels = sh.levels[:len(sh.levels)-1]
		return nil
	}
	if !all && len(sh.edit) > 0 {
		sh.edit = nil
		return nil
	}
	return sh.leaveConfig(c, true)
}

// leaveConfig ends configuration mode. ask: confirm discarding a private
// or exclusive candidate with changes.
func (sh *Shell) leaveConfig(c *call, ask bool) error {
	if sh.sess.Changed() {
		if sh.sess.Mode == commit.Shared {
			c.out.WriteString("warning: the configuration has been changed but not committed\n")
		} else if ask {
			a, err := c.term.Ask("Discard uncommitted changes? [yes,no] (no) ", true)
			if err != nil || !isYes(a) {
				return nil
			}
		}
	}
	sh.sess.Close()
	sh.sess, sh.edit, sh.levels = nil, nil, nil
	c.out.WriteString("Exiting configuration mode\n")
	return nil
}

func (sh *Shell) cfgActivate(c *call) error   { return sh.setActive(c, true) }
func (sh *Shell) cfgDeactivate(c *call) error { return sh.setActive(c, false) }

func (sh *Shell) setActive(c *call, active bool) error {
	if len(c.args) == 0 {
		return &posError{pos: len(c.line), msg: "missing statement"}
	}
	steps, err := sh.resolve(c, c.args, config.ModeNav)
	if err != nil {
		return err
	}
	err = sh.sess.Modify(func(t *config.Tree) error { return t.SetActive(steps, active) })
	if errors.Is(err, config.ErrNotFound) {
		c.out.WriteString("warning: statement not found\n")
		return nil
	}
	return err
}

// splitTo splits "path… to name" arguments.
func splitTo(c *call) (path []config.Token, name config.Token, err error) {
	n := len(c.args)
	if n < 3 || c.args[n-2].Text != "to" || c.args[n-2].Quoted {
		return nil, name, &posError{pos: len(c.line), msg: "syntax error, expecting '<path> to <name>'"}
	}
	return c.args[:n-2], c.args[n-1], nil
}

func (sh *Shell) cfgCopy(c *call) error   { return sh.entryOp(c, (*config.Tree).Copy) }
func (sh *Shell) cfgRename(c *call) error { return sh.entryOp(c, (*config.Tree).Rename) }

func (sh *Shell) entryOp(c *call, op func(*config.Tree, []config.Step, string) error) error {
	path, name, err := splitTo(c)
	if err != nil {
		return err
	}
	steps, err := sh.resolve(c, path, config.ModeNav)
	if err != nil {
		return err
	}
	return sh.sess.Modify(func(t *config.Tree) error { return op(t, steps, name.Text) })
}

func (sh *Shell) cfgStatus(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	c.out.WriteString("Users currently editing the configuration:\n")
	for _, s := range sh.env.Engine.Status() {
		extra := ""
		if s.Mode == commit.Private && s.Changed {
			extra = ", uncommitted changes"
		}
		fmt.Fprintf(c.out, "  %s (%s) since %s%s\n", s.User, s.Mode, s.Since.UTC().Format("2006-01-02 15:04:05 UTC"), extra)
	}
	return nil
}

func (sh *Shell) cfgUpdate(c *call) error {
	if err := noArgs(c); err != nil {
		return err
	}
	if err := sh.sess.Update(); err != nil {
		return err
	}
	c.out.WriteString("update complete\n")
	return nil
}

func (sh *Shell) cfgRun(c *call) error {
	if len(c.args) == 0 {
		return &posError{pos: len(c.line), msg: "missing command"}
	}
	// Operational commands that change modes make no sense here.
	if w := c.args[0].Text; prefixOf(w, "configure") || prefixOf(w, "exit") || prefixOf(w, "quit") {
		return &posError{pos: c.args[0].Pos, msg: "not available with 'run'"}
	}
	return sh.dispatch(c, operational, c.args)
}

var loadModes = map[string]config.LoadMode{
	"merge": config.LoadMerge, "replace": config.LoadReplace, "override": config.LoadOverride, "set": config.LoadSet,
}

func (sh *Shell) cfgLoad(c *call) error {
	if len(c.args) != 2 {
		return &posError{pos: c.argPos(min(len(c.args), 2)), msg: "syntax error, expecting 'load merge|replace|override|set terminal|<file>'"}
	}
	var mode config.LoadMode
	found := false
	for name, m := range loadModes {
		if prefixOf(c.args[0].Text, name) {
			mode, found = m, true
		}
	}
	if !found || c.args[0].Quoted {
		return &posError{pos: c.args[0].Pos, msg: "syntax error, expecting merge, replace, override or set"}
	}
	var text string
	if src := c.args[1]; !src.Quoted && src.Text == "terminal" {
		t, err := c.term.ReadText("[Type ^D at a new line to end input]")
		if err != nil {
			return err
		}
		text = t
	} else {
		raw, err := c.term.ReadFile(src.Text)
		if err != nil {
			return err
		}
		text = string(raw)
	}
	err := sh.sess.Modify(func(t *config.Tree) error { return config.Load(t, mode, text, sh.edit) })
	if err != nil {
		return fmt.Errorf("load failed, candidate unchanged: %w", err)
	}
	c.out.WriteString("load complete\n")
	return nil
}

func (sh *Shell) cfgSave(c *call) error {
	if len(c.args) != 1 {
		return &posError{pos: c.argPos(min(len(c.args), 1)), msg: "syntax error, expecting 'save <file>'"}
	}
	// Render exactly what "show" shows at this level.
	var text strings.Builder
	sc := &call{ctx: c.ctx, line: c.line, out: &text, term: c.term, pipes: &pipeline{}, reply: c.reply}
	if err := sh.show(sc, sh.sess.Candidate(), sh.edit, nil); err != nil {
		return err
	}
	if err := c.term.WriteFile(c.args[0].Text, []byte(text.String())); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Wrote %d lines of configuration to '%s'\n", strings.Count(text.String(), "\n"), c.args[0].Text)
	return nil
}

func (sh *Shell) cfgRollback(c *call) error {
	n := 0
	if len(c.args) > 1 {
		return &posError{pos: c.args[1].Pos, msg: "syntax error"}
	}
	if len(c.args) == 1 {
		v, err := strconv.Atoi(c.args[0].Text)
		if err != nil || v < 0 {
			return &posError{pos: c.args[0].Pos, msg: "expecting a revision number"}
		}
		n = v
	}
	if err := sh.sess.Rollback(n); err != nil {
		return err
	}
	c.out.WriteString("load complete\n")
	return nil
}

func (sh *Shell) cfgCommit(c *call) error {
	var opts commit.CommitOptions
	check, andQuit := false, false
	for i := 0; i < len(c.args); i++ {
		a := c.args[i]
		switch {
		case !a.Quoted && prefixOf(a.Text, "check") && len(c.args) == 1:
			check = true
		case !a.Quoted && prefixOf(a.Text, "and-quit") && !andQuit:
			andQuit = true
		case !a.Quoted && prefixOf(a.Text, "comment") && opts.Comment == "" && i+1 < len(c.args):
			i++
			if _, err := schema.Text.Validate(c.args[i].Text); err != nil {
				return &posError{pos: c.args[i].Pos, msg: err.Error()}
			}
			opts.Comment = c.args[i].Text
		case !a.Quoted && prefixOf(a.Text, "confirmed") && !opts.Confirmed:
			opts.Confirmed = true
			if i+1 < len(c.args) {
				if v, err := strconv.Atoi(c.args[i+1].Text); err == nil {
					if v < 1 || v > 60 {
						return &posError{pos: c.args[i+1].Pos, msg: "expecting 1..60 minutes"}
					}
					opts.Minutes = v
					i++
				}
			}
		default:
			return &posError{pos: a.Pos, msg: "syntax error, expecting check, and-quit, comment <text> or confirmed [<minutes>]"}
		}
	}
	if check {
		issues := sh.sess.Check()
		c.out.WriteString(issues.String())
		if issues.HasErrors() {
			return commit.ErrCheckFailed
		}
		c.out.WriteString("configuration check succeeds\n")
		return nil
	}
	res, err := sh.sess.Commit(c.ctx, opts)
	if res != nil {
		c.out.WriteString(res.Issues.String())
		for _, m := range res.Members {
			switch {
			case m.Err != nil:
				fmt.Fprintf(c.out, "%s: error: %v\n", m.Member, m.Err)
			case m.Pending:
				fmt.Fprintf(c.out, "%s: pending (unreachable, applies on reconnect)\n", m.Member)
			default:
				fmt.Fprintf(c.out, "%s: commit complete\n", m.Member)
			}
		}
		for _, m := range res.Reverted {
			if m.Err != nil {
				fmt.Fprintf(c.out, "%s: error restoring the previous configuration: %v\n", m.Member, m.Err)
			}
		}
	}
	if err != nil {
		return err
	}
	switch {
	case res.NoChanges && res.Confirmed:
		c.out.WriteString("commit confirmed\n")
	case res.NoChanges:
		c.out.WriteString("commit complete (no changes)\n")
	}
	if !res.Deadline.IsZero() {
		mins := int((res.Deadline.Sub(time.Now()) + 30*time.Second) / time.Minute)
		unit := "minutes"
		if mins == 1 {
			unit = "minute"
		}
		fmt.Fprintf(c.out, "commit confirmed will be automatically rolled back in %d %s unless confirmed\n", mins, unit)
	}
	if andQuit {
		return sh.leaveConfig(c, false)
	}
	return nil
}
