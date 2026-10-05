// Package cli implements the Junos-like command line: operational and
// configuration mode, command parsing, completion, "?" help and pipes.
// It runs inside switchd; swcli only does line editing and display.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/lib/conf/config"
	"github.com/thxrben/cerium-switchd/lib/conf/schema"
)

// MaxLineLength bounds a single command line.
const MaxLineLength = 64 << 10

// Terminal is the client side of a session. Everything interactive, and
// all file access, happens there: files are read and written by swcli
// with the permissions of the logged-in user, never by switchd.
type Terminal interface {
	// Ask shows prompt and reads one line; echo=false for passwords.
	Ask(prompt string, echo bool) (string, error)
	// ReadText reads text until end of input (Ctrl-D).
	ReadText(prompt string) (string, error)
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte) error
}

// Env is what a shell needs from switchd.
type Env struct {
	Engine  *commit.Engine
	User    string
	Class   commit.Class
	Version string
	// Built is the build time of the software (RFC 3339; "": unknown).
	Built string
	// HostName returns the name shown in the prompt.
	HostName func() string
	// Ports lists interface names known from the hardware inventory, for
	// completion (may be nil).
	Ports func() []string
	// Ops supplies live data for show/clear commands (nil: unavailable).
	Ops Operational
	// Logs supplies "show log" and "show system syslog" (nil: unavailable).
	Logs Logs
	Log  *slog.Logger
	// Stack runs operational commands on other members (nil: standalone).
	Stack Stack
	// Origin is the member the session is connected to when it was
	// forwarded to this member, the master (reference 1.8; 0: a session
	// of this member). "local" means that member.
	Origin int
	// Role returns this member's role in a stack with more than one
	// member, e.g. "master:1" or "backup:2", shown above the prompt as in
	// Junos VC ("": standalone; nil: none).
	Role func() string
}

// Stack is what the CLI needs to run commands on other stack members.
type Stack interface {
	// Self is this member's id; Members lists all members (sorted).
	Self() int
	Members() []int
	// Exec runs an operational command line on member as the session's
	// user; confirmed answers its questions with yes.
	Exec(ctx context.Context, member int, line string, confirmed bool) (string, error)
}

// Reply is the result of executing one line.
type Reply struct {
	Output string
	// NoMore disables the pager for this output (| no-more).
	NoMore bool
	// Exit ends the CLI session.
	Exit bool
	// Shell asks the client to run a Linux shell as the logged-in user
	// ("start shell") and to return to the CLI when it exits.
	Shell bool
	// ShellMember: the shell runs on this member, not where the client is
	// (0: where the client is).
	ShellMember int
}

// Shell is one user's CLI session.
type Shell struct {
	env  Env
	sess *commit.Session // nil in operational mode
	// edit is the current edit level; levels holds the previous levels for
	// "exit".
	edit   []config.Step
	levels [][]config.Step
	// plainErrors: errors without the caret line (the command runs for
	// another member, whose section does not show the command line).
	plainErrors bool
}

// SetPlainErrors makes the shell report errors as "error: <text>" without
// pointing into the command line (commands run for another member).
func (sh *Shell) SetPlainErrors() { sh.plainErrors = true }

// New creates a shell in operational mode.
func New(env Env) *Shell {
	if env.Log == nil {
		env.Log = slog.Default()
	}
	if env.HostName == nil {
		env.HostName = func() string { return "switch" }
	}
	return &Shell{env: env}
}

// ActiveSeq is the revision number of the active configuration.
func (sh *Shell) ActiveSeq() uint64 { return sh.env.Engine.ActiveSeq() }

// InConfig reports whether the shell is in configuration mode.
func (sh *Shell) InConfig() bool { sh.checkSession(); return sh.sess != nil }

// checkSession returns to operational mode if the engine ended the
// configuration session (mastership moved to another member).
func (sh *Shell) checkSession() {
	if sh.sess != nil && sh.sess.Closed() {
		sh.sess, sh.edit, sh.levels = nil, nil, nil
	}
}

// Prompt returns the prompt for the next line.
func (sh *Shell) Prompt() string {
	sh.checkSession()
	c := ">"
	if sh.sess != nil {
		c = "#"
	}
	return sh.env.User + "@" + sh.env.HostName() + c + " "
}

// origin returns the member a forwarded session is connected to (0: this
// member).
func (sh *Shell) origin() int {
	if sh.env.Stack == nil || sh.env.Origin == sh.env.Stack.Self() {
		return 0
	}
	return sh.env.Origin
}

// Banner returns the lines shown above the prompt: the edit level in
// configuration mode and a pending commit confirmation.
func (sh *Shell) Banner() string {
	var b strings.Builder
	if sh.env.Role != nil {
		if r := sh.env.Role(); r != "" {
			if o := sh.origin(); o != 0 {
				r += fmt.Sprintf(", connected to member %d", o)
			}
			b.WriteString("{" + r + "}\n")
		}
	}
	if p := sh.env.Engine.Pending(); p != nil {
		left := time.Until(p.Deadline)
		if left < 0 {
			left = 0
		}
		fmt.Fprintf(&b, "[commit pending confirmation: %dm left]\n", int((left+time.Minute-1)/time.Minute))
	}
	if sh.sess != nil {
		b.WriteString(editHeader(sh.edit) + "\n")
	}
	return b.String()
}

func editHeader(steps []config.Step) string {
	if len(steps) == 0 {
		return "[edit]"
	}
	return "[edit " + config.PathString(steps) + "]"
}

// Close ends the session, e.g. when the connection drops. A private or
// exclusive candidate is discarded, the shared candidate is kept.
func (sh *Shell) Close() {
	if sh.sess != nil {
		sh.sess.Close()
		sh.sess = nil
	}
}

// posError is an error at a byte position of the command line; it is
// shown with a caret under that position.
type posError struct {
	pos int
	msg string
}

func (e *posError) Error() string { return e.msg }

// call carries the state of one command execution.
type call struct {
	ctx   context.Context
	line  string
	args  []config.Token
	out   *strings.Builder
	term  Terminal
	pipes *pipeline
	reply *Reply
	// only restricts a stack-wide listing to these members' interfaces
	// (nil: all).
	only []int
}

// shows reports whether a stack-wide listing includes the row of
// interface name: interfaces of other members than the target are left
// out; bundles, irb and cme belong to the whole stack.
func (c *call) shows(name string) bool {
	if c.only == nil {
		return true
	}
	p, ok := schema.ParsePhysical(strings.SplitN(name, ".", 2)[0])
	return !ok || slices.Contains(c.only, p.Member)
}

// argPos returns the byte position of argument i (or the end of the line).
func (c *call) argPos(i int) int {
	if i < len(c.args) {
		return c.args[i].Pos
	}
	return len(c.line)
}

// pathErr converts a Resolve error on toks into a positioned error.
func (c *call) pathErr(toks []config.Token, err error) error {
	var pe *config.PathError
	if errors.As(err, &pe) {
		pos := len(c.line)
		if pe.Tok < len(toks) {
			pos = toks[pe.Tok].Pos
		}
		return &posError{pos: pos, msg: pe.Msg}
	}
	return err
}

// Execute runs one command line. It never panics: an internal error is
// logged and reported, and the session stays usable.
func (sh *Shell) Execute(ctx context.Context, line string, term Terminal) (rep Reply) {
	var out strings.Builder
	defer func() {
		if r := recover(); r != nil {
			sh.env.Log.Error("cli: internal error", "user", sh.env.User, "line", line, "panic", r, "stack", string(debug.Stack()))
			rep = Reply{Output: out.String() + "error: internal error while executing the command (logged); the session is still usable\n"}
		}
	}()
	if len(line) > MaxLineLength {
		return Reply{Output: "error: command line too long\n"}
	}
	toks, err := config.Lex(line, config.LexCommand)
	if err != nil {
		return Reply{Output: renderErr(sh.Prompt(), &posError{pos: errPos(err), msg: err.Error()})}
	}
	if len(toks) == 0 {
		return Reply{}
	}
	sh.checkSession()
	cmdToks, pipeSegs := splitPipes(toks)
	c := &call{ctx: ctx, line: line, out: &out, term: term, reply: &rep}
	c.pipes, err = parsePipes(pipeSegs, line)
	if err == nil {
		cmds := operational
		if sh.sess != nil {
			cmds = configuration
		}
		err = sh.dispatch(c, cmds, cmdToks)
	}
	if err != nil {
		if sh.plainErrors {
			out.WriteString("error: " + err.Error() + "\n")
		} else {
			out.WriteString(renderErr(sh.Prompt(), err))
		}
		rep.Output = out.String()
		return rep
	}
	rep.Output = c.pipes.filter(out.String())
	rep.NoMore = rep.NoMore || c.pipes.noMore
	return rep
}

func errPos(err error) int {
	var le *config.LexError
	if errors.As(err, &le) {
		return le.Pos
	}
	return 0
}

// renderErr formats an error; positioned errors get a caret line aligned
// with the command as typed after the prompt.
func renderErr(prompt string, err error) string {
	var pe *posError
	if errors.As(err, &pe) {
		return strings.Repeat(" ", len(prompt)+pe.pos) + "^\n" + pe.msg + "\n"
	}
	return "error: " + err.Error() + "\n"
}

// dispatch finds and runs the command addressed by toks.
func (sh *Shell) dispatch(c *call, cmds []*command, toks []config.Token) error {
	for {
		if len(toks) == 0 {
			return &posError{pos: len(c.line), msg: "missing argument"}
		}
		tk := toks[0]
		if tk.Punct || tk.Quoted {
			return &posError{pos: tk.Pos, msg: "syntax error"}
		}
		m := lookupCmd(cmds, tk.Text)
		switch len(m) {
		case 0:
			return &posError{pos: tk.Pos, msg: "syntax error, expecting <command>"}
		case 1:
		default:
			names := make([]string, len(m))
			for i, x := range m {
				names[i] = x.name
			}
			return &posError{pos: tk.Pos, msg: fmt.Sprintf("syntax error, %q is ambiguous: %s", tk.Text, strings.Join(names, ", "))}
		}
		cmd := m[0]
		if sh.env.Class > cmd.class {
			return &posError{pos: tk.Pos, msg: "permission denied: " + cmd.name}
		}
		toks = toks[1:]
		if len(cmd.sub) > 0 && (cmd.run == nil || (len(toks) > 0 && len(lookupCmd(cmd.sub, toks[0].Text)) > 0)) {
			cmds = cmd.sub
			continue
		}
		if (c.pipes.display != "" || c.pipes.compare) && !cmd.display {
			return errors.New("'| display' and '| compare' are only valid for show commands")
		}
		if cmd.stackWide && sh.env.Stack != nil {
			targets, _, rest, err := sh.parseTarget(toks)
			if err != nil {
				return err
			}
			n := len(toks)
			if n > 0 && isTargetWord(toks[n-1], "local") && targets == nil {
				targets = []int{sh.env.Stack.Self()}
			}
			if n > 1 && isTargetWord(toks[n-2], "member") && targets == nil {
				targets = []int{sh.env.Stack.Self()}
			}
			if !(n > 0 && isTargetWord(toks[n-1], "all-members")) {
				c.only = targets
			}
			toks = rest
		} else if cmd.perMember && sh.env.Stack != nil {
			explicit := len(toks) > 0 && (isTargetWord(toks[len(toks)-1], "local") || isTargetWord(toks[len(toks)-1], "all-members") ||
				(len(toks) > 1 && isTargetWord(toks[len(toks)-2], "member")))
			targets, pos, rest, err := sh.parseTarget(toks)
			if err != nil {
				return err
			}
			if !explicit && cmd.allMembers && len(sh.env.Stack.Members()) > 1 {
				// Commands about the whole chassis cover every member by
				// default (as in a Junos virtual chassis).
				targets, pos = sh.env.Stack.Members(), len(c.line)
			}
			if targets != nil {
				return sh.runOnMembers(c, cmd, rest, targets, pos)
			}
			toks = rest
		}
		c.args = toks
		return cmd.run(sh, c)
	}
}

// lookupCmd returns the commands matching word exactly, or else all
// commands it is a prefix of.
func lookupCmd(cmds []*command, word string) []*command {
	var pre []*command
	for _, c := range cmds {
		if c.name == word {
			return []*command{c}
		}
		if strings.HasPrefix(c.name, word) && !c.hidden {
			pre = append(pre, c)
		}
	}
	return pre
}

// baseSchema returns the schema node at the edit level.
func (sh *Shell) baseSchema() *schema.Node {
	if len(sh.edit) == 0 {
		return schema.Root()
	}
	return sh.edit[len(sh.edit)-1].Schema
}

// resolve resolves c.args relative to the edit level and returns the full
// path from the root.
func (sh *Shell) resolve(c *call, toks []config.Token, mode config.ResolveMode) ([]config.Step, error) {
	steps, err := config.ResolveAt(sh.edit, toks, mode)
	if err != nil {
		return nil, c.pathErr(toks, err)
	}
	return steps, nil
}

// NeedsMaster reports whether line, in operational mode, has to run on
// the configuration master (configure, confirm).
func (sh *Shell) NeedsMaster(line string) bool {
	if sh.InConfig() {
		return false
	}
	toks, err := config.Lex(line, config.LexCommand)
	if err != nil || len(toks) == 0 || toks[0].Punct || toks[0].Quoted {
		return false
	}
	m := lookupCmd(operational, toks[0].Text)
	return len(m) == 1 && (m[0].name == "configure" || m[0].name == "confirm")
}
