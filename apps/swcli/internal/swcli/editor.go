package swcli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"unicode"

	"golang.org/x/term"
)

// Key codes for non-printable keys.
const (
	keyNone = iota
	keyEnter
	keyTab
	keyCtrlC
	keyCtrlD
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyKillEnd   // Ctrl-K
	keyKillStart // Ctrl-U
	keyKillWord  // Ctrl-W
	keyWordLeft  // Alt-b
	keyWordRight // Alt-f
	keyClear     // Ctrl-L
)

type key struct {
	r     rune // printable character (code == keyNone)
	code  int
	paste string // bracketed paste content
	eof   bool
}

type keyReader struct {
	r *bufio.Reader
}

// newKeyReader reads keys from in one byte per read: nothing typed ahead
// is taken from the terminal beyond the key being handled, so what was
// typed while a shell started reaches the shell (it stayed in this buffer
// and went to the CLI after the shell).
func newKeyReader(in io.Reader) *keyReader { return &keyReader{r: bufio.NewReader(oneByte{in})} }

// oneByte limits every read to one byte.
type oneByte struct{ r io.Reader }

func (o oneByte) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return o.r.Read(p)
}

func (k *keyReader) next() key {
	for {
		r, _, err := k.r.ReadRune()
		if err != nil {
			return key{eof: true}
		}
		switch r {
		case 0x01:
			return key{code: keyHome}
		case 0x02:
			return key{code: keyLeft}
		case 0x03:
			return key{code: keyCtrlC, r: 3}
		case 0x04:
			return key{code: keyCtrlD}
		case 0x05:
			return key{code: keyEnd}
		case 0x06:
			return key{code: keyRight}
		case 0x08, 0x7f:
			return key{code: keyBackspace}
		case 0x09:
			return key{code: keyTab}
		case 0x0a, 0x0d:
			return key{code: keyEnter, r: '\r'}
		case 0x0b:
			return key{code: keyKillEnd}
		case 0x0c:
			return key{code: keyClear}
		case 0x0e:
			return key{code: keyDown}
		case 0x10:
			return key{code: keyUp}
		case 0x15:
			return key{code: keyKillStart}
		case 0x17:
			return key{code: keyKillWord}
		case 0x1b:
			if kk, ok := k.escape(); ok {
				return kk
			}
			continue
		}
		if unicode.IsPrint(r) {
			return key{r: r}
		}
	}
}

// escape parses an escape sequence after ESC.
func (k *keyReader) escape() (key, bool) {
	b, err := k.r.ReadByte()
	if err != nil {
		return key{eof: true}, true
	}
	switch b {
	case 'b':
		return key{code: keyWordLeft}, true
	case 'f':
		return key{code: keyWordRight}, true
	case 'O':
		c, _ := k.r.ReadByte()
		switch c {
		case 'H':
			return key{code: keyHome}, true
		case 'F':
			return key{code: keyEnd}, true
		}
		return key{}, false
	case '[':
	default:
		return key{}, false
	}
	var params []byte
	for {
		c, err := k.r.ReadByte()
		if err != nil {
			return key{eof: true}, true
		}
		if c >= 0x40 && c <= 0x7e {
			p := string(params)
			switch c {
			case 'A':
				return key{code: keyUp}, true
			case 'B':
				return key{code: keyDown}, true
			case 'C':
				return key{code: keyRight}, true
			case 'D':
				return key{code: keyLeft}, true
			case 'H':
				return key{code: keyHome}, true
			case 'F':
				return key{code: keyEnd}, true
			case '~':
				switch p {
				case "1", "7":
					return key{code: keyHome}, true
				case "4", "8":
					return key{code: keyEnd}, true
				case "3":
					return key{code: keyDelete}, true
				case "200":
					return key{paste: k.readPaste()}, true
				}
			}
			return key{}, false
		}
		params = append(params, c)
		if len(params) > 16 {
			return key{}, false
		}
	}
}

// readPaste reads until the end-of-paste marker ESC [ 201 ~.
func (k *keyReader) readPaste() string {
	const end = "\x1b[201~"
	var b strings.Builder
	for b.Len() < 4<<20 {
		c, err := k.r.ReadByte()
		if err != nil {
			break
		}
		b.WriteByte(c)
		if strings.HasSuffix(b.String(), end) {
			return strings.TrimSuffix(b.String(), end)
		}
	}
	return b.String()
}

// editor is the line being edited. All methods require ui.mu.
type editor struct {
	u      *ui
	prompt string
	buf    []rune
	pos    int
	row    int // cursor row relative to the prompt's first row
	hist   *history
	hidx   int
	saved  []rune
}

func newEditor(u *ui, prompt string, h *history) *editor {
	return &editor{u: u, prompt: prompt, hist: h, hidx: len(h.lines)}
}

func (e *editor) width() int {
	w, _, err := term.GetSize(int(e.u.out.Fd()))
	if err != nil || w < 10 {
		return 80
	}
	return w
}

// clear moves to the prompt's first row and erases the line.
func (e *editor) clear() {
	if e.row > 0 {
		e.u.write(fmt.Sprintf("\x1b[%dA", e.row))
	}
	e.u.write("\r\x1b[J")
	e.row = 0
}

// redraw repaints prompt and buffer and places the cursor.
func (e *editor) redraw() {
	e.clear()
	w := e.width()
	plen := len([]rune(e.prompt))
	total := plen + len(e.buf)
	_, _ = io.WriteString(e.u.out, e.prompt+string(e.buf))
	if total > 0 && total%w == 0 {
		_, _ = io.WriteString(e.u.out, "\r\n") // leave the pending-wrap state
	}
	endRow := total / w
	target := plen + e.pos
	tr, tc := target/w, target%w
	if endRow > tr {
		e.u.write(fmt.Sprintf("\x1b[%dA", endRow-tr))
	}
	e.u.write("\r")
	if tc > 0 {
		e.u.write(fmt.Sprintf("\x1b[%dC", tc))
	}
	e.row = tr
}

// end places the cursor after the last character.
func (e *editor) end() {
	e.pos = len(e.buf)
	e.redraw()
}

func (e *editor) insert(s string) {
	rs := []rune(s)
	atEnd := e.pos == len(e.buf)
	e.buf = append(e.buf[:e.pos], append(rs, e.buf[e.pos:]...)...)
	e.pos += len(rs)
	// Typing at the end of the line only echoes the characters (cheap on
	// slow serial consoles), unless the line wraps.
	if atEnd {
		w := e.width()
		total := len([]rune(e.prompt)) + len(e.buf)
		if (total-len(rs))/w == total/w && total%w != 0 {
			e.u.write(s)
			return
		}
	}
	e.redraw()
}

func (e *editor) deleteRight() {
	if e.pos < len(e.buf) {
		e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
		e.redraw()
	}
}

func (e *editor) key(k key) {
	switch k.code {
	case keyNone:
		if k.r != 0 {
			e.insert(string(k.r))
		}
		return
	case keyBackspace:
		if e.pos > 0 {
			e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
			e.pos--
		}
	case keyDelete:
		e.deleteRight()
		return
	case keyLeft:
		if e.pos > 0 {
			e.pos--
		}
	case keyRight:
		if e.pos < len(e.buf) {
			e.pos++
		}
	case keyHome:
		e.pos = 0
	case keyEnd:
		e.pos = len(e.buf)
	case keyKillEnd:
		e.buf = e.buf[:e.pos]
	case keyKillStart:
		e.buf = append([]rune(nil), e.buf[e.pos:]...)
		e.pos = 0
	case keyKillWord:
		i := e.pos
		for i > 0 && e.buf[i-1] == ' ' {
			i--
		}
		for i > 0 && e.buf[i-1] != ' ' {
			i--
		}
		e.buf = append(e.buf[:i], e.buf[e.pos:]...)
		e.pos = i
	case keyWordLeft:
		for e.pos > 0 && e.buf[e.pos-1] == ' ' {
			e.pos--
		}
		for e.pos > 0 && e.buf[e.pos-1] != ' ' {
			e.pos--
		}
	case keyWordRight:
		for e.pos < len(e.buf) && e.buf[e.pos] == ' ' {
			e.pos++
		}
		for e.pos < len(e.buf) && e.buf[e.pos] != ' ' {
			e.pos++
		}
	case keyUp:
		if e.hidx > 0 {
			if e.hidx == len(e.hist.lines) {
				e.saved = e.buf
			}
			e.hidx--
			e.buf = []rune(e.hist.lines[e.hidx])
			e.pos = len(e.buf)
		}
	case keyDown:
		if e.hidx < len(e.hist.lines) {
			e.hidx++
			if e.hidx == len(e.hist.lines) {
				e.buf = e.saved
			} else {
				e.buf = []rune(e.hist.lines[e.hidx])
			}
			e.pos = len(e.buf)
		}
	case keyClear:
		e.u.write("\x1b[H\x1b[2J")
		e.row = 0
	}
	e.redraw()
}
