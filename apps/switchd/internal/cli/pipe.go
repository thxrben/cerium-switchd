package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/lib/conf/config"
)

// pipeline holds the parsed "| …" modifiers of a command line.
type pipeline struct {
	display    string // "", "set" or "json"
	relative   bool   // display set relative
	compare    bool
	compareRev int
	noMore     bool
	filters    []func(lines []string) []string
}

// pipeCommands are the pipe keywords, for parsing and completion.
var pipeCommands = []struct{ name, help string }{
	{"compare", "Compare configuration changes with a prior revision"},
	{"count", "Count occurrences"},
	{"display", "Show additional kinds of information"},
	{"except", "Show only text that does not match a pattern"},
	{"find", "Search for the first occurrence of a pattern"},
	{"last", "Display the end of the output"},
	{"match", "Show only text that matches a pattern"},
	{"no-more", "Don't paginate output"},
}

// splitPipes splits tokens at unquoted "|".
func splitPipes(toks []config.Token) (cmd []config.Token, pipes [][]config.Token) {
	cur := &cmd
	for _, t := range toks {
		if t.Punct && t.Text == "|" {
			pipes = append(pipes, nil)
			cur = &pipes[len(pipes)-1]
			continue
		}
		*cur = append(*cur, t)
	}
	return cmd, pipes
}

func parsePipes(segs [][]config.Token, line string) (*pipeline, error) {
	p := &pipeline{}
	for _, seg := range segs {
		if len(seg) == 0 {
			return nil, &posError{pos: len(line), msg: "missing pipe command"}
		}
		var names []string
		for _, pc := range pipeCommands {
			if strings.HasPrefix(pc.name, seg[0].Text) {
				names = append(names, pc.name)
			}
		}
		if len(names) != 1 || seg[0].Quoted {
			return nil, &posError{pos: seg[0].Pos, msg: "syntax error, expecting a pipe command"}
		}
		args := seg[1:]
		argPos := func(i int) int {
			if i < len(args) {
				return args[i].Pos
			}
			return len(line)
		}
		regexArg := func() (*regexp.Regexp, error) {
			if len(args) != 1 {
				return nil, &posError{pos: argPos(1), msg: "expecting one pattern (quote it if it contains spaces)"}
			}
			re, err := regexp.Compile(args[0].Text)
			if err != nil {
				return nil, &posError{pos: args[0].Pos, msg: "invalid pattern: " + err.Error()}
			}
			return re, nil
		}
		switch names[0] {
		case "match", "except", "find":
			re, err := regexArg()
			if err != nil {
				return nil, err
			}
			p.filters = append(p.filters, lineFilter(names[0], re))
		case "last":
			n := 10
			if len(args) > 1 {
				return nil, &posError{pos: argPos(1), msg: "syntax error"}
			}
			if len(args) == 1 {
				v, err := strconv.Atoi(args[0].Text)
				if err != nil || v < 1 {
					return nil, &posError{pos: args[0].Pos, msg: "expecting a number of lines"}
				}
				n = v
			}
			p.filters = append(p.filters, func(l []string) []string {
				if len(l) > n {
					return l[len(l)-n:]
				}
				return l
			})
		case "count":
			if len(args) > 0 {
				return nil, &posError{pos: argPos(0), msg: "syntax error"}
			}
			p.filters = append(p.filters, func(l []string) []string {
				return []string{fmt.Sprintf("Count: %d lines", len(l))}
			})
		case "no-more":
			if len(args) > 0 {
				return nil, &posError{pos: argPos(0), msg: "syntax error"}
			}
			p.noMore = true
		case "display":
			if p.display != "" {
				return nil, &posError{pos: seg[0].Pos, msg: "duplicate '| display'"}
			}
			switch {
			case len(args) == 1 && prefixOf(args[0].Text, "set"):
				p.display = "set"
			case len(args) == 2 && prefixOf(args[0].Text, "set") && prefixOf(args[1].Text, "relative"):
				p.display, p.relative = "set", true
			case len(args) == 1 && prefixOf(args[0].Text, "json"):
				p.display = "json"
			default:
				return nil, &posError{pos: argPos(0), msg: "syntax error, expecting 'set', 'set relative' or 'json'"}
			}
		case "compare":
			p.compare = true
			switch {
			case len(args) == 0:
			case len(args) == 2 && prefixOf(args[0].Text, "rollback"):
				v, err := strconv.Atoi(args[1].Text)
				if err != nil || v < 0 {
					return nil, &posError{pos: args[1].Pos, msg: "expecting a revision number"}
				}
				p.compareRev = v
			default:
				return nil, &posError{pos: argPos(0), msg: "syntax error, expecting 'rollback <n>'"}
			}
		}
	}
	if p.compare && p.display != "" {
		return nil, fmt.Errorf("'| compare' cannot be combined with '| display'")
	}
	return p, nil
}

func prefixOf(word, full string) bool { return word != "" && strings.HasPrefix(full, word) }

func lineFilter(kind string, re *regexp.Regexp) func([]string) []string {
	return func(lines []string) []string {
		var out []string
		for i, l := range lines {
			m := re.MatchString(l)
			switch kind {
			case "match":
				if m {
					out = append(out, l)
				}
			case "except":
				if !m {
					out = append(out, l)
				}
			case "find":
				if m {
					return lines[i:]
				}
			}
		}
		return out
	}
}

// filter applies the text filters to the command output.
func (p *pipeline) filter(out string) string {
	if len(p.filters) == 0 || out == "" {
		return out
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	for _, f := range p.filters {
		lines = f(lines)
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
