package pairing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"filesync/internal/identity"
	"filesync/internal/proto"
	"filesync/internal/store"
	"filesync/internal/transport"
	"filesync/internal/vclock"
)

type gateOpen bool

func (g gateOpen) PairingOpen() bool { return bool(g) }

// TestPairFinalDoesNotReplaceConfirm scripts a misbehaving acceptor
// that sends PairFinal immediately after PairAccept: the initiator
// must not write any trust — only the user's own confirmation plus
// the peer's PairConfirm can finalize (spec rule 6).
func TestPairFinalDoesNotReplaceConfirm(t *testing.T) {
	a := newPairNode(t, Options{}, nil)
	x := newPairNode(t, Options{}, nil)
	x.tm.SetPairingGate(gateOpen(true))

	const code = "424242"
	handler := func(ctx context.Context, c *transport.Conn) {
		defer c.Close()
		m1, err := c.ReadMessage(ctx)
		if err != nil || m1.Header.Type != proto.TypePairRequest {
			return
		}
		nonceB := bytes.Repeat([]byte{3}, 32)
		session := "0123456789abcdef0123456789abcdef"
		if err := c.WriteMessage(ctx, proto.Message{Header: proto.Header{
			Type: proto.TypePairChallenge, SessionID: session,
			Nonce: base64.StdEncoding.EncodeToString(nonceB),
		}}); err != nil {
			return
		}
		m2, err := c.ReadMessage(ctx)
		if err != nil {
			return
		}
		nonceA, _ := base64.StdEncoding.DecodeString(m2.Header.Nonce)
		proofB := proofFor(codeKey(code), "acceptor", nonceB, nonceA, c.PeerID(), x.id.DeviceID)
		_ = c.WriteMessage(ctx, proto.Message{Header: proto.Header{
			Type: proto.TypePairAccept, SessionID: session,
			Proof: base64.StdEncoding.EncodeToString(proofB),
		}})
		_ = c.WriteMessage(ctx, proto.Message{Header: proto.Header{
			Type: proto.TypePairFinal, SessionID: session,
		}})
		time.Sleep(500 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = x.tm.Serve(ctx, nil, handler) }()
	waitAddr(t, x.tm)

	pp, err := a.pm.DialPair(context.Background(), a.tm, x.tm.Addr(), code)
	if err != nil {
		t.Fatalf("DialPair: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	assertNoPeers(t, a.st)
	if n := a.pm.pendingCount(); n != 1 {
		t.Errorf("pending sessions = %d, want 1 (premature final ignored, session alive)", n)
	}
	_ = a.pm.RejectFingerprint(pp.SessionID)
}

// TestPairProofReplayFails captures a valid proof from session 1 and
// replays it in session 2 (fresh nonces): it must be rejected. Proof
// binding to nonces and device IDs is also checked directly.
func TestPairProofReplayFails(t *testing.T) {
	// Unit-level binding checks.
	key := codeKey("123456")
	nb1, nb2 := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	na := bytes.Repeat([]byte{3}, 32)
	p := proofFor(key, "initiator", nb1, na, "DEV-A", "DEV-B")
	if bytes.Equal(p, proofFor(key, "initiator", nb2, na, "DEV-A", "DEV-B")) {
		t.Error("proof does not bind the acceptor nonce")
	}
	if bytes.Equal(p, proofFor(key, "initiator", nb1, na, "DEV-A", "DEV-C")) {
		t.Error("proof does not bind the acceptor device id")
	}
	if bytes.Equal(p, proofFor(key, "acceptor", nb1, na, "DEV-A", "DEV-B")) {
		t.Error("proof does not bind the role tag")
	}

	b := newPairNode(t, Options{}, nil)
	b.serve(t, nil)
	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	c := newPairNode(t, Options{}, nil)
	ctx := context.Background()

	// Session 1: manual initiator with a correct proof.
	conn1, _, _, err := c.tm.DialPairing(ctx, b.tm.Addr(), "")
	if err != nil {
		t.Fatalf("DialPairing 1: %v", err)
	}
	if err := conn1.WriteMessage(ctx, proto.Message{Header: proto.Header{
		Type: proto.TypePairRequest, DeviceID: c.id.DeviceID, Name: "c",
	}}); err != nil {
		t.Fatalf("send request 1: %v", err)
	}
	chal, err := conn1.ReadMessage(ctx)
	if err != nil || chal.Header.Type != proto.TypePairChallenge {
		t.Fatalf("challenge 1: %v / %v", chal.Header.Type, err)
	}
	nonceB1, _ := base64.StdEncoding.DecodeString(chal.Header.Nonce)
	nonceA := bytes.Repeat([]byte{5}, 32)
	proof1 := proofFor(codeKey(code), "initiator", nonceB1, nonceA, c.id.DeviceID, b.id.DeviceID)
	if err := conn1.WriteMessage(ctx, proto.Message{Header: proto.Header{
		Type: proto.TypePairProof, SessionID: chal.Header.SessionID,
		Nonce: base64.StdEncoding.EncodeToString(nonceA),
		Proof: base64.StdEncoding.EncodeToString(proof1),
	}}); err != nil {
		t.Fatalf("send proof 1: %v", err)
	}
	resp, err := conn1.ReadMessage(ctx)
	if err != nil || resp.Header.Type != proto.TypePairAccept {
		t.Fatalf("session 1 response: %v / %v, want pair_accept", resp.Header.Type, err)
	}
	conn1.Close()
	time.Sleep(150 * time.Millisecond) // let b drop session 1's pending

	// Session 2: replay the captured proof verbatim.
	conn2, _, _, err := c.tm.DialPairing(ctx, b.tm.Addr(), "")
	if err != nil {
		t.Fatalf("DialPairing 2: %v", err)
	}
	defer conn2.Close()
	if err := conn2.WriteMessage(ctx, proto.Message{Header: proto.Header{
		Type: proto.TypePairRequest, DeviceID: c.id.DeviceID, Name: "c",
	}}); err != nil {
		t.Fatalf("send request 2: %v", err)
	}
	chal2, err := conn2.ReadMessage(ctx)
	if err != nil || chal2.Header.Type != proto.TypePairChallenge {
		t.Fatalf("challenge 2: %v / %v", chal2.Header.Type, err)
	}
	nonceB2, _ := base64.StdEncoding.DecodeString(chal2.Header.Nonce)
	if bytes.Equal(nonceB1, nonceB2) {
		t.Fatal("acceptor reused a nonce across sessions")
	}
	if err := conn2.WriteMessage(ctx, proto.Message{Header: proto.Header{
		Type: proto.TypePairProof, SessionID: chal2.Header.SessionID,
		Nonce: base64.StdEncoding.EncodeToString(nonceA),
		Proof: base64.StdEncoding.EncodeToString(proof1), // replayed
	}}); err != nil {
		t.Fatalf("send replayed proof: %v", err)
	}
	resp2, err := conn2.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read replay response: %v", err)
	}
	if resp2.Header.Type != proto.TypePairReject || resp2.Header.Code != proto.CodeBadProof {
		t.Fatalf("replay response = %s/%s, want pair_reject/bad_proof", resp2.Header.Type, resp2.Header.Code)
	}
}

// TestPairBoundDeviceMismatch: once a window is bound to initiator A,
// a different device B cannot use the same code window.
func TestPairBoundDeviceMismatch(t *testing.T) {
	a := newPairNode(t, Options{}, nil)
	b := newPairNode(t, Options{}, nil)
	c := newPairNode(t, Options{}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	pp, err := a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), code)
	if err != nil {
		t.Fatalf("DialPair (bound device): %v", err)
	}
	_, err = c.pm.DialPair(context.Background(), c.tm, b.tm.Addr(), code)
	if err == nil || !strings.Contains(err.Error(), "pairing_unavailable") {
		t.Fatalf("DialPair (other device): err = %v, want pairing_unavailable", err)
	}
	_ = a.pm.RejectFingerprint(pp.SessionID)
	time.Sleep(100 * time.Millisecond)
	assertNoPeers(t, b.st)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestPairCodeNotInLogs runs a wrong attempt plus a full pairing with
// debug logging into a buffer and asserts the buffer contains neither
// the code nor any encoding of its stored hash or salt.
func TestPairCodeNotInLogs(t *testing.T) {
	lb := &lockedBuffer{}
	a := newPairNode(t, Options{}, lb)
	b := newPairNode(t, Options{}, lb)
	a.serve(t, nil)
	b.serve(t, nil)

	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	data, found, err := b.st.JournalGet(codeJournalKey)
	if err != nil || !found {
		t.Fatalf("record: found=%v err=%v", found, err)
	}
	var rec codeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	secrets := map[string]string{
		"code":          code,
		"code hash hex": hex.EncodeToString(rec.CodeHash),
		"code hash b64": base64.StdEncoding.EncodeToString(rec.CodeHash),
		"salt b64":      base64.StdEncoding.EncodeToString(rec.Salt),
	}
	if _, err := a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), wrongCode(code)); err == nil {
		t.Fatal("wrong-code attempt unexpectedly succeeded")
	}
	pairUp(t, a, b)
	out := lb.String()
	if out == "" {
		t.Fatal("log buffer is empty; test would be vacuous")
	}
	for name, s := range secrets {
		if s != "" && strings.Contains(out, s) {
			t.Errorf("log output contains %s", name)
		}
	}
}

// TestRevokeClosesLiveConn: revoke must close the peer's live trusted
// connection promptly and block reconnects (Flow G).
func TestRevokeClosesLiveConn(t *testing.T) {
	a := newPairNode(t, Options{}, nil)
	b := newPairNode(t, Options{}, nil)
	a.serve(t, holdHandler)
	b.serve(t, holdHandler)
	pairUp(t, a, b)
	// Finalization lands asynchronously in the session goroutines;
	// both records must exist before dialing/revoking.
	if _, ok := waitPeer(t, a.st, b.id.DeviceID); !ok {
		t.Fatal("initiator record missing after pairing")
	}
	if _, ok := waitPeer(t, b.st, a.id.DeviceID); !ok {
		t.Fatal("acceptor record missing after pairing")
	}

	ctx := context.Background()
	conn, err := a.tm.Dial(ctx, b.id.DeviceID)
	if err != nil {
		t.Fatalf("Dial after pairing: %v", err)
	}
	defer conn.Close()
	if err := b.pm.Revoke(a.id.DeviceID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if p, found, _ := b.st.GetPeer(a.id.DeviceID); !found || !p.Revoked {
		t.Fatalf("store record after revoke: %+v found=%v", p, found)
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err = conn.ReadMessage(rctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("live connection survived revoke")
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("connection took %v to die after revoke (looks like a timeout, not a close)", elapsed)
	}
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	if _, err := a.tm.Dial(dctx, b.id.DeviceID); err == nil {
		t.Fatal("revoked peer could reconnect")
	}
}

func TestRevokeUnknownPeer(t *testing.T) {
	n := newPairNode(t, Options{}, nil)
	ghost := identity.DeviceIDFromCertDER([]byte("never-paired"))
	if err := n.pm.Revoke(ghost); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Revoke(unknown) = %v, want store.ErrNotFound", err)
	}
}

// TestUntrustedCannotSendIndex: with the pairing window open, an
// untrusted connection that sends a sync message instead of a
// PairRequest gets Error{unpaired} and is closed (spec D2).
func TestUntrustedCannotSendIndex(t *testing.T) {
	b := newPairNode(t, Options{}, nil)
	b.serve(t, nil)
	if _, _, err := b.pm.CreateCode(); err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	attacker := newPairNode(t, Options{}, nil)
	ctx := context.Background()
	conn, _, _, err := attacker.tm.DialPairing(ctx, b.tm.Addr(), "")
	if err != nil {
		t.Fatalf("DialPairing: %v", err)
	}
	defer conn.Close()
	idx, err := proto.IndexMessage([]store.Entry{{Path: "x.txt", Vector: vclock.New()}}, false)
	if err != nil {
		t.Fatalf("IndexMessage: %v", err)
	}
	if err := conn.WriteMessage(ctx, idx); err != nil {
		t.Fatalf("write index: %v", err)
	}
	msg, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if msg.Header.Type != proto.TypeError || msg.Header.Code != proto.CodeUnpaired {
		t.Fatalf("response = %s/%s, want error/unpaired", msg.Header.Type, msg.Header.Code)
	}
}
