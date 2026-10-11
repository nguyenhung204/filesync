// Package identity manages a node's self-signed TLS identity: an ECDSA
// P-256 key plus certificate stored in the config directory. The device ID
// is derived from the certificate: SHA-256 over the DER bytes, base32
// encoded without padding (52 chars). Peers pin certificates by this ID.
package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File names inside the config directory, exported for later jobs.
const (
	CertFileName = "cert.pem"
	KeyFileName  = "key.pem"
)

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// DeviceIDFromCertDER derives the canonical device ID (52 chars) from a
// certificate's DER encoding.
func DeviceIDFromCertDER(der []byte) string {
	sum := sha256.Sum256(der)
	return base32NoPad.EncodeToString(sum[:])
}

// FingerprintFromCertDER renders the same SHA-256 as uppercase hex grouped
// in blocks of 4 characters separated by single spaces (16 groups), for
// human comparison during pairing.
func FingerprintFromCertDER(der []byte) string {
	sum := sha256.Sum256(der)
	hexStr := strings.ToUpper(hex.EncodeToString(sum[:]))
	return groupString(hexStr, 4, " ")
}

// DisplayDeviceID groups a device ID into blocks of 4 characters separated
// by '-' (13 blocks).
func DisplayDeviceID(id string) string {
	return groupString(id, 4, "-")
}

func groupString(s string, size int, sep string) string {
	var b strings.Builder
	for i, c := range s {
		if i > 0 && i%size == 0 {
			b.WriteString(sep)
		}
		b.WriteRune(c)
	}
	return b.String()
}

// ParseDeviceID accepts the raw form or the display form (with '-' or
// spaces, any case) and returns the canonical 52-char uppercase form. Input
// that does not decode to exactly 32 bytes is rejected.
func ParseDeviceID(s string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	cleaned = strings.ToUpper(cleaned)
	raw, err := base32NoPad.DecodeString(cleaned)
	if err != nil {
		return "", fmt.Errorf("identity: parse device id: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("identity: parse device id: decoded %d bytes, want 32", len(raw))
	}
	// Reject non-canonical encodings (unused low bits set in the last char).
	if re := base32NoPad.EncodeToString(raw); re != cleaned {
		return "", fmt.Errorf("identity: parse device id: non-canonical encoding")
	}
	return cleaned, nil
}

// Identity is a loaded node identity.
type Identity struct {
	DeviceID    string
	Fingerprint string
	Certificate *x509.Certificate
	CertDER     []byte

	key *ecdsa.PrivateKey
}

func newIdentity(cert *x509.Certificate, der []byte, key *ecdsa.PrivateKey) *Identity {
	return &Identity{
		DeviceID:    DeviceIDFromCertDER(der),
		Fingerprint: FingerprintFromCertDER(der),
		Certificate: cert,
		CertDER:     append([]byte(nil), der...),
		key:         key,
	}
}

// TLSKeyPair returns the identity as a tls.Certificate for tls.Config.
func (id *Identity) TLSKeyPair() tls.Certificate {
	return tls.Certificate{
		Certificate: [][]byte{id.CertDER},
		PrivateKey:  id.key,
		Leaf:        id.Certificate,
	}
}

// ensureDir mirrors the helper in internal/config on purpose: identity and
// config must stay independent so later jobs can use either package alone.
// On Windows Chmod has near-no-op semantics; there we rely on the profile
// directory's ACLs.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod dir %s: %w", dir, err)
	}
	return nil
}

func fsyncFile(f *os.File) {
	_ = f.Sync() // best-effort, as in config.Save
}

// LoadOrCreate loads the identity stored in dir, or creates and stores a
// fresh one if neither file exists yet. If exactly one file exists, or the
// pair is corrupt/mismatched/expired, it returns an error and never
// regenerates: a new identity means a new device ID and would break every
// existing pairing.
func LoadOrCreate(dir string) (id *Identity, created bool, err error) {
	certPath := filepath.Join(dir, CertFileName)
	keyPath := filepath.Join(dir, KeyFileName)
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	certExists := certErr == nil
	keyExists := keyErr == nil
	switch {
	case certExists && keyExists:
		id, err := load(certPath, keyPath)
		if err != nil {
			return nil, false, err
		}
		return id, false, nil
	case !certExists && !keyExists:
		if certErr != nil && !os.IsNotExist(certErr) {
			return nil, false, fmt.Errorf("identity: stat %s: %w", certPath, certErr)
		}
		if keyErr != nil && !os.IsNotExist(keyErr) {
			return nil, false, fmt.Errorf("identity: stat %s: %w", keyPath, keyErr)
		}
		id, err := create(dir, certPath, keyPath)
		if err != nil {
			// Lost a creation race: the winner's files now exist, so fall
			// back to loading exactly once instead of overwriting.
			if errors.Is(err, fs.ErrExist) {
				loaded, lerr := load(certPath, keyPath)
				if lerr == nil {
					return loaded, false, nil
				}
			}
			return nil, false, err
		}
		return id, true, nil
	default:
		return nil, false, fmt.Errorf(
			"identity: corrupt identity in %s: exactly one of %s/%s exists; refusing to regenerate",
			dir, CertFileName, KeyFileName)
	}
}

func load(certPath, keyPath string) (*Identity, error) {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("identity: read key: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("identity: corrupt key file %s: no EC PRIVATE KEY block", keyPath)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: corrupt key file %s: %w", keyPath, err)
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("identity: read cert: %w", err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("identity: corrupt cert file %s: no CERTIFICATE block", certPath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: corrupt cert file %s: %w", certPath, err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		return nil, fmt.Errorf("identity: mismatch: cert public key does not match private key in %s", keyPath)
	}
	if time.Now().After(cert.NotAfter) {
		return nil, fmt.Errorf("identity: certificate expired at %s; refusing to regenerate", cert.NotAfter.Format(time.RFC3339))
	}
	// Best-effort: tighten a loosened key file back to 0600. A chmod failure
	// does not fail the load.
	if st, err := os.Stat(keyPath); err == nil && st.Mode().Perm()&^0o600 != 0 {
		_ = os.Chmod(keyPath, 0o600)
	}
	return newIdentity(cert, certBlock.Bytes, key), nil
}

func create(dir, certPath, keyPath string) (*Identity, error) {
	if err := ensureDir(dir); err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "filesync-device"},
		NotBefore:             now.Add(-5 * time.Minute), // skew tolerance for validity checks
		NotAfter:              now.AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // self-signed; trust comes from pinning, not the chain
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("identity: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("identity: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writeFileExclusive(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("identity: write key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFileExclusive(certPath, certPEM, 0o644); err != nil {
		// Remove the key we just wrote so a later LoadOrCreate does not
		// see a half-created pair as corruption.
		_ = os.Remove(keyPath)
		return nil, fmt.Errorf("identity: write cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("identity: parse created cert: %w", err)
	}
	return newIdentity(cert, der, key), nil
}

func randomSerial() (*big.Int, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("identity: generate serial: %w", err)
	}
	b[0] &= 0x7F // keep it positive
	if b == [16]byte{} {
		b[15] = 1
	}
	return new(big.Int).SetBytes(b[:]), nil
}

func writeFileExclusive(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	fsyncFile(f)
	return f.Close()
}
