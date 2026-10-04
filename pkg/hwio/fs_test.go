package hwio

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An unchanged file is not written again (slow system disks).
func TestWriteFileAtomicUnchanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFileAtomic(p, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
	if err := WriteFileAtomic(p, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); !fi.ModTime().Equal(old) {
		t.Fatal("unchanged content was written again")
	}
	WriteFileAtomic(p, []byte("b"), 0o600)
	if raw, _ := os.ReadFile(p); string(raw) != "b" {
		t.Fatalf("changed content not written: %q", raw)
	}
}
