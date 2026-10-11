package transport

// Tester round 1 additions: bidirectional simultaneous exchange,
// unlimited-bandwidth fast path, bounded backoff attempts against a
// dead address, and Dial through a fake Discovery backend.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"filesync/internal/proto"
	"filesync/internal/store"
)

type fakeDiscovery struct{ cands []Candidate }

func (f fakeDiscovery) Start(context.Context) error { return nil }
func (f fakeDiscovery) Candidates() []Candidate     { return f.cands }
func (f fakeDiscovery) Close() error                { return nil }

func pingPong(t *testing.T, ctx context.Context, c *Conn) {
	t.Helper()
	if err := c.WriteMessage(ctx, proto.Message{Header: proto.Header{Type: proto.TypePing}}); err != nil {
		t.Fatalf("WriteMessage ping: %v", err)
	}
	msg, err := c.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("ReadMessage pong: %v", err)
	}
	if msg.Header.Type != proto.TypePong {
		t.Fatalf("got %s, want pong", msg.Header.Type)
	}
}

// Both peers dial each other at the same time; the internal Hello
// exchange and a Ping/Pong must succeed in both directions.
func TestTester_BidirectionalSimultaneousExchange(t *testing.T) {
	a := newTestNode(t, Options{})
	b := newTestNode(t, Options{})
	peerChA := make(chan string, 2)
	peerChB := make(chan string, 2)
	a.serve(t, echoHandler(peerChA), nil)
	b.serve(t, echoHandler(peerChB), nil)
	pairBoth(t, a, b)

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	conns := make([]*Conn, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		conns[0], errs[0] = a.mgr.Dial(ctx, b.id.DeviceID)
	}()
	go func() {
		defer wg.Done()
		conns[1], errs[1] = b.mgr.Dial(ctx, a.id.DeviceID)
	}()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("simultaneous dial %d: %v", i, err)
		}
	}
	defer conns[0].Close()
	defer conns[1].Close()
	if conns[0].PeerID() != b.id.DeviceID || conns[1].PeerID() != a.id.DeviceID {
		t.Errorf("peer IDs = %s / %s", conns[0].PeerID(), conns[1].PeerID())
	}
	pingPong(t, ctx, conns[0])
	pingPong(t, ctx, conns[1])
	// Each server side must have seen the other as its peer.
	for _, tc := range []struct {
		ch   chan string
		want string
	}{{peerChB, a.id.DeviceID}, {peerChA, b.id.DeviceID}} {
		select {
		case got := <-tc.ch:
			if got != tc.want {
				t.Errorf("server saw peer %s, want %s", got, tc.want)
			}
		case <-time.After(2 * time.Second):
			t.Error("server handler never ran for one direction")
		}
	}
}

// BandwidthLimitKBps == 0 must mean "no limiter": 2 MiB goes out at
// loopback speed, nowhere near the ~2s the 512 KiB/s limit imposes.
func TestTester_UnlimitedBandwidthIsFast(t *testing.T) {
	a := newTestNode(t, Options{})
	b := newTestNode(t, Options{})
	if a.cfg.BandwidthLimitKBps != 0 {
		t.Fatalf("default BandwidthLimitKBps = %d, want 0", a.cfg.BandwidthLimitKBps)
	}
	a.serve(t, discardHandler, nil)
	b.serve(t, discardHandler, nil)
	pairBoth(t, a, b)

	ctx := context.Background()
	conn, err := a.mgr.Dial(ctx, b.id.DeviceID)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	payload := make([]byte, 1<<20)
	sum := sha256.Sum256(payload)
	start := time.Now()
	for i := 0; i < 2; i++ {
		err := conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
			Type: proto.TypeChunkData, Path: "big.bin", Index: i,
			Size: len(payload), SHA256: hex.EncodeToString(sum[:]),
		}, Payload: payload})
		if err != nil {
			t.Fatalf("WriteMessage %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("2 MiB with limit=0 took %v; limiter appears active when it must be off", elapsed)
	}
}

// DialWithBackoff against a dead address must wait between attempts
// (exponential, capped) instead of spinning: with an injected Sleep
// the number of retries stays exactly the number of sleeps, and the
// recorded delays follow [50ms 100ms 120ms 120ms 120ms] for
// Base=50ms/Max=120ms — a busy loop would try thousands of times.
func TestTester_BackoffBoundedAttemptsDeadAddress(t *testing.T) {
	var delays []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := Options{
		BackoffBase: 50 * time.Millisecond,
		BackoffMax:  120 * time.Millisecond,
		JitterRand:  func() float64 { return 0.5 },
		Sleep: func(c context.Context, d time.Duration) error {
			delays = append(delays, d)
			if len(delays) >= 5 {
				cancel()
			}
			return c.Err()
		},
	}
	n := newTestNode(t, opts)
	peerID := n.id.DeviceID // any well-formed ID; peer record below is what matters
	_ = peerID
	other := newTestNode(t, Options{})
	if err := n.st.PutPeer(store.Peer{
		DeviceID: other.id.DeviceID, CertFingerprint: other.id.Fingerprint,
		Addresses: []string{"127.0.0.1:1"}, // dead: connection refused instantly
	}); err != nil {
		t.Fatalf("PutPeer: %v", err)
	}
	if _, err := n.mgr.DialWithBackoff(ctx, other.id.DeviceID); err == nil {
		t.Fatal("DialWithBackoff to dead address returned no error")
	}
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 120 * time.Millisecond, 120 * time.Millisecond, 120 * time.Millisecond}
	if len(delays) != len(want) {
		t.Fatalf("retries (sleeps) = %d (%v), want %d bounded retries", len(delays), delays, len(want))
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delay[%d] = %v, want %v (exponential, capped)", i, delays[i], want[i])
		}
	}
}

// A peer with no static addresses must still be reachable when the
// Discovery backend (fake here; mDNS in production) reports it.
func TestTester_FakeDiscoveryDial(t *testing.T) {
	b := newTestNode(t, Options{})
	b.serve(t, echoHandler(nil), nil)
	fake := fakeDiscovery{cands: []Candidate{{DeviceID: b.id.DeviceID, Name: "b", Addr: b.mgr.Addr()}}}
	a := newTestNode(t, Options{Discovery: fake})
	a.serve(t, echoHandler(nil), nil)
	// a knows b's fingerprint but has NO static address for b.
	if err := a.st.PutPeer(store.Peer{
		DeviceID: b.id.DeviceID, Name: "peer",
		CertFingerprint: b.id.Fingerprint, PairedAt: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("PutPeer a: %v", err)
	}
	putPeer(t, b.st, a)

	cands := a.mgr.Candidates(b.id.DeviceID)
	found := false
	for _, c := range cands {
		if c == b.mgr.Addr() {
			found = true
		}
	}
	if !found {
		t.Fatalf("Candidates = %v, fake discovery address %s missing", cands, b.mgr.Addr())
	}
	conn, err := a.mgr.Dial(context.Background(), b.id.DeviceID)
	if err != nil {
		t.Fatalf("Dial via fake discovery: %v", err)
	}
	defer conn.Close()
	pingPong(t, context.Background(), conn)
}
