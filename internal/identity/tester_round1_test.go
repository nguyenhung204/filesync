package identity

import (
	"crypto/tls"
	"io"
	"testing"
	"time"
)

// Tester round 1: an empty expected device ID must never authenticate
// anyone (spec: no "accept anyone" mode in Server/ClientTLSConfig).
// The builder's tests cover empty-expected at VerifyPeerCert level;
// this covers it through a real TLS handshake both ways.
func TestTesterTLSHandshake_EmptyExpectedPinFails(t *testing.T) {
	a, _ := mustCreate(t)
	b, _ := mustCreate(t)

	// Client side: A dials with expected "" — the client must reject
	// B's (otherwise valid) certificate during the handshake.
	addr, srvErr := runEchoServer(t, b.ServerTLSConfig(a.DeviceID))
	conn, err := tls.Dial("tcp", addr, a.ClientTLSConfig(""))
	if err == nil {
		conn.Close()
		t.Fatal("client with empty expected pin: handshake unexpectedly succeeded")
	}
	<-srvErr // server errors too once the client aborts; already asserted

	// Server side: B serves with expected "" — B must reject A's
	// (otherwise valid) client certificate.
	addr2, srvErr2 := runEchoServer(t, b.ServerTLSConfig(""))
	conn2, err2 := tls.Dial("tcp", addr2, a.ClientTLSConfig(b.DeviceID))
	if err2 == nil {
		// TLS 1.3 clients can finish locally before the server rejects
		// the certificate; the failure must then surface on first I/O.
		_, werr := conn2.Write([]byte("ping"))
		buf := make([]byte, 4)
		_, rerr := io.ReadFull(conn2, buf)
		conn2.Close()
		if werr == nil && rerr == nil {
			t.Fatal("server with empty expected pin: connection unexpectedly usable")
		}
	}
	select {
	case serr := <-srvErr2:
		if serr == nil {
			t.Fatal("server with empty expected pin: handshake unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not report a handshake result")
	}
}

// Tester round 1: VerifyPeerCert must reject a cert whose validity
// window check fails via garbage DER and a fingerprint-form expected
// ID (display form with dashes), not only the raw canonical form.
func TestTesterVerifyPeerCert_DisplayFormAndGarbage(t *testing.T) {
	a, _ := mustCreate(t)
	if err := VerifyPeerCert([][]byte{a.CertDER}, DisplayDeviceID(a.DeviceID)); err != nil {
		t.Fatalf("display-form expected pin rejected: %v", err)
	}
	if err := VerifyPeerCert([][]byte{[]byte("not a der cert")}, a.DeviceID); err == nil {
		t.Fatal("garbage DER accepted")
	}
	if err := VerifyPeerCert([][]byte{{}}, a.DeviceID); err == nil {
		t.Fatal("empty DER accepted")
	}
}
