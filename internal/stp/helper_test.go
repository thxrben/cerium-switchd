//go:build linux

package stp

import (
	"os"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/names"
	"github.com/thxrben/cerium-switchd/pkg/netdev"
)

// The image ships the kernel's STP helper (its root is read-only): it must
// be exactly what cer-rstpd expects (/sbin is /usr/sbin on Debian).
func TestImageSTPHelper(t *testing.T) {
	const path = "../../image/overlay/usr/sbin/bridge-stp"
	want := netdev.STPHelperText(names.Bridge)
	if os.Getenv("WRITE_HELPER") != "" {
		os.WriteFile(path, []byte(want), 0o755)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s:\n%s\nwant\n%s", path, got, want)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s is not executable", path)
	}
}
