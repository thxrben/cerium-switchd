package software

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/usbstore"
)

func TestSourcesAndCurl(t *testing.T) {
	for _, bad := range []string{"gopher://x/y", "usb:", "usb:../x", "relative/file", "http://"} {
		if _, err := ParseSource(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	var got []string
	f := &Fetcher{VRF: "oob", Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return nil, nil
	}}
	src, err := ParseSource("sftp://admin@files.example/pub/ceros.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Fetch(context.Background(), src, "/tmp/x", "secret"); err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(got, " ")
	for _, want := range []string{"ip vrf exec oob curl", "-u admin:secret", "-o /tmp/x", "sftp://files.example/pub/ceros.tar.gz"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("curl command lacks %q: %s", want, cmd)
		}
	}
}

// fakeUSB is a stick with one file.
type fakeUSB map[string]string

func (u fakeUSB) CopyTo(p string, w io.Writer, max int64) (int64, error) {
	b, ok := u[p]
	if !ok {
		return 0, os.ErrNotExist
	}
	if int64(len(b)) > max {
		return 0, usbstore.ErrTooLarge
	}
	n, err := io.WriteString(w, b)
	return int64(n), err
}

func TestUSBAndFile(t *testing.T) {
	dir := t.TempDir()
	f := &Fetcher{USB: fakeUSB{"ceros.bundle": "pkg"}}
	src, _ := ParseSource("usb:ceros.bundle")
	dst := filepath.Join(dir, "out")
	if err := f.Fetch(context.Background(), src, dst, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "pkg" {
		t.Errorf("usb: %q", b)
	}
	f.MaxSize = 2
	if err := f.Fetch(context.Background(), src, dst, ""); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
}
