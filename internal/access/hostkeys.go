package access

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io/fs"
	"math/big"
	"path/filepath"

	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// Host keys of the CLI SSH server (reference 5.1): derived from a secret
// every stack member has (the stack key), so every member presents the
// same keys and the management address keeps its fingerprint when
// mastership moves.

// HostKeyFiles are the private key files WriteHostKeys writes in dir.
func HostKeyFiles(dir string) []string {
	return []string{filepath.Join(dir, "ssh_host_ed25519_key"), filepath.Join(dir, "ssh_host_ecdsa_key")}
}

// WriteHostKeys derives the host keys from secret and writes them (and the
// .pub files) to dir when they differ. It reports whether a file changed.
func WriteHostKeys(dir string, secret []byte) (bool, error) {
	edSeed, err := hkdf.Key(sha256.New, secret, nil, "ceros ssh host key ed25519", ed25519.SeedSize)
	if err != nil {
		return false, err
	}
	ed := ed25519.NewKeyFromSeed(edSeed)
	ecKey, err := deriveP256(secret)
	if err != nil {
		return false, err
	}
	edPriv, edPub := openSSHEd25519(ed)
	der, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		return false, err
	}
	ecPriv := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	files := []struct {
		name string
		data []byte
		mode fs.FileMode
	}{
		{"ssh_host_ed25519_key", edPriv, 0o600},
		{"ssh_host_ed25519_key.pub", edPub, 0o644},
		{"ssh_host_ecdsa_key", ecPriv, 0o600},
		{"ssh_host_ecdsa_key.pub", ecdsaPub(&ecKey.PublicKey), 0o644},
	}
	if err := hwio.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	changed := false
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if have, err := hwio.ReadFile(path); err == nil && bytes.Equal(have, f.data) {
			continue
		}
		if err := hwio.WriteFile(path+".tmp", f.data, f.mode); err != nil {
			return changed, err
		}
		if err := hwio.Rename(path+".tmp", path); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// deriveP256 makes a P-256 key from secret: a scalar in [1, n-1].
func deriveP256(secret []byte) (*ecdsa.PrivateKey, error) {
	raw, err := hkdf.Key(sha256.New, secret, nil, "ceros ssh host key ecdsa-p256", 40)
	if err != nil {
		return nil, err
	}
	n := elliptic.P256().Params().N
	d := new(big.Int).SetBytes(raw)
	d.Mod(d, new(big.Int).Sub(n, big.NewInt(1)))
	d.Add(d, big.NewInt(1))
	b := d.FillBytes(make([]byte, 32))
	k, err := ecdh.P256().NewPrivateKey(b)
	if err != nil {
		return nil, err
	}
	pub := k.PublicKey().Bytes() // 0x04 || X || Y
	key := &ecdsa.PrivateKey{D: d}
	key.Curve = elliptic.P256()
	key.X = new(big.Int).SetBytes(pub[1:33])
	key.Y = new(big.Int).SetBytes(pub[33:])
	return key, nil
}

func sshString(b *bytes.Buffer, s []byte) {
	binary.Write(b, binary.BigEndian, uint32(len(s)))
	b.Write(s)
}

const hostKeyComment = "ceros-stack"

// openSSHEd25519 encodes an Ed25519 key in OpenSSH's format
// (openssh-key-v1, unencrypted) and its public key line.
func openSSHEd25519(k ed25519.PrivateKey) (priv, pub []byte) {
	pk := k.Public().(ed25519.PublicKey)
	var blob bytes.Buffer
	sshString(&blob, []byte("ssh-ed25519"))
	sshString(&blob, pk)
	var sec bytes.Buffer
	check := binary.BigEndian.Uint32(pk[:4]) // any value, the same twice
	binary.Write(&sec, binary.BigEndian, check)
	binary.Write(&sec, binary.BigEndian, check)
	sshString(&sec, []byte("ssh-ed25519"))
	sshString(&sec, pk)
	sshString(&sec, k) // seed || public key
	sshString(&sec, []byte(hostKeyComment))
	for i := byte(1); sec.Len()%8 != 0; i++ {
		sec.WriteByte(i)
	}
	var b bytes.Buffer
	b.WriteString("openssh-key-v1\x00")
	sshString(&b, []byte("none"))
	sshString(&b, []byte("none"))
	sshString(&b, nil)
	binary.Write(&b, binary.BigEndian, uint32(1))
	sshString(&b, blob.Bytes())
	sshString(&b, sec.Bytes())
	priv = pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: b.Bytes()})
	pub = fmt.Appendf(nil, "ssh-ed25519 %s %s\n", base64.StdEncoding.EncodeToString(blob.Bytes()), hostKeyComment)
	return priv, pub
}

// ecdsaPub is the public key line of a P-256 key.
func ecdsaPub(k *ecdsa.PublicKey) []byte {
	var blob bytes.Buffer
	sshString(&blob, []byte("ecdsa-sha2-nistp256"))
	sshString(&blob, []byte("nistp256"))
	point := make([]byte, 65)
	point[0] = 4
	k.X.FillBytes(point[1:33])
	k.Y.FillBytes(point[33:])
	sshString(&blob, point)
	return fmt.Appendf(nil, "ecdsa-sha2-nistp256 %s %s\n", base64.StdEncoding.EncodeToString(blob.Bytes()), hostKeyComment)
}
