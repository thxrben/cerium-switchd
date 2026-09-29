package config

import (
	"errors"
)

// LoadMode selects how Load combines text with the candidate.
type LoadMode int

const (
	// LoadMerge adds the text: leaves are overwritten, leaf-lists extended,
	// nothing is removed.
	LoadMerge LoadMode = iota
	// LoadReplace is LoadMerge plus the replace: and delete: directives.
	LoadReplace
	// LoadOverride replaces the entire configuration.
	LoadOverride
	// LoadSet executes set/delete/activate/deactivate lines.
	LoadSet
)

func (m LoadMode) String() string {
	return [...]string{"merge", "replace", "override", "set"}[m]
}

// IsSetFormat reports whether text is in set format: its first word is a
// configuration command rather than a statement.
func IsSetFormat(text string) bool {
	toks, _ := Lex(text, LexConfig)
	if len(toks) == 0 || toks[0].Quoted {
		return false
	}
	switch toks[0].Text {
	case "set", "delete", "activate", "deactivate":
		return true
	}
	return false
}

// Load applies text to t relative to the edit level base. It is all or
// nothing: on error t is unchanged and the error names the line.
func Load(t *Tree, mode LoadMode, text string, base []Step) error {
	var work *Tree
	switch mode {
	case LoadOverride:
		if len(base) > 0 {
			return errors.New("load override is only allowed at the top level")
		}
		work = New()
	default:
		work = t.Clone()
	}
	var err error
	if mode == LoadSet || IsSetFormat(text) {
		err = ApplySetLinesAt(work, text, base)
	} else {
		err = parseCurly(work, text, base, mode == LoadReplace)
	}
	if err != nil {
		return err
	}
	work.Normalize()
	t.Root = work.Root
	return nil
}
