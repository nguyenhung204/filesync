package pairing

// Regression tests for the round-1 review findings:
//   - parallel sessions losing attempts updates / double-binding /
//     double-finalizing on one code (code + security BLOCKER),
//   - BoundID binding at first proof instead of first PairRequest,
//   - CreateCode invalidating sessions opened under the old code,
//   - non-Pair* messages during the confirm phase (D2 discipline).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"filesync/internal/proto"
	"filesync/internal/store"
)

func loadRecordForTest(t *testing.T, n *pairNode) (codeRecord, bool) {
	t.Helper()
	data, found, err := n.st.JournalGet(codeJournalKey)
	if err != nil {
		t.Fatalf("JournalGet: %v", err)
	}
	if !found {
		return codeRecord{}, false
	}
	var rec codeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	return rec, true
}

// TestRound2_ParallelWrongProofsExhaustAttempts fires a wave of
// simultaneous wrong proofs from ONE initiator identity (the
// attacker's parallel connections share one certificate, exactly the
// round-1 security probe). Every wrong proof must be counted:
// attempts must persist at exactly the wave size, the next wrong
// proof must destroy the code, and the correct code must then be
// refused. On the round-1 code this fails: the wave's increments are
// lost into a stale snapshot and attempts persist as 1.
func TestRound2_ParallelWrongProofsExhaustAttempts(t *testing.T) {
	a := newPairNode(t, Options{}, nil)
	b := newPairNode(t, Options{}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	wrong := wrongCode(code)

	const wave = 4 // == MaxConnsPerPeer at the default Parallelism
	start := make(chan struct{})
	errs := make([]error, wave)
	var wg sync.WaitGroup
	for i := 0; i < wave; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), wrong)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err == nil || !strings.Contains(err.Error(), "bad_proof") {
			t.Fatalf("parallel attempt %d: err = %v, want bad_proof", i, err)
		}
	}
	rec, found := loadRecordForTest(t, b)
	if !found {
		t.Fatal("code record vanished before MaxAttempts wrong proofs")
	}
	if rec.Attempts != wave {
		t.Fatalf("persisted attempts = %d, want exactly %d (lost updates under parallelism)", rec.Attempts, wave)
	}

	// One more wrong proof reaches MaxAttempts and destroys the code.
	if _, err := a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), wrong); err == nil ||
		!strings.Contains(err.Error(), "bad_proof") {
		t.Fatalf("attempt %d: err = %v, want bad_proof", wave+1, err)
	}
	if _, found := loadRecordForTest(t, b); found {
		t.Fatal("code record survived MaxAttempts wrong proofs")
	}
	// And the correct code is now refused.
	if _, err := a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), code); err == nil ||
		!strings.Contains(err.Error(), "pairing_unavailable") {
		t.Fatalf("correct code after exhaustion: err = %v, want pairing_unavailable", err)
	}
	assertNoPeers(t, a.st)
	assertNoPeers(t, b.st)
}

// TestRound2_TwoInitiatorsOnlyOneBinds races two different devices
// with the CORRECT code. Exactly one may reach the pending-confirm
// stage; the other is refused pairing_unavailable once the window
// binds at the first submitted proof.
func TestRound2_TwoInitiatorsOnlyOneBinds(t *testing.T) {
	a := newPairNode(t, Options{}, nil)
	c := newPairNode(t, Options{}, nil)
	b := newPairNode(t, Options{}, nil)
	a.serve(t, nil)
	c.serve(t, nil)
	b.serve(t, nil)

	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	type result struct {
		node *pairNode
		pp   PendingPair
		err  error
	}
	results := make([]result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, n := range []*pairNode{a, c} {
		wg.Add(1)
		go func(i int, n *pairNode) {
			defer wg.Done()
			<-start
			pp, err := n.pm.DialPair(context.Background(), n.tm, b.tm.Addr(), code)
			results[i] = result{node: n, pp: pp, err: err}
		}(i, n)
	}
	close(start)
	wg.Wait()

	var winner *result
	successes := 0
	for i := range results {
		r := &results[i]
		if r.err == nil {
			successes++
			winner = r
		} else if !strings.Contains(r.err.Error(), "pairing_unavailable") {
			t.Fatalf("losing initiator: err = %v, want pairing_unavailable", r.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful DialPair calls = %d, want exactly 1", successes)
	}
	if err := winner.node.pm.RejectFingerprint(winner.pp.SessionID); err != nil {
		t.Fatalf("winner cleanup reject: %v", err)
	}
	waitNoPending(t, b.pm)
	assertNoPeers(t, b.st)
}

// TestRound2_CreateCodeInvalidatesLiveSession: a session that has
// passed its proof and sits in the confirm phase must die when the
// acceptor replaces the code — the old code must not be able to
// complete a pairing after being replaced (Flow C.3).
func TestRound2_CreateCodeInvalidatesLiveSession(t *testing.T) {
	a := newPairNode(t, Options{}, nil)
	b := newPairNode(t, Options{}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code1, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode 1: %v", err)
	}
	pp, err := a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), code1)
	if err != nil {
		t.Fatalf("DialPair: %v", err)
	}
	waitPending(t, b.pm, pp.SessionID)

	// Replace the code (loop past the 1-in-a-million collision so the
	// "old code is dead" assertion below is deterministic).
	code2 := code1
	for code2 == code1 {
		code2, _, err = b.pm.CreateCode()
		if err != nil {
			t.Fatalf("CreateCode 2: %v", err)
		}
	}

	// Both sides' pending sessions die on the epoch change.
	waitNoPending(t, b.pm)
	waitNoPending(t, a.pm)
	if err := a.pm.ConfirmFingerprint(pp.SessionID); !errors.Is(err, errNoSession) {
		t.Fatalf("ConfirmFingerprint after invalidation = %v, want errNoSession", err)
	}
	// The old code no longer proves anything.
	if _, err := a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), code1); err == nil ||
		!strings.Contains(err.Error(), "bad_proof") {
		t.Fatalf("old code after replacement: err = %v, want bad_proof", err)
	}
	assertNoPeers(t, a.st)
	assertNoPeers(t, b.st)
}

// TestRound2_NonPairMessageDuringConfirmRejected drives a manual
// initiator through a correct proof, then sends a sync message on the
// still-untrusted pairing connection during the confirm phase: the
// acceptor must answer Error{unpaired} and tear the session down
// (round-1 security NIT: runSession used to ignore such messages).
func TestRound2_NonPairMessageDuringConfirmRejected(t *testing.T) {
	b := newPairNode(t, Options{}, nil)
	b.serve(t, nil)
	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	c := newPairNode(t, Options{}, nil)
	ctx := context.Background()
	conn, _, _, err := c.tm.DialPairing(ctx, b.tm.Addr(), "")
	if err != nil {
		t.Fatalf("DialPairing: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
		Type: proto.TypePairRequest, DeviceID: c.id.DeviceID, Name: "c",
	}}); err != nil {
		t.Fatalf("send request: %v", err)
	}
	chal, err := conn.ReadMessage(ctx)
	if err != nil || chal.Header.Type != proto.TypePairChallenge {
		t.Fatalf("challenge: %v / %v", chal.Header.Type, err)
	}
	nonceB, _ := base64.StdEncoding.DecodeString(chal.Header.Nonce)
	nonceA := make([]byte, 32)
	proof := proofFor(codeKey(code), "initiator", nonceB, nonceA, c.id.DeviceID, b.id.DeviceID)
	if err := conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
		Type: proto.TypePairProof, SessionID: chal.Header.SessionID,
		Nonce: base64.StdEncoding.EncodeToString(nonceA),
		Proof: base64.StdEncoding.EncodeToString(proof),
	}}); err != nil {
		t.Fatalf("send proof: %v", err)
	}
	resp, err := conn.ReadMessage(ctx)
	if err != nil || resp.Header.Type != proto.TypePairAccept {
		t.Fatalf("proof response: %v / %v, want pair_accept", resp.Header.Type, err)
	}
	waitPending(t, b.pm, chal.Header.SessionID)

	// Now misbehave: a sync message on the pairing connection.
	idx, err := proto.IndexMessage([]store.Entry{{Path: "x.txt"}}, false)
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
	waitNoPending(t, b.pm)
	assertNoPeers(t, b.st)

	// Let the read loop observe the close before the test ends.
	time.Sleep(50 * time.Millisecond)
}
