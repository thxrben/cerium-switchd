package daemon

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/lib/software/software"
)

func testStore(t *testing.T, avail uint64) (*bundleStore, *uint64) {
	t.Helper()
	var size uint64
	s := &bundleStore{dir: t.TempDir(), log: slog.New(slog.DiscardHandler),
		mount:     func(_ string, n uint64) error { size = n; return nil },
		available: func() (uint64, error) { return avail, nil },
		slot:      func() uint64 { return 0 }}
	return s, &size
}

func TestBundleStoreRoom(t *testing.T) {
	s, mounted := testStore(t, 1<<30)
	old := s.path("1.0")
	os.WriteFile(old, make([]byte, 100<<20), 0o600)
	// Without slots: what is available less 256 MB, plus the bundles a new
	// one replaces.
	room, err := s.prepare(0)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(1<<30 + 100<<20 - keepFree); room != want {
		t.Fatalf("room %d, want %d", room, want)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the old bundle was not replaced")
	}
	if *mounted != room+1<<20 {
		t.Fatalf("tmpfs size %d", *mounted)
	}
	// Too large before the transfer.
	if _, err := s.prepare(2 << 30); !errors.Is(err, software.ErrTooLarge) {
		t.Fatalf("err %v", err)
	}
	// A kept bundle stays and takes its share of the room.
	up := s.path("upload")
	os.WriteFile(up, make([]byte, 10<<20), 0o600)
	if _, err := s.prepare(0, up); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(up); err != nil {
		t.Fatal("the kept bundle was removed")
	}
	// With an update slot: the slot, less what is kept.
	s.slot = func() uint64 { return 516 << 20 }
	if room, _ := s.prepare(0, up); room != 506<<20 {
		t.Fatalf("slot room %d", room>>20)
	}
	// Little memory: no room.
	s.slot = func() uint64 { return 0 }
	s.available = func() (uint64, error) { return 200 << 20, nil }
	if room, _ := s.room(up); room != 0 {
		t.Fatalf("room %d", room)
	}
}

func TestBundleStoreSweep(t *testing.T) {
	s, _ := testStore(t, 1<<30)
	a, b := s.path("1.0"), s.path("1.1")
	os.WriteFile(a, []byte("a"), 0o600)
	os.WriteFile(b, []byte("b"), 0o600)
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(a, past, past)
	os.Chtimes(b, past, past)
	s.touch(b)
	s.sweep(true) // an update runs: nothing goes
	if _, err := os.Stat(a); err != nil {
		t.Fatal("removed while busy")
	}
	s.sweep(false)
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Fatal("unused bundle kept")
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatal("used bundle removed")
	}
	if filepath.Dir(a) != s.dir {
		t.Fatal(a)
	}
}
