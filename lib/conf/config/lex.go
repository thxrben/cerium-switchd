package config

import (
	"fmt"
	"strings"
)

// Token is one lexical element of a command line or configuration text.
type Token struct {
	Text   string
	Pos    int  // byte offset of the first character in the input
	End    int  // byte offset after the last character
	Quoted bool // the token was written in double quotes
	Punct  bool // one of [ ] { } ; |
}

// LexError reports a lexical problem at a byte position.
type LexError struct {
	Pos int
	Msg string
}

func (e *LexError) Error() string { return fmt.Sprintf("%s at position %d", e.Msg, e.Pos) }

// LexMode selects which characters are punctuation.
type LexMode int

const (
	// LexCommand is used for CLI lines: "[", "]" and "|" are punctuation.
	LexCommand LexMode = iota
	// LexConfig is used for configuration text: "[", "]", "{", "}", ";"
	// are punctuation and "#" / "/* */" start comments.
	LexConfig
)

// Lex splits input into tokens. Unterminated quotes are reported as errors
// but the tokens seen so far are returned as well, so that completion can
// still work on partial input.
func Lex(input string, mode LexMode) ([]Token, error) {
	var toks []Token
	i := 0
	n := len(input)
	isPunct := func(c byte) bool {
		switch c {
		case '[', ']':
			return true
		case '|':
			return mode == LexCommand
		case '{', '}', ';':
			return mode == LexConfig
		}
		return false
	}
	for i < n {
		c := input[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case mode == LexConfig && c == '#':
			for i < n && input[i] != '\n' {
				i++
			}
		case mode == LexConfig && c == '/' && i+1 < n && input[i+1] == '*':
			end := strings.Index(input[i+2:], "*/")
			if end < 0 {
				return toks, &LexError{Pos: i, Msg: "unterminated comment"}
			}
			i += end + 4
		case isPunct(c):
			toks = append(toks, Token{Text: string(c), Pos: i, End: i + 1, Punct: true})
			i++
		case c == '"':
			start := i
			i++
			var b strings.Builder
			closed := false
			for i < n {
				ch := input[i]
				if ch == '\\' && i+1 < n {
					b.WriteByte(input[i+1])
					i += 2
					continue
				}
				if ch == '"' {
					closed = true
					i++
					break
				}
				b.WriteByte(ch)
				i++
			}
			toks = append(toks, Token{Text: b.String(), Pos: start, End: i, Quoted: true})
			if !closed {
				return toks, &LexError{Pos: start, Msg: "unterminated quoted string"}
			}
		default:
			start := i
			for i < n {
				ch := input[i]
				if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || ch == '"' || isPunct(ch) {
					break
				}
				i++
			}
			toks = append(toks, Token{Text: input[start:i], Pos: start, End: i})
		}
	}
	return toks, nil
}

// Quote renders a value so that Lex reads it back as one token.
func Quote(s string) string {
	if s == "" {
		return `""`
	}
	needs := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '"', '\\', '[', ']', '{', '}', ';', '|', '#', '\n', '\r':
			needs = true
		}
	}
	if strings.HasPrefix(s, "/*") || IsDirective(s) {
		needs = true
	}
	if !needs {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}
