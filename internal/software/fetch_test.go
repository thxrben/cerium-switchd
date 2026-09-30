package software

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestUSBAndFile(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "sys")
	os.MkdirAll(filepath.Join(sys, "block", "sdb", "sdb1"), 0o755)
	os.WriteFile(filepath.Join(sys, "block", "sdb", "removable"), []byte("1\n"), 0o644)
	mnt := filepath.Join(dir, "mnt")
	var cmds []string
	f := &Fetcher{SysRoot: sys, MountDir: mnt, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		cmds = append(cmds, name+" "+strings.Join(args, " "))
		if name == "mount" {
			os.WriteFile(filepath.Join(mnt, "ceros.tar.gz"), []byte("pkg"), 0o644)
		}
		return nil, nil
	}}
	src, _ := ParseSource("usb:ceros.tar.gz")
	dst := filepath.Join(dir, "out")
	if err := f.Fetch(context.Background(), src, dst, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "pkg" || !strings.Contains(strings.Join(cmds, "\n"), "mount -o ro /dev/sdb1") || !strings.HasPrefix(cmds[len(cmds)-1], "umount") {
		t.Errorf("usb: %q %v", b, cmds)
	}
}
