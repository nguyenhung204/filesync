package pairing

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filesync/internal/config"
	"filesync/internal/identity"
	"filesync/internal/logx"
	"filesync/internal/store"
	"filesync/internal/transport"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

type pairNode struct {
	id  *identity.Identity
	st  *store.Store
	cfg config.Config
	tm  *transport.Manager
	pm  *Manager
}

func newPairNode(t *testing.T, opts Options, logOut io.Writer) *pairNode {
	t.Helper()
	if logOut == nil {
		logOut = io.Discard
	}
	dir := t.TempDir()
	id, _, err := identity.LoadOrCreate(filepath.Join(dir, "cfg"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.DeviceName = "node-" + id.DeviceID[:5]
	log := logx.New(logOut, logx.LevelDebug)
	tm := transport.NewManager(id, cfg, st, log, transport.Options{})
	pm := NewManager(id, st, log, tm, opts)
	tm.SetPairingGate(pm)
	return &pairNode{id: id, st: st, cfg: cfg, tm: tm, pm: pm}
}

func waitAddr(t *testing.T, mgr *transport.Manager) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for mgr.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("Serve did not start listening")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return mgr.Addr()
}

func (n *pairNode) serve(t *testing.T, syncH transport.Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = n.tm.Serve(ctx, syncH, n.pm.HandleConn) }()
	waitAddr(t, n.tm)
}

func holdHandler(ctx context.Context, c *transport.Conn) {
	for {
		if _, err := c.ReadMessage(ctx); err != nil {
			return
		}
	}
}

func waitPeer(t *testing.T, st *store.Store, deviceID string) (store.Peer, bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, found, err := st.GetPeer(deviceID)
		if err == nil && found {
			return p, true
		}
		if time.Now().After(deadline) {
			return store.Peer{}, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertNoPeers(t *testing.T, st *store.Store) {
	t.Helper()
	peers, err := st.ListPeers()
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("store has %d peers, want 0: %+v", len(peers), peers)
	}
}

func wrongCode(code string) string {
	if code != "000000" {
		return "000000"
	}
	return "000001"
}

// waitPending blocks until m has registered the pending session
// with sessionID. The acceptor registers its side asynchronously in
// the HandleConn goroutine, while DialPair returns as soon as the
// INITIATOR side is registered — confirming on the acceptor without
// this wait races the registration and flakes with "unknown or
// expired session" (round-1 tester finding). Every test that
// confirms/rejects on the acceptor must wait here first.
func waitPending(t *testing.T, m *Manager, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for m.getPending(sessionID) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("session %s never became pending", sessionID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitNoPending blocks until m has no pending sessions left.
func waitNoPending(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for m.pendingCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("manager still has %d pending sessions", m.pendingCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pairUp runs a full happy-path pairing between initiator a and
// acceptor b and returns the code that was used.
func pairUp(t *testing.T, a, b *pairNode) string {
	t.Helper()
	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	pp, err := a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), code)
	if err != nil {
		t.Fatalf("DialPair: %v", err)
	}
	if pp.DeviceID != b.id.DeviceID || pp.Fingerprint != b.id.Fingerprint {
		t.Fatalf("PendingPair = %+v, want device %s", pp, b.id.DeviceID)
	}
	if err := a.pm.ConfirmFingerprint(pp.SessionID); err != nil {
		t.Fatalf("ConfirmFingerprint (initiator): %v", err)
	}
	waitPending(t, b.pm, pp.SessionID)
	if err := b.pm.ConfirmFingerprint(pp.SessionID); err != nil {
		t.Fatalf("ConfirmFingerprint (acceptor): %v", err)
	}
	return code
}

func TestPairHappyPath(t *testing.T) {
	a := newPairNode(t, Options{}, nil)
	b := newPairNode(t, Options{}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code := pairUp(t, a, b)
	_ = code
	pa, ok := waitPeer(t, a.st, b.id.DeviceID)
	if !ok {
		t.Fatal("initiator store never gained the peer")
	}
	if pa.Revoked || pa.CertFingerprint != b.id.Fingerprint {
		t.Errorf("initiator's record = %+v", pa)
	}
	if len(pa.Addresses) != 1 || pa.Addresses[0] != b.tm.Addr() {
		t.Errorf("initiator's addresses = %v, want the dialed address", pa.Addresses)
	}
	pb, ok := waitPeer(t, b.st, a.id.DeviceID)
	if !ok {
		t.Fatal("acceptor store never gained the peer")
	}
	if pb.Revoked || pb.CertFingerprint != a.id.Fingerprint {
		t.Errorf("acceptor's record = %+v", pb)
	}
	if _, found, err := b.st.JournalGet(codeJournalKey); err != nil || found {
		t.Errorf("code record after success: found=%v err=%v, want consumed", found, err)
	}
}

func TestPairWrongCode(t *testing.T) {
	a := newPairNode(t, Options{}, nil)
	b := newPairNode(t, Options{}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	_, err = a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), wrongCode(code))
	if err == nil || !strings.Contains(err.Error(), "bad_proof") {
		t.Fatalf("DialPair with wrong code: err = %v, want bad_proof", err)
	}
	data, found, err := b.st.JournalGet(codeJournalKey)
	if err != nil || !found {
		t.Fatalf("code record missing after one wrong attempt: found=%v err=%v", found, err)
	}
	var rec codeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if rec.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", rec.Attempts)
	}
	assertNoPeers(t, a.st)
	assertNoPeers(t, b.st)
}

func TestPairAttemptsExceeded(t *testing.T) {
	a := newPairNode(t, Options{}, nil)
	b := newPairNode(t, Options{}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	for i := 0; i < MaxAttempts; i++ {
		_, err = a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), wrongCode(code))
		if err == nil || !strings.Contains(err.Error(), "bad_proof") {
			t.Fatalf("attempt %d: err = %v, want bad_proof", i+1, err)
		}
	}
	// The code is destroyed now: even the correct code is refused.
	_, err = a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), code)
	if err == nil || !strings.Contains(err.Error(), "pairing_unavailable") {
		t.Fatalf("6th attempt with correct code: err = %v, want pairing_unavailable", err)
	}
	assertNoPeers(t, a.st)
	assertNoPeers(t, b.st)
}

func TestPairCodeExpired(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	a := newPairNode(t, Options{}, nil)
	b := newPairNode(t, Options{Now: clock.Now}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	clock.Advance(CodeTTL + time.Second)
	_, err = a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), code)
	if err == nil || !strings.Contains(err.Error(), "pairing_expired") {
		t.Fatalf("expired code: err = %v, want pairing_expired", err)
	}
	if _, found, err := b.st.JournalGet(codeJournalKey); err != nil || found {
		t.Errorf("expired record not cleaned up: found=%v err=%v", found, err)
	}
}

func TestPairCodeOneTime(t *testing.T) {
	a := newPairNode(t, Options{}, nil)
	b := newPairNode(t, Options{}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code := pairUp(t, a, b)
	// Finalization (and with it the code's consumption) completes
	// asynchronously in the acceptor's session goroutine after both
	// ConfirmFingerprint calls return. Wait until the code record is
	// actually gone — reusing the code in the gap would legitimately
	// still find an open window (round-2 flake fix).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, found := loadRecordForTest(t, b); !found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("code record not consumed after successful pairing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err := a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), code)
	if err == nil || !strings.Contains(err.Error(), "pairing_unavailable") {
		t.Fatalf("reused code: err = %v, want pairing_unavailable", err)
	}
}

func TestPairNoConfirmNoTrust(t *testing.T) {
	clockA := &fakeClock{t: time.Now()}
	clockB := &fakeClock{t: time.Now()}
	a := newPairNode(t, Options{Now: clockA.Now}, nil)
	b := newPairNode(t, Options{Now: clockB.Now}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	if _, err := a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), code); err != nil {
		t.Fatalf("DialPair: %v", err)
	}
	// Nobody confirms; the confirm window lapses on both clocks.
	clockA.Advance(ConfirmTTL + time.Second)
	clockB.Advance(ConfirmTTL + time.Second)
	time.Sleep(400 * time.Millisecond)
	assertNoPeers(t, a.st)
	assertNoPeers(t, b.st)
	if n := a.pm.pendingCount(); n != 0 {
		t.Errorf("initiator still has %d pending sessions", n)
	}
	if n := b.pm.pendingCount(); n != 0 {
		t.Errorf("acceptor still has %d pending sessions", n)
	}
}

func TestPairOneSideRejects(t *testing.T) {
	a := newPairNode(t, Options{}, nil)
	b := newPairNode(t, Options{}, nil)
	a.serve(t, nil)
	b.serve(t, nil)

	code, _, err := b.pm.CreateCode()
	if err != nil {
		t.Fatalf("CreateCode: %v", err)
	}
	pp, err := a.pm.DialPair(context.Background(), a.tm, b.tm.Addr(), code)
	if err != nil {
		t.Fatalf("DialPair: %v", err)
	}
	if err := a.pm.ConfirmFingerprint(pp.SessionID); err != nil {
		t.Fatalf("ConfirmFingerprint: %v", err)
	}
	waitPending(t, b.pm, pp.SessionID)
	if err := b.pm.RejectFingerprint(pp.SessionID); err != nil {
		t.Fatalf("RejectFingerprint: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	assertNoPeers(t, a.st)
	assertNoPeers(t, b.st)
}
