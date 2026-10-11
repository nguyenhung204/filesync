package transport

// Regression tests for the round-2 transport fixes: IdleTimeout
// semantics (0 = off, default comes from DefaultOptions) and the
// first-message deadline for untrusted connections.

import (
	"context"
	"testing"
	"time"
)

type gateStub bool

func (g gateStub) PairingOpen() bool { return bool(g) }

func TestRound2_IdleTimeoutSemantics(t *testing.T) {
	// Spec: IdleTimeout 0 = off. A zero Options must therefore NOT
	// silently re-enable the 120s default (round-1 behaviour).
	o := Options{}
	o.applyDefaults(4)
	if o.IdleTimeout != 0 {
		t.Errorf("zero Options: IdleTimeout = %v, want 0 (off)", o.IdleTimeout)
	}
	if o.FirstMessageTimeout != 15*time.Second {
		t.Errorf("zero Options: FirstMessageTimeout = %v, want default 15s", o.FirstMessageTimeout)
	}
	// DefaultOptions carries the spec defaults and applyDefaults
	// must not clobber them.
	d := DefaultOptions()
	if d.IdleTimeout != 120*time.Second {
		t.Errorf("DefaultOptions: IdleTimeout = %v, want 120s", d.IdleTimeout)
	}
	d.applyDefaults(4)
	if d.IdleTimeout != 120*time.Second {
		t.Errorf("DefaultOptions after applyDefaults: IdleTimeout = %v, want 120s", d.IdleTimeout)
	}
	// Negative disables the first-message deadline.
	n := Options{FirstMessageTimeout: -time.Second}
	n.applyDefaults(4)
	if n.FirstMessageTimeout != 0 {
		t.Errorf("negative FirstMessageTimeout = %v, want 0 (off)", n.FirstMessageTimeout)
	}
}

// TestRound2_UntrustedFirstMessageDeadline: an untrusted peer that
// completes the handshake and then stays silent must be cut off
// after FirstMessageTimeout, not after the (here disabled) idle
// timeout — the slow-loris guard from the round-1 security review.
func TestRound2_UntrustedFirstMessageDeadline(t *testing.T) {
	b := newTestNode(t, Options{FirstMessageTimeout: 300 * time.Millisecond})
	b.mgr.SetPairingGate(gateStub(true))
	b.serve(t, nil, discardHandler)

	attacker := newTestNode(t, Options{})
	ctx := context.Background()
	conn, _, _, err := attacker.mgr.DialPairing(ctx, b.mgr.Addr(), "")
	if err != nil {
		t.Fatalf("DialPairing: %v", err)
	}
	defer conn.Close()
	// Send nothing. The server must close the connection shortly
	// after the 300ms first-message deadline; without the guard this
	// read would hang until the test context gives up.
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = conn.ReadMessage(rctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("silent untrusted connection was never closed")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("connection took %v to be closed, want shortly after the 300ms deadline", elapsed)
	}
}
