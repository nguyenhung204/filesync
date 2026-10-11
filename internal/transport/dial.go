package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"filesync/internal/identity"
	"filesync/internal/logx"
	"filesync/internal/proto"
)

// Candidates returns the addresses to try for peerID: static addresses
// from the store first (in stored order), then mDNS cache hits,
// deduplicated.
func (m *Manager) Candidates(peerID string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	if peer, found, err := m.trust.GetPeer(peerID); err == nil && found {
		for _, a := range peer.Addresses {
			add(a)
		}
	}
	if m.disc != nil {
		for _, c := range m.disc.Candidates() {
			if c.DeviceID == peerID {
				add(c.Addr)
			}
		}
	}
	return out
}

// Dial connects to a trusted peer, trying every candidate address. The
// TLS handshake pins the peer's certificate to peerID via
// identity.ClientTLSConfig, and the Hello handshake (Flow E) completes
// before Dial returns.
func (m *Manager) Dial(ctx context.Context, peerID string) (*Conn, error) {
	cands := m.Candidates(peerID)
	if len(cands) == 0 {
		return nil, fmt.Errorf("transport: dial %s: no candidate addresses", peerID)
	}
	var lastErr error
	for _, addr := range cands {
		c, err := m.dialOne(ctx, addr, m.id.ClientTLSConfig(peerID), peerID, true)
		if err == nil {
			return c, nil
		}
		lastErr = err
		m.log.Debug(ctx, "transport: dial attempt failed",
			logx.String("peer", peerID), logx.String("addr", addr), logx.Err(err))
	}
	return nil, fmt.Errorf("transport: dial %s: tried %d addresses: %w", peerID, len(cands), lastErr)
}

// DialWithBackoff retries Dial with exponential backoff + jitter
// (Flow H) until success or ctx cancellation.
func (m *Manager) DialWithBackoff(ctx context.Context, peerID string) (*Conn, error) {
	for n := 0; ; n++ {
		c, err := m.Dial(ctx, peerID)
		if err == nil {
			return c, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		d := m.backoffDelay(n)
		m.log.Debug(ctx, "transport: backoff before redial",
			logx.String("peer", peerID), logx.Int("attempt", n), logx.Int64("delay_ms", d.Milliseconds()), logx.Err(err))
		if err := m.opts.Sleep(ctx, d); err != nil {
			return nil, err
		}
	}
}

// backoffDelay computes min(Max, Base * 2^n) scaled by jitter in
// [0.8, 1.2], capped at Max again after jitter so the sequence never
// exceeds BackoffMax (spec Flow H formula + checklist bound).
func (m *Manager) backoffDelay(n int) time.Duration {
	d := m.opts.BackoffBase
	for i := 0; i < n && d < m.opts.BackoffMax; i++ {
		d *= 2
	}
	if d > m.opts.BackoffMax {
		d = m.opts.BackoffMax
	}
	factor := 1 + (m.opts.JitterRand()*2-1)*0.2
	d = time.Duration(float64(d) * factor)
	if d > m.opts.BackoffMax {
		d = m.opts.BackoffMax
	}
	return d
}

// DialPairing connects to addr for a pairing session and returns the
// connection plus the server device ID and fingerprint derived from
// the certificate it presented.
//
// With expectedID != "" the connection pins that ID via
// identity.ClientTLSConfig. With expectedID == "" the peer is not
// known yet by definition: InsecureSkipVerify is set, but that does
// NOT disable authentication — exactly as in identity.ClientTLSConfig
// it only turns off DNS/CA verification, which is meaningless for
// self-signed certificates. The VerifyPeerCertificate callback still
// enforces exactly one certificate, parseable, in its validity
// window, and captures its device ID + fingerprint. The peer is then
// authenticated by the pairing HMAC proof plus the user's fingerprint
// confirmation (pairing package); until both succeed the connection
// stays untrusted and carries only Pair* messages.
func (m *Manager) DialPairing(ctx context.Context, addr string, expectedID string) (*Conn, string, string, error) {
	if expectedID != "" {
		c, err := m.dialOne(ctx, addr, m.id.ClientTLSConfig(expectedID), expectedID, false)
		if err != nil {
			return nil, "", "", err
		}
		return c, c.PeerID(), c.Fingerprint(), nil
	}
	var captured atomic.Value // []byte (cert DER)
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{m.id.TLSKeyPair()},
		InsecureSkipVerify: true, // see doc comment: verification happens in VerifyPeerCertificate + pairing proof
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if err := verifyCertShape(rawCerts); err != nil {
				return err
			}
			captured.Store(append([]byte(nil), rawCerts[0]...))
			return nil
		},
	}
	c, err := m.dialOne(ctx, addr, cfg, "", false)
	if err != nil {
		return nil, "", "", err
	}
	return c, c.PeerID(), c.Fingerprint(), nil
}

// dialOne performs one TCP+TLS connection attempt. When hello is true
// the trusted Hello handshake runs before returning.
func (m *Manager) dialOne(ctx context.Context, addr string, tlsCfg *tls.Config, expectedID string, hello bool) (*Conn, error) {
	corrID := logx.NewCorrelationID()
	cctx, cancel := context.WithCancel(logx.WithCorrelationID(ctx, corrID))
	d := net.Dialer{}
	raw, err := d.DialContext(cctx, "tcp", addr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("transport: dial %s: %w", addr, err)
	}
	tc := tls.Client(raw, tlsCfg)
	_ = tc.SetDeadline(time.Now().Add(m.opts.HandshakeTimeout))
	if err := tc.HandshakeContext(cctx); err != nil {
		raw.Close()
		cancel()
		return nil, fmt.Errorf("transport: handshake %s: %w", addr, err)
	}
	_ = tc.SetDeadline(time.Time{})
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) != 1 {
		tc.Close()
		cancel()
		return nil, fmt.Errorf("transport: handshake %s: %d peer certificates", addr, len(certs))
	}
	peerID := identity.DeviceIDFromCertDER(certs[0].Raw)
	fp := identity.FingerprintFromCertDER(certs[0].Raw)
	if expectedID != "" && peerID != expectedID {
		tc.Close()
		cancel()
		return nil, fmt.Errorf("transport: handshake %s: peer is %s, want %s", addr, peerID, expectedID)
	}
	trusted := hello && m.classify(peerID, fp)
	if hello && !trusted {
		tc.Close()
		cancel()
		return nil, fmt.Errorf("transport: dial %s: peer %s is not trusted (unknown or revoked)", addr, peerID)
	}
	c := newConn(tc, peerID, fp, trusted, cctx, cancel, m)
	if !m.register(c) {
		c.Close()
		return nil, fmt.Errorf("transport: dial %s: connection limit reached", addr)
	}
	if hello {
		if err := m.exchangeHello(cctx, c); err != nil {
			m.unregister(c)
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

// helloMessage builds this node's Hello.
func (m *Manager) helloMessage() proto.Message {
	return proto.Message{Header: proto.Header{
		Type:     proto.TypeHello,
		Protocol: proto.ProtocolVersion,
		DeviceID: m.id.DeviceID,
		Name:     m.cfg.DeviceName,
	}}
}

// exchangeHello is the active side of Flow E step 3.
func (m *Manager) exchangeHello(ctx context.Context, c *Conn) error {
	if err := c.WriteMessage(ctx, m.helloMessage()); err != nil {
		return fmt.Errorf("transport: send hello: %w", err)
	}
	resp, err := c.ReadMessage(ctx)
	if err != nil {
		return fmt.Errorf("transport: read hello response: %w", err)
	}
	return m.checkHello(ctx, c, resp)
}

// acceptHello is the receiving side of Flow E step 3, run by Serve
// before the sync handler is invoked.
func (m *Manager) acceptHello(ctx context.Context, c *Conn) error {
	msg, err := c.ReadMessage(ctx)
	if err != nil {
		return fmt.Errorf("transport: read hello: %w", err)
	}
	if err := m.checkHello(ctx, c, msg); err != nil {
		return err
	}
	if err := c.WriteMessage(ctx, m.helloMessage()); err != nil {
		return fmt.Errorf("transport: send hello: %w", err)
	}
	return nil
}

// checkHello validates a Hello received from the peer of c. Version
// mismatch and identity mismatch are answered with the Flow E/F error
// message before returning an error (the caller then closes).
func (m *Manager) checkHello(ctx context.Context, c *Conn, msg proto.Message) error {
	if msg.Header.Type == proto.TypeError {
		return fmt.Errorf("transport: peer error %s: %s", msg.Header.Code, msg.Header.Message)
	}
	if msg.Header.Type != proto.TypeHello {
		c.sendError(ctx, proto.CodeBadMessage, "first message must be hello")
		return fmt.Errorf("transport: first message is %s, want hello", msg.Header.Type)
	}
	if msg.Header.Protocol != proto.ProtocolVersion {
		c.sendError(ctx, proto.CodeProtocolVersion,
			fmt.Sprintf("unsupported protocol version %d, want %d", msg.Header.Protocol, proto.ProtocolVersion))
		return fmt.Errorf("transport: unsupported protocol version %d", msg.Header.Protocol)
	}
	if msg.Header.DeviceID != c.PeerID() {
		c.sendError(ctx, proto.CodeBadMessage, "hello device_id does not match certificate")
		return fmt.Errorf("transport: hello device_id %s does not match certificate %s", msg.Header.DeviceID, c.PeerID())
	}
	return nil
}
