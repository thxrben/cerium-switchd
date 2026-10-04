package access

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The same secret gives the same keys on every member; ssh-keygen reads
// them and computes the same public keys.
func TestHostKeys(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	secret := bytes.Repeat([]byte{7}, 32)
	if ch, err := WriteHostKeys(a, secret); err != nil || !ch {
		t.Fatal(ch, err)
	}
	if ch, err := WriteHostKeys(a, secret); err != nil || ch {
		t.Fatalf("unchanged keys rewritten: %v %v", ch, err)
	}
	WriteHostKeys(b, secret)
	for _, f := range []string{"ssh_host_ed25519_key", "ssh_host_ecdsa_key", "ssh_host_ed25519_key.pub", "ssh_host_ecdsa_key.pub"} {
		x, _ := os.ReadFile(filepath.Join(a, f))
		y, _ := os.ReadFile(filepath.Join(b, f))
		if len(x) == 0 || !bytes.Equal(x, y) {
			t.Fatalf("%s differs between two members", f)
		}
	}
	if fi, _ := os.Stat(filepath.Join(a, "ssh_host_ed25519_key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode %v", fi.Mode())
	}
	other := t.TempDir()
	WriteHostKeys(other, bytes.Repeat([]byte{8}, 32))
	x, _ := os.ReadFile(filepath.Join(a, "ssh_host_ed25519_key.pub"))
	y, _ := os.ReadFile(filepath.Join(other, "ssh_host_ed25519_key.pub"))
	if bytes.Equal(x, y) {
		t.Fatal("another stack has the same keys")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	for _, k := range []string{"ssh_host_ed25519_key", "ssh_host_ecdsa_key"} {
		out, err := exec.Command("ssh-keygen", "-y", "-f", filepath.Join(a, k)).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh-keygen cannot read %s: %v %s", k, err, out)
		}
		pub, _ := os.ReadFile(filepath.Join(a, k+".pub"))
		f := strings.Fields(string(pub))
		if got := strings.Fields(string(out)); len(got) < 2 || got[0] != f[0] || got[1] != f[1] {
			t.Fatalf("%s: ssh-keygen derives %q, the .pub says %q", k, out, pub)
		}
	}
}
