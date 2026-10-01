package software

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvRoundTrip(t *testing.T) {
	e := Env{"ORDER": "B A", "A_OK": "1", "B_TRY": "1", "B_ROOTHASH": strings.Repeat("0f", 32)}
	raw, err := e.Bytes()
	if err != nil || len(raw) != EnvSize {
		t.Fatalf("%d bytes, %v", len(raw), err)
	}
	got, err := ParseEnv(raw)
	if err != nil || len(got) != len(e) || got["B_ROOTHASH"] != e["B_ROOTHASH"] {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := (Env{"X": "a\nb"}).Bytes(); err == nil {
		t.Fatal("newline in a value accepted")
	}
	if _, err := (Env{"X": strings.Repeat("a", EnvSize)}).Bytes(); err == nil {
		t.Fatal("oversized block accepted")
	}
}

// A block written by grub-editenv (a header, variables, '#' padding).
func TestEnvGrubFormat(t *testing.T) {
	raw := []byte("# GRUB Environment Block\nORDER=A B\nA_TRY=1\n")
	raw = append(raw, bytes.Repeat([]byte{'#'}, EnvSize-len(raw))...)
	e, err := ParseEnv(raw)
	if err != nil || e["ORDER"] != "A B" || e["A_TRY"] != "1" || len(e) != 2 {
		t.Fatalf("%v %v", e, err)
	}
}

func TestEnvDefaultsAndInPlaceWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grubenv")
	e, err := ReadEnv(path)
	if err != nil || e.Order()[0] != "A" || !e.Slot("A").OK || !e.Slot("B").OK {
		t.Fatalf("defaults %v %v", e, err)
	}
	os.WriteFile(path, []byte("garbage"), 0o644)
	if e, _ = ReadEnv(path); e["ORDER"] != "A B" {
		t.Fatalf("damaged block: %v", e)
	}
	e.Installed("B", &BundleManifest{Version: "2", RootHash: "aa", HashOffset: 4096})
	if err := WriteEnv(path, e); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	got, _ := ReadEnv(path)
	if st.Size() != EnvSize || got.Order()[0] != "B" || got.Slot("B").Version != "2" || got.Slot("B").Tried {
		t.Fatalf("%d %v", st.Size(), got)
	}
}

func TestEnvSlotTransitions(t *testing.T) {
	e := Env{"ORDER": "A B", "A_OK": "1", "B_OK": "1"}
	e.Invalidate("B")
	if e.Slot("B").OK || e.Order()[0] != "A" {
		t.Fatal(e)
	}
	e.Installed("B", &BundleManifest{Version: "2", RootHash: "aa", HashOffset: 4096})
	if !e.Slot("B").OK || !e.Slot("B").First {
		t.Fatal(e)
	}
	e["B_TRY"] = "1" // GRUB started it
	e.Confirm("B")
	if e.Slot("B").Tried {
		t.Fatal(e)
	}
	e.Invalidate("B") // rollback
	if e.Order()[0] != "A" || e.Slot("B").OK {
		t.Fatal(e)
	}
	if got := (Env{"ORDER": "B B junk"}).Order(); strings.Join(got, " ") != "B A" {
		t.Fatalf("order %v", got)
	}
}

// fakeDisk builds a sysfs/dev tree: disk vda with partitions 1-5; vda2 is
// the running slot A.
func fakeDisk(t *testing.T) (sysRoot, devRoot string) {
	t.Helper()
	root := t.TempDir()
	sysRoot, devRoot = filepath.Join(root, "sys"), filepath.Join(root, "dev")
	disk := filepath.Join(sysRoot, "devices/pci/vda")
	os.MkdirAll(filepath.Join(sysRoot, "class/block"), 0o755)
	os.MkdirAll(filepath.Join(devRoot, "disk/by-partuuid"), 0o755)
	for i := 1; i <= 5; i++ {
		n := "vda" + string(rune('0'+i))
		os.MkdirAll(filepath.Join(disk, n), 0o755)
		os.WriteFile(filepath.Join(disk, n, "partition"), []byte(string(rune('0'+i))+"\n"), 0o644)
		os.Symlink(filepath.Join(disk, n), filepath.Join(sysRoot, "class/block", n))
		os.WriteFile(filepath.Join(devRoot, n), nil, 0o644)
	}
	os.Symlink("../../vda2", filepath.Join(devRoot, "disk/by-partuuid/1111-2222"))
	return
}

func TestDetectSystem(t *testing.T) {
	sysRoot, devRoot := fakeDisk(t)
	if _, err := DetectSystem("root=/dev/sda1 quiet", sysRoot, devRoot); err == nil {
		t.Fatal("a system without cerOS slots detected")
	}
	s, err := DetectSystem("ceros.slot=A ceros.part=1111-2222 quiet", sysRoot, devRoot)
	if err != nil {
		t.Fatal(err)
	}
	if s.Active != "A" || s.Backup() != "B" || s.Dev["B"] != filepath.Join(devRoot, "vda3") || s.ESP != filepath.Join(devRoot, "vda1") {
		t.Fatalf("%+v", s)
	}
}

func TestWriteSlot(t *testing.T) {
	sysRoot, devRoot := fakeDisk(t)
	s, err := DetectSystem("ceros.slot=A ceros.part=1111-2222", sysRoot, devRoot)
	if err != nil {
		t.Fatal(err)
	}
	img := bytes.Repeat([]byte{7}, 10000)
	h := sha256.Sum256(img)
	sum := hex.EncodeToString(h[:])
	if err := s.WriteSlot("A", bytes.NewReader(img), int64(len(img)), sum, nil); err == nil {
		t.Fatal("the running slot was written")
	}
	if err := s.WriteSlot("B", bytes.NewReader(img), int64(len(img)), sum, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(s.Dev["B"]); !bytes.Equal(got, img) {
		t.Fatal("slot B content differs")
	}
	if err := s.WriteSlot("B", bytes.NewReader(img[:5000]), int64(len(img)), sum, nil); err == nil {
		t.Fatal("a short image was accepted")
	}
	if err := s.WriteSlot("B", bytes.NewReader(img), int64(len(img)), strings.Repeat("0", 64), nil); err == nil ||
		!strings.Contains(err.Error(), "reads back") {
		t.Fatalf("read-back mismatch not reported: %v", err)
	}
}
