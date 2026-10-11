// Package transport moves proto messages between FileSync nodes over
// TCP + TLS 1.3 mutual authentication. A connection is trusted only if
// the device ID derived from the peer's certificate has a non-revoked
// store record with a matching fingerprint (spec D2); untrusted
// connections may reach the pairing handler only while the pairing gate
// is open, and are closed before any message is read otherwise.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"filesync/internal/chunk"
	"filesync/internal/config"
	"filesync/internal/identity"
	"filesync/internal/logx"
	"filesync/internal/store"
)

// TrustSource is the slice of the store transport needs.
type TrustSource interface {
	GetPeer(deviceID string) (store.Peer, bool, error)
}

// Handler serves one established connection in its own goroutine.
type Handler func(ctx context.Context, c *Conn)

// PairingGate reports whether the pairing window is currently open.
// pairing.Manager implements it; transport never imports pairing.
type PairingGate interface{ PairingOpen() bool }

// Options tunes a Manager. Most zero values get the documented
// defaults via applyDefaults, with two exceptions: IdleTimeout, where
// 0 means OFF per spec (DefaultOptions supplies the 120s default),
// and MDNSEnabled: a bool cannot express "default true", so callers
// wanting discovery should start from DefaultOptions (job 004 does).
type Options struct {
	HandshakeTimeout time.Duration // default 10s
	IdleTimeout      time.Duration // 0 = off; DefaultOptions sets 120s
	// FirstMessageTimeout bounds how long an UNTRUSTED connection
	// may take to send its first message after the handshake
	// (slow-loris guard for the pairing door, round-2 addition).
	// 0 = default 15s; negative = off.
	FirstMessageTimeout time.Duration
	MaxConnsPerPeer     int           // default cfg.Parallelism
	MaxTotalConns       int           // default 64
	BackoffBase         time.Duration // default 1s
	BackoffMax          time.Duration // default 60s
	// Sleep waits between dial attempts; nil = real timer. Tests inject.
	Sleep func(ctx context.Context, d time.Duration) error
	// JitterRand returns [0,1); nil = math/rand/v2. Tests inject.
	JitterRand func() float64
	// MDNSEnabled turns on mDNS discovery when Discovery is nil.
	MDNSEnabled bool
	// Discovery overrides the built-in discovery implementation
	// (tests inject a fake). Nil = mDNS if MDNSEnabled, else static.
	Discovery Discovery
}

// DefaultOptions returns Options with every spec default applied,
// including MDNSEnabled = true.
func DefaultOptions() Options {
	return Options{
		HandshakeTimeout:    10 * time.Second,
		IdleTimeout:         120 * time.Second,
		FirstMessageTimeout: 15 * time.Second,
		MaxTotalConns:       64,
		BackoffBase:         time.Second,
		BackoffMax:          60 * time.Second,
		MDNSEnabled:         true,
	}
}

func (o *Options) applyDefaults(parallelism int) {
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = 10 * time.Second
	}
	// IdleTimeout: 0 means OFF (spec) — the 120s default lives in
	// DefaultOptions, not here (round-2 fix: previously a zero
	// Options silently re-enabled the timeout and there was no way
	// to turn it off). Negative values normalize to off.
	if o.IdleTimeout < 0 {
		o.IdleTimeout = 0
	}
	if o.FirstMessageTimeout == 0 {
		o.FirstMessageTimeout = 15 * time.Second
	} else if o.FirstMessageTimeout < 0 {
		o.FirstMessageTimeout = 0
	}
	if o.MaxConnsPerPeer <= 0 {
		if parallelism > 0 {
			o.MaxConnsPerPeer = parallelism
		} else {
			o.MaxConnsPerPeer = 4
		}
	}
	if o.MaxTotalConns <= 0 {
		o.MaxTotalConns = 64
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = time.Second
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 60 * time.Second
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if o.JitterRand == nil {
		o.JitterRand = rand.Float64
	}
}

// Manager owns the listener, the dialer, the live-connection registry
// and the send bandwidth limiter for one node.
type Manager struct {
	id    *identity.Identity
	cfg   config.Config
	trust TrustSource
	log   *logx.Logger
	opts  Options

	limiter *rate.Limiter // nil = unlimited
	disc    Discovery

	gateMu sync.RWMutex
	gate   PairingGate

	mu        sync.Mutex
	conns     map[string]map[*Conn]struct{}
	total     int
	boundAddr string
}

// NewManager builds a Manager for identity id. cfg supplies the listen
// address, device name, parallelism and bandwidth limit.
func NewManager(id *identity.Identity, cfg config.Config, trust TrustSource, log *logx.Logger, opts Options) *Manager {
	opts.applyDefaults(cfg.Parallelism)
	m := &Manager{
		id:    id,
		cfg:   cfg,
		trust: trust,
		log:   log,
		opts:  opts,
		conns: make(map[string]map[*Conn]struct{}),
	}
	if cfg.BandwidthLimitKBps > 0 {
		m.limiter = rate.NewLimiter(rate.Limit(cfg.BandwidthLimitKBps*1024), chunk.ChunkSize)
	}
	switch {
	case opts.Discovery != nil:
		m.disc = opts.Discovery
	case opts.MDNSEnabled:
		m.disc = newMDNSDiscovery(id, cfg, log)
	default:
		m.disc = staticDiscovery{}
	}
	return m
}

// SetPairingGate installs the pairing gate consulted for every
// untrusted inbound connection.
func (m *Manager) SetPairingGate(g PairingGate) {
	m.gateMu.Lock()
	m.gate = g
	m.gateMu.Unlock()
}

func (m *Manager) pairingOpen() bool {
	m.gateMu.RLock()
	g := m.gate
	m.gateMu.RUnlock()
	return g != nil && g.PairingOpen()
}

// Addr returns the actual bound listen address once Serve is running
// ("" before that). With a ":0" config this is how callers learn the
// chosen port.
func (m *Manager) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.boundAddr
}

// verifyCertShape enforces the non-pinning half of
// identity.VerifyPeerCert: exactly one certificate, parseable, inside
// its validity window. Pinning to a specific device ID is impossible
// on the shared listener (D1: it serves many peers plus pairing), so
// trust is decided by classify() against the store after handshake.
func verifyCertShape(rawCerts [][]byte) error {
	if len(rawCerts) != 1 {
		return fmt.Errorf("transport: expected exactly 1 peer certificate, got %d", len(rawCerts))
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("transport: parse peer certificate: %w", err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return fmt.Errorf("transport: peer certificate outside validity window")
	}
	return nil
}

func (m *Manager) serverTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{m.id.TLSKeyPair()},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyCertShape(rawCerts)
		},
	}
}

// classify decides trust for an authenticated peer identity (D2):
// a store record must exist, not be revoked, and its fingerprint must
// match the certificate actually presented. Store errors fail closed.
func (m *Manager) classify(peerID, fingerprint string) bool {
	peer, found, err := m.trust.GetPeer(peerID)
	if err != nil {
		m.log.Error(context.Background(), "transport: trust lookup failed", logx.String("peer", peerID), logx.Err(err))
		return false
	}
	return found && !peer.Revoked && peer.CertFingerprint == fingerprint
}

func (m *Manager) register(c *Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total >= m.opts.MaxTotalConns {
		return false
	}
	set := m.conns[c.peerID]
	if set == nil {
		set = make(map[*Conn]struct{})
		m.conns[c.peerID] = set
	}
	if len(set) >= m.opts.MaxConnsPerPeer {
		return false
	}
	set[c] = struct{}{}
	m.total++
	return true
}

func (m *Manager) unregister(c *Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if set := m.conns[c.peerID]; set != nil {
		if _, ok := set[c]; ok {
			delete(set, c)
			m.total--
		}
		if len(set) == 0 {
			delete(m.conns, c.peerID)
		}
	}
}

// ClosePeerConns closes every live connection with deviceID and
// returns how many were closed. pairing.Manager calls it on revoke.
func (m *Manager) ClosePeerConns(deviceID string) int {
	m.mu.Lock()
	set := m.conns[deviceID]
	targets := make([]*Conn, 0, len(set))
	for c := range set {
		targets = append(targets, c)
	}
	m.mu.Unlock()
	for _, c := range targets {
		c.Close()
	}
	return len(targets)
}

func (m *Manager) closeAllConns() {
	m.mu.Lock()
	var all []*Conn
	for _, set := range m.conns {
		for c := range set {
			all = append(all, c)
		}
	}
	m.mu.Unlock()
	for _, c := range all {
		c.Close()
	}
}

// Serve announces on discovery, listens, and dispatches connections
// until ctx is cancelled. Trusted connections go to syncHandler after
// the Hello handshake; untrusted ones go to pairHandler only while the
// pairing gate is open, and are closed unread otherwise.
func (m *Manager) Serve(ctx context.Context, syncHandler Handler, pairHandler Handler) error {
	if m.disc != nil {
		if err := m.disc.Start(ctx); err != nil {
			// Discovery is never the only path (static peers still
			// work), so a discovery failure must not kill Serve.
			m.log.Warn(ctx, "transport: discovery start failed", logx.Err(err))
		}
		defer m.disc.Close()
	}
	ln, err := net.Listen("tcp", m.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("transport: listen %s: %w", m.cfg.ListenAddr, err)
	}
	m.mu.Lock()
	m.boundAddr = ln.Addr().String()
	m.mu.Unlock()
	tlsLn := tls.NewListener(ln, m.serverTLSConfig())
	// The watcher goroutine exits on EITHER ctx cancellation (its
	// job: close the listener) or Serve returning for any other
	// reason — previously an early accept error leaked it forever.
	serveDone := make(chan struct{})
	defer close(serveDone)
	go func() {
		select {
		case <-ctx.Done():
			tlsLn.Close()
		case <-serveDone:
		}
	}()
	defer m.closeAllConns()
	for {
		raw, err := tlsLn.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("transport: accept: %w", err)
		}
		go m.handle(ctx, raw.(*tls.Conn), syncHandler, pairHandler)
	}
}

func (m *Manager) handle(serveCtx context.Context, tc *tls.Conn, syncHandler, pairHandler Handler) {
	corrID := logx.NewCorrelationID()
	ctx, cancel := context.WithCancel(logx.WithCorrelationID(serveCtx, corrID))
	defer cancel()

	_ = tc.SetDeadline(time.Now().Add(m.opts.HandshakeTimeout))
	if err := tc.HandshakeContext(ctx); err != nil {
		m.log.Warn(ctx, "transport: handshake failed", logx.Err(err))
		tc.Close()
		return
	}
	_ = tc.SetDeadline(time.Time{})
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) != 1 {
		m.log.Warn(ctx, "transport: unexpected peer certificate count", logx.Int("count", len(certs)))
		tc.Close()
		return
	}
	peerID := identity.DeviceIDFromCertDER(certs[0].Raw)
	fp := identity.FingerprintFromCertDER(certs[0].Raw)
	trusted := m.classify(peerID, fp)

	c := newConn(tc, peerID, fp, trusted, ctx, cancel, m)
	if !m.register(c) {
		m.log.Warn(ctx, "transport: connection limit reached", logx.String("peer", peerID))
		c.Close()
		return
	}
	defer func() {
		m.unregister(c)
		c.Close()
	}()
	m.log.Info(ctx, "transport: connection accepted",
		logx.String("peer", peerID), logx.Bool("trusted", trusted), logx.String("remote", c.RemoteAddr()))

	if !trusted {
		if pairHandler == nil || !m.pairingOpen() {
			// D2: closed immediately after handshake, before any
			// protocol message is read.
			m.log.Warn(ctx, "transport: untrusted peer rejected, pairing closed", logx.String("peer", peerID))
			return
		}
		if m.opts.FirstMessageTimeout > 0 {
			// Slow-loris guard: an untrusted peer gets a short
			// deadline for its first message only; Conn.ReadMessage
			// relaxes to the IdleTimeout regime after it arrives.
			c.setFirstMessageDeadline(time.Now().Add(m.opts.FirstMessageTimeout))
		}
		pairHandler(ctx, c)
		return
	}
	if syncHandler == nil {
		return
	}
	if err := m.acceptHello(ctx, c); err != nil {
		m.log.Warn(ctx, "transport: hello failed", logx.String("peer", peerID), logx.Err(err))
		return
	}
	syncHandler(ctx, c)
}
