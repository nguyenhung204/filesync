package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base32"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func mustCreate(t *testing.T) (*Identity, string) {
	t.Helper()
	dir := t.TempDir()
	id, created, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first LoadOrCreate: created = false")
	}
	return id, dir
}

func TestLoadOrCreate_CreatesThenLoadsSameIdentity(t *testing.T) {
	id1, dir := mustCreate(t)
	id2, created, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second LoadOrCreate: created = true")
	}
	if id2.DeviceID != id1.DeviceID || id2.Fingerprint != id1.Fingerprint {
		t.Fatalf("identity changed across loads: %s vs %s", id1.DeviceID, id2.DeviceID)
	}
}

func TestLoadOrCreate_KeyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions not applicable on Windows")
	}
	_, dir := mustCreate(t)
	st, err := os.Stat(filepath.Join(dir, KeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key.pem perm = %o, want 600", st.Mode().Perm())
	}
}

func TestLoadOrCreate_MissingOneFileFails(t *testing.T) {
	for _, victim := range []string{KeyFileName, CertFileName} {
		t.Run(victim, func(t *testing.T) {
			_, dir := mustCreate(t)
			other := CertFileName
			if victim == CertFileName {
				other = KeyFileName
			}
			otherPath := filepath.Join(dir, other)
			before, err := os.ReadFile(otherPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, victim)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadOrCreate(dir); err == nil {
				t.Fatal("missing one file: expected error")
			}
			after, err := os.ReadFile(otherPath)
			if err != nil {
				t.Fatalf("surviving file was removed: %v", err)
			}
			if string(after) != string(before) {
				t.Fatal("surviving file was modified")
			}
			// No fresh pair may have been generated.
			if _, err := os.Stat(filepath.Join(dir, victim)); !os.IsNotExist(err) {
				t.Fatal("a new file was generated despite missing pair half")
			}
		})
	}
}

func TestLoadOrCreate_MismatchedPairFails(t *testing.T) {
	_, dirA := mustCreate(t)
	_, dirB := mustCreate(t)
	foreignKey, err := os.ReadFile(filepath.Join(dirB, KeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, KeyFileName), foreignKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(dirA); err == nil {
		t.Fatal("mismatched key/cert pair: expected error")
	}
}

func TestDeviceIDAndFingerprintDerivation(t *testing.T) {
	id, _ := mustCreate(t)
	der := id.CertDER
	sum := sha256.Sum256(der)

	gotID := DeviceIDFromCertDER(der)
	if gotID != id.DeviceID {
		t.Fatalf("DeviceIDFromCertDER = %s, identity has %s", gotID, id.DeviceID)
	}
	if len(gotID) != 52 {
		t.Fatalf("device id length = %d, want 52", len(gotID))
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(gotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 32 || string(raw) != string(sum[:]) {
		t.Fatal("device id does not decode back to SHA-256 of DER")
	}

	disp := DisplayDeviceID(gotID)
	groups := strings.Split(disp, "-")
	if len(groups) != 13 {
		t.Fatalf("display id has %d groups, want 13", len(groups))
	}
	for _, g := range groups {
		if len(g) != 4 {
			t.Fatalf("display group %q not 4 chars", g)
		}
	}

	fp := FingerprintFromCertDER(der)
	fgroups := strings.Split(fp, " ")
	if len(fgroups) != 16 {
		t.Fatalf("fingerprint has %d groups, want 16", len(fgroups))
	}
	if strings.ReplaceAll(fp, " ", "") != strings.ToUpper(hex.EncodeToString(sum[:])) {
		t.Fatal("fingerprint does not match SHA-256 hex")
	}

	for _, form := range []string{gotID, disp, strings.ToLower(gotID), strings.ReplaceAll(fp, " ", "")[:0] + gotID} {
		parsed, err := ParseDeviceID(form)
		if err != nil {
			t.Fatalf("ParseDeviceID(%q): %v", form, err)
		}
		if parsed != gotID {
			t.Fatalf("ParseDeviceID(%q) = %q, want %q", form, parsed, gotID)
		}
	}
	if _, err := ParseDeviceID("not-a-device-id!!!"); err == nil {
		t.Fatal("garbage device id accepted")
	}
	if _, err := ParseDeviceID("AAAA"); err == nil {
		t.Fatal("short device id accepted")
	}
}

func TestVerifyPeerCert_CorrectAndWrongPin(t *testing.T) {
	a, _ := mustCreate(t)
	b, _ := mustCreate(t)
	if err := VerifyPeerCert([][]byte{a.CertDER}, a.DeviceID); err != nil {
		t.Fatalf("correct pin: %v", err)
	}
	if err := VerifyPeerCert([][]byte{a.CertDER}, DisplayDeviceID(a.DeviceID)); err != nil {
		t.Fatalf("correct pin in display form: %v", err)
	}
	if err := VerifyPeerCert([][]byte{a.CertDER}, b.DeviceID); err == nil {
		t.Fatal("wrong pin accepted")
	}
	if err := VerifyPeerCert(nil, a.DeviceID); err == nil {
		t.Fatal("empty rawCerts accepted")
	}
	if err := VerifyPeerCert([][]byte{a.CertDER, b.CertDER}, a.DeviceID); err == nil {
		t.Fatal("two certs accepted")
	}
	if err := VerifyPeerCert([][]byte{a.CertDER}, ""); err == nil {
		t.Fatal("empty expected id accepted")
	}
}

// runEchoServer accepts one TLS connection with cfg, handshakes, echoes one
// message, and reports the handshake error (if any) on errCh.
func runEchoServer(t *testing.T, cfg *tls.Config) (addr string, errCh chan error) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	errCh = make(chan error, 1)
	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		tc := conn.(*tls.Conn)
		if err := tc.Handshake(); err != nil {
			errCh <- err
			return
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(tc, buf); err != nil {
			errCh <- err
			return
		}
		if _, err := tc.Write(buf); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	return ln.Addr().String(), errCh
}

func TestTLSHandshake_CorrectPinSucceeds(t *testing.T) {
	a, _ := mustCreate(t)
	b, _ := mustCreate(t)
	addr, srvErr := runEchoServer(t, b.ServerTLSConfig(a.DeviceID))

	conn, err := tls.Dial("tcp", addr, a.ClientTLSConfig(b.DeviceID))
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q, want ping", buf)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
}

func TestTLSHandshake_WrongPinFails(t *testing.T) {
	a, _ := mustCreate(t)
	b, _ := mustCreate(t)
	c, _ := mustCreate(t)

	// Client expects C but reaches B: the client handshake must fail.
	addr, srvErr := runEchoServer(t, b.ServerTLSConfig(a.DeviceID))
	conn, err := tls.Dial("tcp", addr, a.ClientTLSConfig(c.DeviceID))
	if err == nil {
		conn.Close()
		t.Fatal("client with wrong pin: handshake unexpectedly succeeded")
	}
	<-srvErr // server side will error too; outcome already asserted

	// Server expects C but client is A: the server handshake must fail.
	addr2, srvErr2 := runEchoServer(t, b.ServerTLSConfig(c.DeviceID))
	conn2, err2 := tls.Dial("tcp", addr2, a.ClientTLSConfig(b.DeviceID))
	if err2 == nil {
		// TLS 1.3: the client can finish locally before the server rejects
		// its certificate; then the failure surfaces on first I/O instead.
		_, werr := conn2.Write([]byte("ping"))
		buf := make([]byte, 4)
		_, rerr := io.ReadFull(conn2, buf)
		conn2.Close()
		if werr == nil && rerr == nil {
			t.Fatal("server with wrong pin: connection unexpectedly usable")
		}
	}
	select {
	case serr := <-srvErr2:
		if serr == nil {
			t.Fatal("server with wrong pin: handshake unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not report a handshake result")
	}
}

func TestIdentity_CertProperties(t *testing.T) {
	id, _ := mustCreate(t)
	cert := id.Certificate
	if !cert.IsCA {
		t.Fatal("cert IsCA = false")
	}
	if len(cert.DNSNames) != 0 || len(cert.IPAddresses) != 0 {
		t.Fatal("cert SANs must be empty")
	}
	hasServer, hasClient := false, false
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageServerAuth {
			hasServer = true
		}
		if u == x509.ExtKeyUsageClientAuth {
			hasClient = true
		}
	}
	if !hasServer || !hasClient {
		t.Fatal("ExtKeyUsage must contain ServerAuth and ClientAuth")
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatal("public key is not ECDSA")
	}
	if pub.Curve != elliptic.P256() {
		t.Fatal("public key is not P-256")
	}
	validity := cert.NotAfter.Sub(cert.NotBefore)
	twentyYears := 20 * 365 * 24 * time.Hour
	twentyOneYears := 21 * 366 * 24 * time.Hour
	if validity < twentyYears || validity > twentyOneYears {
		t.Fatalf("validity window = %v, want approx 20 years", validity)
	}
}
