package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"filesync/internal/proto"
)

// Conn is one TLS connection with handshake completed and peer identity
// authenticated from the presented certificate. All message I/O goes
// through proto framing; writes are serialized and bandwidth-limited.
type Conn struct {
	nc      *tls.Conn
	mgr     *Manager
	peerID  string
	fp      string
	trusted bool

	ctx    context.Context
	cancel context.CancelFunc

	// firstDeadline, when set, bounds the arrival of the FIRST
	// message on this connection (untrusted slow-loris guard). reads
	// counts successful reads; both are touched only by the
	// connection's reader path, which is serialized by design (one
	// framing reader per connection).
	firstDeadline time.Time
	reads         int

	wmu  sync.Mutex
	once sync.Once
}

func newConn(nc *tls.Conn, peerID, fp string, trusted bool, ctx context.Context, cancel context.CancelFunc, mgr *Manager) *Conn {
	return &Conn{nc: nc, mgr: mgr, peerID: peerID, fp: fp, trusted: trusted, ctx: ctx, cancel: cancel}
}

// PeerID returns the device ID derived from the peer's certificate
// (never from a message field). It is set for trusted and untrusted
// connections alike — untrusted only means "not in the trust store".
func (c *Conn) PeerID() string { return c.peerID }

// Fingerprint returns the display fingerprint of the peer's
// certificate, as used for pairing confirmation and store records.
func (c *Conn) Fingerprint() string { return c.fp }

// Trusted reports the D2 classification made at accept/dial time.
func (c *Conn) Trusted() bool { return c.trusted }

// RemoteAddr returns the network address of the peer ("host:port").
func (c *Conn) RemoteAddr() string {
	if a := c.nc.RemoteAddr(); a != nil {
		return a.String()
	}
	return ""
}

// Context returns the connection's context: it carries the
// correlation id (the peer id is NOT in the context — use PeerID),
// and is cancelled when the connection closes.
func (c *Conn) Context() context.Context { return c.ctx }

// setFirstMessageDeadline bounds how long the peer may take to send
// its first message; later messages fall back to the IdleTimeout
// regime. Set by the Manager for untrusted connections before the
// handler starts, so no synchronization with the reader is needed.
func (c *Conn) setFirstMessageDeadline(t time.Time) { c.firstDeadline = t }

// Close closes the connection, cancels its context, and removes the
// connection from the manager's registry (freeing its per-peer slot).
// Idempotent; the registry removal is itself idempotent, so the
// accept path's explicit unregister remains safe.
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() {
		c.cancel()
		err = c.nc.Close()
		c.mgr.unregister(c)
	})
	return err
}

// ReadMessage reads one proto message. The effective read deadline
// is the tightest of: the first-message deadline (first read on an
// untrusted connection), now + IdleTimeout (when enabled), and any
// deadline on ctx. With none of these set, the read is unbounded.
func (c *Conn) ReadMessage(ctx context.Context) (proto.Message, error) {
	var deadline time.Time
	if c.reads == 0 && !c.firstDeadline.IsZero() {
		deadline = c.firstDeadline
	}
	if c.mgr.opts.IdleTimeout > 0 {
		if d := time.Now().Add(c.mgr.opts.IdleTimeout); deadline.IsZero() || d.Before(deadline) {
			deadline = d
		}
	}
	if d, ok := ctx.Deadline(); ok && (deadline.IsZero() || d.Before(deadline)) {
		deadline = d
	}
	_ = c.nc.SetReadDeadline(deadline)
	m, err := proto.ReadMessage(c.nc)
	if err != nil {
		return proto.Message{}, err
	}
	c.reads++
	return m, nil
}

// WriteMessage frames and sends one message, serialized against other
// writers on this connection and throttled by the manager-wide send
// limiter (spec D6).
func (c *Conn) WriteMessage(ctx context.Context, m proto.Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.mgr.limiter == nil {
		if err := proto.WriteMessage(c.nc, m); err != nil {
			return fmt.Errorf("transport: write message: %w", err)
		}
		return nil
	}
	return writeLimited(ctx, c.nc, m, c.mgr.limiter)
}

// sendError best-effort delivers an Error message; used on protocol
// failure paths right before the connection is closed.
func (c *Conn) sendError(ctx context.Context, code, msg string) {
	_ = c.WriteMessage(ctx, proto.Message{Header: proto.Header{
		Type: proto.TypeError, Code: code, Message: msg,
	}})
}
