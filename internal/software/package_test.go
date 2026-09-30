package software

import (
	"bytes"
	"strings"
	"testing"
)

func TestPackage(t *testing.T) {
	var b bytes.Buffer
	if err := Write(&b, "v1", "2026-10-01T00:00:00Z", map[string][]byte{"amd64": []byte("x86"), "arm64": []byte("arm")}); err != nil {
		t.Fatal(err)
	}
	raw := b.Bytes()
	p, err := Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Version != "v1" || strings.Join(p.Manifest.Arches(), ",") != "amd64,arm64" {
		t.Errorf("manifest: %+v", p.Manifest)
	}
	if prog, err := p.Program("arm64"); err != nil || string(prog) != "arm" {
		t.Errorf("arm64: %q %v", prog, err)
	}
	if _, err := p.Program("arm"); err == nil || !strings.Contains(err.Error(), "has no program for arm") {
		t.Errorf("missing arch: %v", err)
	}
	// A changed program fails the manifest check.
	var bad bytes.Buffer
	Write(&bad, "v1", "", map[string][]byte{"amd64": []byte("x86")})
	m := bad.Bytes()
	if _, err := Read(bytes.NewReader(m[:len(m)/2])); err == nil {
		t.Error("truncated package accepted")
	}
	if _, err := Read(strings.NewReader("not a package")); err == nil {
		t.Error("garbage accepted")
	}
}
