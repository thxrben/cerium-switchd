package swcli

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestKeyReader(t *testing.T) {
	in := "a\x01\x1b[D\x1b[3~\x1b[1~\x1bOF\x1bb\x1b[200~x\ny\x1b[201~\x03\x04\t\r\x7fü\x1b[99Zq"
	k := newKeyReader(strings.NewReader(in))
	want := []key{
		{r: 'a'}, {code: keyHome}, {code: keyLeft}, {code: keyDelete}, {code: keyHome}, {code: keyEnd},
		{code: keyWordLeft}, {paste: "x\ny"}, {code: keyCtrlC, r: 3}, {code: keyCtrlD}, {code: keyTab},
		{code: keyEnter, r: '\r'}, {code: keyBackspace}, {r: 'ü'}, {r: 'q'}, {eof: true},
	}
	for i, w := range want {
		if got := k.next(); got != w {
			t.Fatalf("key %d: got %+v, want %+v", i, got, w)
		}
	}
}

func testEditor(t *testing.T) *editor {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return newEditor(&ui{out: f}, "u@h> ", &history{lines: []string{"show version", "configure"}})
}

func TestEditorOps(t *testing.T) {
	e := testEditor(t)
	e.insert("set system host-name")
	e.key(key{code: keyWordLeft})
	e.key(key{code: keyKillEnd})
	if string(e.buf) != "set system " {
		t.Fatalf("after kill-end: %q", string(e.buf))
	}
	e.key(key{code: keyKillWord})
	if string(e.buf) != "set " || e.pos != 4 {
		t.Fatalf("after kill-word: %q %d", string(e.buf), e.pos)
	}
	e.key(key{code: keyHome})
	e.insert("x")
	e.key(key{code: keyDelete})
	if string(e.buf) != "xet " {
		t.Fatalf("insert at start + delete: %q", string(e.buf))
	}
	e.key(key{code: keyEnd})
	e.key(key{code: keyBackspace})
	e.key(key{code: keyKillStart})
	if len(e.buf) != 0 {
		t.Fatalf("kill-start: %q", string(e.buf))
	}
	e.insert("draft")
	e.key(key{code: keyUp})
	e.key(key{code: keyUp})
	if string(e.buf) != "show version" {
		t.Fatalf("history: %q", string(e.buf))
	}
	e.key(key{code: keyUp}) // stays at the oldest
	e.key(key{code: keyDown})
	e.key(key{code: keyDown})
	if string(e.buf) != "draft" {
		t.Fatalf("history back to draft: %q", string(e.buf))
	}
	e.insert(strings.Repeat("long ", 40)) // wraps at 80 columns
	if e.row == 0 {
		t.Error("cursor row not tracked for wrapped lines")
	}
}

func TestHelpers(t *testing.T) {
	if cp := commonPrefix([]string{"interfaces", "interface-range"}); cp != "interface" {
		t.Errorf("commonPrefix = %q", cp)
	}
	for s, want := range map[string]bool{`set x "a b`: true, `set x "a b"`: false, `"a\"`: true, `"a\""`: false} {
		if inQuote(s) != want {
			t.Errorf("inQuote(%q) != %v", s, want)
		}
	}
	h := &history{}
	h.add("a")
	h.add("a")
	if len(h.lines) != 1 {
		t.Error("duplicate history entry")
	}
}

func TestFilterCrash(t *testing.T) {
	var shown strings.Builder
	in := "error: something\nSIGSEGV: segmentation violation\nPC=0x1\ngoroutine 1 [running]:\n"
	rep := filterCrash(strings.NewReader(in), &shown)
	if shown.String() != "error: something\n" || !strings.HasPrefix(string(rep), "SIGSEGV") || !strings.Contains(string(rep), "goroutine 1") {
		t.Errorf("shown %q report %q", shown.String(), rep)
	}
	for script, want := range map[string]bool{"exit 0": false, "exit 1": false, "exit 2": true,
		"kill -HUP $$": false, "kill -SEGV $$": true, "kill -KILL $$": true} {
		if _, crashed := exitStatus(exec.Command("sh", "-c", script).Run()); crashed != want {
			t.Errorf("%s: crashed = %v", script, crashed)
		}
	}
}

// The key reader takes nothing from the input beyond the key it returns
// (type-ahead before 'start shell' must reach the shell).
func TestKeyReaderNoReadAhead(t *testing.T) {
	r := strings.NewReader("start shell\rhostname\r")
	k := newKeyReader(r)
	for range len("start shell\r") {
		k.next()
	}
	if rest, _ := io.ReadAll(r); string(rest) != "hostname\r" {
		t.Fatalf("read ahead: %q left", rest)
	}
}
