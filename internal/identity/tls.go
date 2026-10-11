package identity

import (
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"
)

// VerifyPeerCert verifies a peer's leaf certificate against the expected
// device ID: exactly one certificate must be presented, the device ID
// derived from its DER bytes must equal expected (constant-time compare),
// and the certificate must be inside its validity window.
func VerifyPeerCert(rawCerts [][]byte, expectedPeerDeviceID string) error {
	expected, err := ParseDeviceID(expectedPeerDeviceID)
	if err != nil {
		return fmt.Errorf("identity: verify peer: bad expected device id: %w", err)
	}
	if len(rawCerts) != 1 {
		return fmt.Errorf("identity: verify peer: expected exactly 1 certificate, got %d", len(rawCerts))
	}
	got := DeviceIDFromCertDER(rawCerts[0])
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return fmt.Errorf("identity: verify peer: certificate pin mismatch")
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("identity: verify peer: parse leaf: %w", err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return fmt.Errorf("identity: verify peer: certificate outside validity window %s..%s",
			leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	return nil
}

// ServerTLSConfig returns a TLS 1.3 server config requiring a client
// certificate, verified by pinning to expectedPeerDeviceID.
//
// If expectedPeerDeviceID is empty the returned config still rejects every
// peer (VerifyPeerCert fails on an empty expected ID): there is no
// "accept anyone" mode. Job 003's pairing flow must use its own handshake
// rather than passing an empty ID here.
func (id *Identity) ServerTLSConfig(expectedPeerDeviceID string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.TLSKeyPair()},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return VerifyPeerCert(rawCerts, expectedPeerDeviceID)
		},
	}
}

// ClientTLSConfig returns a TLS 1.3 client config presenting this identity
// and verifying the server by pinning to expectedPeerDeviceID.
//
// InsecureSkipVerify is set to true here, and that does NOT disable
// authentication: it disables Go's DNS/CA-chain verification, which is
// meaningless for self-signed certificates without SANs. Authentication is
// fully enforced in VerifyPeerCertificate, which pins the peer's
// certificate by SHA-256 (via the device ID). A config without that
// callback would be a security bug. An empty expectedPeerDeviceID makes
// the callback reject every peer (see ServerTLSConfig).
func (id *Identity) ClientTLSConfig(expectedPeerDeviceID string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{id.TLSKeyPair()},
		InsecureSkipVerify: true, // see doc comment: verification happens in VerifyPeerCertificate
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return VerifyPeerCert(rawCerts, expectedPeerDeviceID)
		},
	}
}
