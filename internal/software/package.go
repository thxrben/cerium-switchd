// Package software handles cerOS software packages (reference 3.6): the
// package format, fetching, and installing on one member with an
// automatic return to the previous version when the new one does not
// start.
//
// A package is a gzip-compressed tar file with manifest.json and one
// switchd program per architecture, named switchd-linux-<arch>.
package software

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
)

// Manifest describes a package.
type Manifest struct {
	Version string `json:"version"`
	Built   string `json:"built"` // RFC 3339
	// Files maps each file name to its SHA-256 (hex).
	Files map[string]string `json:"files"`
}

// Arches lists the architectures in the package.
func (m *Manifest) Arches() []string {
	var out []string
	for f := range m.Files {
		if a, ok := strings.CutPrefix(f, "switchd-linux-"); ok {
			out = append(out, a)
		}
	}
	slices.Sort(out)
	return out
}

// Arch is the architecture name of this program (the package's naming).
func Arch() string { return runtime.GOARCH }

// maxFile bounds a file inside a package.
const maxFile = 256 << 20

// Package is a verified package in memory.
type Package struct {
	Manifest Manifest
	files    map[string][]byte
}

// Program returns the switchd program for arch.
func (p *Package) Program(arch string) ([]byte, error) {
	b, ok := p.files["switchd-linux-"+arch]
	if !ok {
		return nil, fmt.Errorf("the package %s has no program for %s (it has %s)", p.Manifest.Version, arch, strings.Join(p.Manifest.Arches(), ", "))
	}
	return b, nil
}

// Read reads and verifies a package: every file must be listed in the
// manifest with its SHA-256, and every listed file must be there.
func Read(r io.Reader) (*Package, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("not a software package: %w", err)
	}
	tr := tar.NewReader(gz)
	p := &Package{files: map[string][]byte{}}
	var manifest []byte
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("damaged package: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxFile {
			return nil, fmt.Errorf("damaged package: %s is too large", h.Name)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxFile))
		if err != nil {
			return nil, fmt.Errorf("damaged package: %w", err)
		}
		if h.Name == "manifest.json" {
			manifest = b
			continue
		}
		p.files[h.Name] = b
	}
	if manifest == nil {
		return nil, errors.New("not a software package: manifest.json is missing")
	}
	if err := json.Unmarshal(manifest, &p.Manifest); err != nil || p.Manifest.Version == "" {
		return nil, fmt.Errorf("damaged package: manifest.json: %v", err)
	}
	for name, want := range p.Manifest.Files {
		b, ok := p.files[name]
		if !ok {
			return nil, fmt.Errorf("damaged package: %s is missing", name)
		}
		if got := Sum(b); got != want {
			return nil, fmt.Errorf("damaged package: %s has SHA-256 %s, the manifest says %s", name, got, want)
		}
	}
	for name := range p.files {
		if _, ok := p.Manifest.Files[name]; !ok {
			return nil, fmt.Errorf("damaged package: %s is not in the manifest", name)
		}
	}
	if len(p.Manifest.Arches()) == 0 {
		return nil, errors.New("damaged package: no switchd program")
	}
	return p, nil
}

// ReadFile reads and verifies a package file; want is its SHA-256 ("":
// not checked).
func ReadFile(path, want string) (*Package, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if want != "" && !strings.EqualFold(Sum(raw), want) {
		return nil, fmt.Errorf("the package's SHA-256 is %s, expected %s", Sum(raw), strings.ToLower(want))
	}
	return Read(bytes.NewReader(raw))
}

// Sum returns the SHA-256 of b in hex.
func Sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Write builds a package from the programs (arch -> file contents).
func Write(w io.Writer, version, built string, programs map[string][]byte) error {
	m := Manifest{Version: version, Built: built, Files: map[string]string{}}
	arches := make([]string, 0, len(programs))
	for a, b := range programs {
		m.Files["switchd-linux-"+a] = Sum(b)
		arches = append(arches, a)
	}
	slices.Sort(arches)
	raw, _ := json.MarshalIndent(m, "", "  ")
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	add := func(name string, b []byte, mode int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	if err := add("manifest.json", append(raw, '\n'), 0o644); err != nil {
		return err
	}
	for _, a := range arches {
		if err := add("switchd-linux-"+a, programs[a], 0o755); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
