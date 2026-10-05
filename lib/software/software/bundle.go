package software

import (
	"archive/tar"
	"bytes"
	"cmp"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
)

// A bundle is the software of one platform (docs/os-image.md §4.1): an
// uncompressed tar file with manifest.json, manifest.sig (Ed25519 over
// the manifest's bytes) and rootfs.img (the slot image), in this order, so
// the signature is checked before the image is read.

// BundleManifest describes a bundle.
type BundleManifest struct {
	Version    string `json:"version"`
	Built      string `json:"built"` // RFC 3339
	Arch       string `json:"arch"`  // amd64
	Platform   string `json:"platform"`
	Compatible string `json:"compatible"`
	// The image: size, SHA-256 (hex), dm-verity root hash and the offset
	// of the hash tree in the image.
	ImageSize   int64  `json:"image_size"`
	ImageSHA256 string `json:"image_sha256"`
	RootHash    string `json:"roothash"`
	HashOffset  int64  `json:"hash_offset"`
	// MinFrom: the oldest version this one can update from ("": any).
	MinFrom string `json:"min_from,omitempty"`
}

const (
	bundleManifest = "manifest.json"
	bundleSig      = "manifest.sig"
	bundleImage    = "rootfs.img"
	// maxManifest bounds the manifest and signature files.
	maxManifest = 64 << 10
	// MaxImage bounds a slot image (the slot partition is 2 GiB).
	MaxImage = 2 << 30
)

// ---- keys ----

const (
	pubPrefix  = "ceros-ed25519 "
	privPrefix = "ceros-ed25519-private "
)

// PublicKey is a key that signs bundles.
type PublicKey struct {
	ID   string
	Key  ed25519.PublicKey
	File string // where it was loaded from
}

// KeyID names a public key: the first 8 bytes of its SHA-256, in hex.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// ParsePublicKey reads "ceros-ed25519 <base64> [comment]".
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), pubPrefix)
	if !ok {
		return nil, errors.New("not a cerOS public key (ceros-ed25519 …)")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Fields(rest)[0])
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("damaged cerOS public key")
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey reads "ceros-ed25519-private <base64 seed>".
func ParsePrivateKey(text string) (ed25519.PrivateKey, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), privPrefix)
	if !ok {
		return nil, errors.New("not a cerOS signing key (ceros-ed25519-private …)")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Fields(rest)[0])
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("damaged cerOS signing key")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// GenerateKey returns a new signing key and its public key as text.
func GenerateKey(comment string) (priv, pub string, err error) {
	p, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	priv = privPrefix + base64.StdEncoding.EncodeToString(k.Seed()) + "\n"
	pub = PublicKeyText(p, comment)
	return priv, pub, nil
}

// PublicKeyText formats a public key file.
func PublicKeyText(p ed25519.PublicKey, comment string) string {
	s := pubPrefix + base64.StdEncoding.EncodeToString(p)
	if comment != "" {
		s += " " + comment
	}
	return s + "\n"
}

// LoadKeys reads the trusted keys: every *.pub file in dir.
func LoadKeys(dir string) ([]PublicKey, error) {
	files, err := hwio.Glob(filepath.Join(dir, "*.pub"))
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	var keys []PublicKey
	for _, f := range files {
		raw, err := hwio.ReadFile(f)
		if err != nil {
			return nil, err
		}
		k, err := ParsePublicKey(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		keys = append(keys, PublicKey{ID: KeyID(k), Key: k, File: f})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no trusted signing keys in %s", dir)
	}
	return keys, nil
}

// ---- writing ----

// WriteBundle writes a bundle: m (whose image size and SHA-256 are filled
// in from the image file) signed with key.
func WriteBundle(w io.Writer, m BundleManifest, imagePath string, key ed25519.PrivateKey) error {
	f, err := hwio.Open(imagePath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, hwio.Reader(f, SlotIODeadline()))
	if err != nil {
		return err
	}
	m.ImageSize, m.ImageSHA256 = n, hex.EncodeToString(h.Sum(nil))
	if err := m.check(); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	raw = append(raw, '\n')
	sig := KeyID(key.Public().(ed25519.PublicKey)) + " " + base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw)) + "\n"
	tw := tar.NewWriter(w)
	add := func(name string, size int64, r io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := io.Copy(tw, r)
		return err
	}
	if err := add(bundleManifest, int64(len(raw)), bytes.NewReader(raw)); err != nil {
		return err
	}
	if err := add(bundleSig, int64(len(sig)), strings.NewReader(sig)); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := add(bundleImage, n, f); err != nil {
		return err
	}
	return tw.Close()
}

func (m *BundleManifest) check() error {
	switch {
	case m.Version == "" || strings.ContainsAny(m.Version, "/\x00 \n"):
		return fmt.Errorf("bad version %q", m.Version)
	case m.Arch == "" || m.Compatible == "":
		return errors.New("the architecture and compatible string are missing")
	case m.ImageSize <= 0 || m.ImageSize > MaxImage:
		return fmt.Errorf("bad image size %d", m.ImageSize)
	case len(m.ImageSHA256) != 64:
		return errors.New("bad image SHA-256")
	case m.RootHash == "" || m.HashOffset <= 0 || m.HashOffset >= m.ImageSize || m.HashOffset%4096 != 0:
		return errors.New("bad verity root hash or hash offset")
	}
	if _, err := hex.DecodeString(m.RootHash); err != nil {
		return errors.New("bad verity root hash")
	}
	return nil
}

// ---- reading ----

// Bundle is an open bundle whose signature is verified; its image is read
// with Image.
type Bundle struct {
	Manifest BundleManifest
	KeyID    string // the key that signed it
	tr       *tar.Reader
	imageOK  bool
}

// OpenBundle reads the manifest and signature from r and verifies them with
// the trusted keys; nothing of the image is read yet.
func OpenBundle(r io.Reader, keys []PublicKey) (*Bundle, error) {
	tr := tar.NewReader(r)
	next := func(want string, max int64) ([]byte, error) {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("not a cerOS bundle: %w", err)
		}
		if h.Name != want || h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("not a cerOS bundle: %s where %s was expected", h.Name, want)
		}
		if h.Size > max {
			return nil, fmt.Errorf("damaged bundle: %s is too large", want)
		}
		return io.ReadAll(tr)
	}
	raw, err := next(bundleManifest, maxManifest)
	if err != nil {
		return nil, err
	}
	sig, err := next(bundleSig, maxManifest)
	if err != nil {
		return nil, err
	}
	id, b64, ok := strings.Cut(strings.TrimSpace(string(sig)), " ")
	s, err := base64.StdEncoding.DecodeString(b64)
	if !ok || err != nil || len(s) != ed25519.SignatureSize {
		return nil, errors.New("damaged bundle: unreadable signature")
	}
	i := slices.IndexFunc(keys, func(k PublicKey) bool { return k.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("the bundle is signed by key %s, which this system does not trust", id)
	}
	if !ed25519.Verify(keys[i].Key, raw, s) {
		return nil, fmt.Errorf("the bundle's signature (key %s) is wrong: it was changed or damaged", id)
	}
	b := &Bundle{KeyID: id, tr: tr}
	if err := json.Unmarshal(raw, &b.Manifest); err != nil {
		return nil, fmt.Errorf("damaged bundle: manifest: %w", err)
	}
	if err := b.Manifest.check(); err != nil {
		return nil, fmt.Errorf("damaged bundle: %w", err)
	}
	return b, nil
}

// Image returns the image, which is checked against the manifest's size
// and SHA-256 while it is read: the reader returns an error instead of EOF
// when the image does not match. It can be called once.
func (b *Bundle) Image() (io.Reader, error) {
	h, err := b.tr.Next()
	if err != nil {
		return nil, fmt.Errorf("damaged bundle: %w", err)
	}
	if h.Name != bundleImage || h.Size != b.Manifest.ImageSize {
		return nil, fmt.Errorf("damaged bundle: %s (%d bytes) where %s (%d bytes) was expected",
			h.Name, h.Size, bundleImage, b.Manifest.ImageSize)
	}
	return &checkedReader{r: b.tr, h: sha256.New(), want: b.Manifest.ImageSHA256, left: h.Size, ok: &b.imageOK}, nil
}

// Verified reports whether the whole image was read and matched.
func (b *Bundle) Verified() bool { return b.imageOK }

type checkedReader struct {
	r    io.Reader
	h    hash.Hash
	want string
	left int64
	ok   *bool
}

func (c *checkedReader) Read(p []byte) (int, error) {
	if c.left == 0 {
		if got := hex.EncodeToString(c.h.Sum(nil)); got != c.want {
			return 0, fmt.Errorf("damaged bundle: the image has SHA-256 %s, the manifest says %s", got, c.want)
		}
		*c.ok = true
		return 0, io.EOF
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.h.Write(p[:n])
	c.left -= int64(n)
	if errors.Is(err, io.EOF) {
		if c.left > 0 {
			return n, io.ErrUnexpectedEOF
		}
		err = nil
	}
	return n, err
}

// VerifyBundleFile checks a whole bundle file: signature and image.
func VerifyBundleFile(path string, keys []PublicKey) (*BundleManifest, error) {
	f, err := hwio.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := OpenBundle(hwio.Reader(f, SlotIODeadline()), keys)
	if err != nil {
		return nil, err
	}
	img, err := b.Image()
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(io.Discard, img); err != nil {
		return nil, err
	}
	return &b.Manifest, nil
}

// Compatible is what this program's platform accepts.
const Compatible = "ceros-x86_64"

// CompareVersions orders versions such as "1.2.10" and "1.10": fields
// that are numbers compare as numbers, others as text.
func CompareVersions(a, b string) int {
	split := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '-' })
	}
	fa, fb := split(a), split(b)
	for i := 0; i < len(fa) && i < len(fb); i++ {
		na, ea := strconv.Atoi(fa[i])
		nb, eb := strconv.Atoi(fb[i])
		if ea == nil && eb == nil {
			if c := cmp.Compare(na, nb); c != 0 {
				return c
			}
			continue
		}
		if c := strings.Compare(fa[i], fb[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(fa), len(fb))
}
