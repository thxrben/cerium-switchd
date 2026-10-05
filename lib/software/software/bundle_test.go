package software

import (
	"bytes"
	"crypto/ed25519"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) (ed25519.PrivateKey, []PublicKey) {
	t.Helper()
	priv, pub, err := GenerateKey("test")
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParsePrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k, []PublicKey{{ID: KeyID(p), Key: p}}
}

// testBundle returns a bundle with an image of 3 blocks + 1 hash block.
func testBundle(t *testing.T, key ed25519.PrivateKey) ([]byte, []byte) {
	t.Helper()
	img := bytes.Repeat([]byte("cerOS-image-data"), 4*4096/16)
	path := filepath.Join(t.TempDir(), "rootfs.img")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	m := BundleManifest{Version: "1.2.3", Built: "2026-10-01T00:00:00Z", Arch: "amd64", Platform: "x86_64-efi",
		Compatible: "ceros-x86_64", RootHash: strings.Repeat("ab", 32), HashOffset: 3 * 4096}
	var b bytes.Buffer
	if err := WriteBundle(&b, m, path, key); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), img
}

func readAll(raw []byte, keys []PublicKey) ([]byte, *Bundle, error) {
	b, err := OpenBundle(bytes.NewReader(raw), keys)
	if err != nil {
		return nil, nil, err
	}
	r, err := b.Image()
	if err != nil {
		return nil, b, err
	}
	img, err := io.ReadAll(r)
	return img, b, err
}

func TestBundleRoundTrip(t *testing.T) {
	key, keys := testKey(t)
	raw, want := testBundle(t, key)
	img, b, err := readAll(raw, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(img, want) || !b.Verified() || b.Manifest.Version != "1.2.3" || b.Manifest.ImageSize != int64(len(want)) {
		t.Fatalf("bundle read back wrong: verified=%v %+v", b.Verified(), b.Manifest)
	}
}

func TestBundleUntrustedKey(t *testing.T) {
	key, _ := testKey(t)
	_, other := testKey(t)
	raw, _ := testBundle(t, key)
	if _, err := OpenBundle(bytes.NewReader(raw), other); err == nil || !strings.Contains(err.Error(), "does not trust") {
		t.Fatalf("untrusted key accepted: %v", err)
	}
}

// Every changed byte of a bundle is rejected, and a changed manifest or
// signature is rejected before any of the image is read.
func TestBundleTamper(t *testing.T) {
	key, keys := testKey(t)
	raw, _ := testBundle(t, key)
	imgStart := bytes.Index(raw, []byte("cerOS-image-data"))
	for i := 0; i < len(raw); i += 7 {
		bad := bytes.Clone(raw)
		bad[i] ^= 0x01
		b, err := OpenBundle(bytes.NewReader(bad), keys)
		if err != nil {
			continue
		}
		// Tar padding and header fields that do not change the content may
		// still open; the content must then be unchanged.
		r, err := b.Image()
		if err != nil {
			continue
		}
		got, err := io.ReadAll(r)
		if err == nil && !bytes.Equal(got, raw[imgStart:imgStart+len(got)]) {
			t.Fatalf("byte %d changed: a different image was accepted", i)
		}
	}
	// The image changed: only the end of the read fails.
	bad := bytes.Clone(raw)
	bad[imgStart+100] ^= 0xff
	if _, _, err := readAll(bad, keys); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("changed image accepted: %v", err)
	}
	// The manifest changed: rejected by the signature.
	bad = bytes.Replace(raw, []byte(`"1.2.3"`), []byte(`"1.2.4"`), 1)
	if _, err := OpenBundle(bytes.NewReader(bad), keys); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("changed manifest accepted: %v", err)
	}
}

func TestBundleTruncated(t *testing.T) {
	key, keys := testKey(t)
	raw, _ := testBundle(t, key)
	for _, n := range []int{100, 1500, len(raw) / 2, len(raw) - 2000} {
		if _, _, err := readAll(raw[:n], keys); err == nil {
			t.Fatalf("bundle cut at %d accepted", n)
		}
	}
}

func TestLoadKeys(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadKeys(dir); err == nil {
		t.Fatal("no keys accepted")
	}
	_, pub, _ := GenerateKey("a")
	os.WriteFile(filepath.Join(dir, "a.pub"), []byte(pub), 0o644)
	keys, err := LoadKeys(dir)
	if err != nil || len(keys) != 1 || len(keys[0].ID) != 16 {
		t.Fatalf("keys %v %v", keys, err)
	}
	os.WriteFile(filepath.Join(dir, "b.pub"), []byte("ssh-ed25519 AAAA"), 0o644)
	if _, err := LoadKeys(dir); err == nil {
		t.Fatal("a damaged key file is accepted")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"1.2.10", "1.10", -1}, {"1.10", "1.9", 1}, {"1.2", "1.2", 0}, {"1.2", "1.2.1", -1}, {"1.2-rc1", "1.2-rc2", -1}} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s: %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
