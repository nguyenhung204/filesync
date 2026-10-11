package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"filesync/internal/config"
	"filesync/internal/identity"
	"filesync/internal/logx"
	"filesync/internal/proto"
	"filesync/internal/store"
)

type testNode struct {
	id  *identity.Identity
	st  *store.Store
	cfg config.Config
	mgr *Manager
}

func newTestNode(t *testing.T, opts Options) *testNode {
	t.Helper()
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
	log := logx.New(io.Discard, logx.LevelDebug)
	return &testNode{id: id, st: st, cfg: cfg, mgr: NewManager(id, cfg, st, log, opts)}
}

func (n *testNode) serve(t *testing.T, syncH, pairH Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = n.mgr.Serve(ctx, syncH, pairH) }()
	deadline := time.Now().Add(5 * time.Second)
	for n.mgr.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("Serve did not start listening")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// echoHandler answers Ping with Pong and reports the peer id of each
// connection it serves on peerCh.
func echoHandler(peerCh chan<- string) Handler {
	return func(ctx context.Context, c *Conn) {
		if peerCh != nil {
			peerCh <- c.PeerID()
		}
		for {
			msg, err := c.ReadMessage(ctx)
			if err != nil {
				return
			}
			if msg.Header.Type == proto.TypePing {
				_ = c.WriteMessage(ctx, proto.Message{Header: proto.Header{Type: proto.TypePong}})
			}
		}
	}
}

// discardHandler reads and drops everything until the conn dies.
func discardHandler(ctx context.Context, c *Conn) {
	for {
		if _, err := c.ReadMessage(ctx); err != nil {
			return
		}
	}
}

func putPeer(t *testing.T, st *store.Store, other *testNode) {
	t.Helper()
	err := st.PutPeer(store.Peer{
		DeviceID:        other.id.DeviceID,
		Name:            "peer",
		CertFingerprint: other.id.Fingerprint,
		Addresses:       []string{other.mgr.Addr()},
		PairedAt:        time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("PutPeer: %v", err)
	}
}

func pairBoth(t *testing.T, a, b *testNode) {
	t.Helper()
	putPeer(t, a.st, b)
	putPeer(t, b.st, a)
}

func TestTrustedPeerConnect(t *testing.T) {
	a := newTestNode(t, Options{})
	b := newTestNode(t, Options{})
	peerCh := make(chan string, 1)
	a.serve(t, echoHandler(nil), nil)
	b.serve(t, echoHandler(peerCh), nil)
	pairBoth(t, a, b)

	ctx := context.Background()
	conn, err := a.mgr.Dial(ctx, b.id.DeviceID)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if conn.PeerID() != b.id.DeviceID {
		t.Errorf("client PeerID = %s, want %s", conn.PeerID(), b.id.DeviceID)
	}
	if !conn.Trusted() {
		t.Error("client conn not marked trusted")
	}
	if err := conn.WriteMessage(ctx, proto.Message{Header: proto.Header{Type: proto.TypePing}}); err != nil {
		t.Fatalf("WriteMessage ping: %v", err)
	}
	msg, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msg.Header.Type != proto.TypePong {
		t.Errorf("got %s, want pong", msg.Header.Type)
	}
	select {
	case got := <-peerCh:
		if got != a.id.DeviceID {
			t.Errorf("server saw peer %s, want %s", got, a.id.DeviceID)
		}
	case <-time.After(2 * time.Second):
		t.Error("server handler never ran")
	}
}

func TestHelloVersionMismatch(t *testing.T) {
	a := newTestNode(t, Options{})
	b := newTestNode(t, Options{})
	a.serve(t, echoHandler(nil), nil)
	b.serve(t, echoHandler(nil), nil)
	pairBoth(t, a, b)

	conn, srvID, _, err := a.mgr.DialPairing(context.Background(), b.mgr.Addr(), "")
	if err != nil {
		t.Fatalf("DialPairing: %v", err)
	}
	defer conn.Close()
	if srvID != b.id.DeviceID {
		t.Fatalf("server id = %s", srvID)
	}
	ctx := context.Background()
	err = conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
		Type: proto.TypeHello, Protocol: 1, DeviceID: a.id.DeviceID,
	}})
	if err != nil {
		t.Fatalf("write hello v1: %v", err)
	}
	msg, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read error response: %v", err)
	}
	if msg.Header.Type != proto.TypeError || msg.Header.Code != proto.CodeProtocolVersion {
		t.Fatalf("got %s/%s, want error/protocol_version", msg.Header.Type, msg.Header.Code)
	}
	// Connection must be closed right after.
	if _, err := conn.ReadMessage(ctx); err == nil {
		t.Error("connection still open after version mismatch")
	}
}

func TestCertIDMismatchRejected(t *testing.T) {
	a := newTestNode(t, Options{})
	b := newTestNode(t, Options{})
	a.serve(t, echoHandler(nil), nil)
	b.serve(t, echoHandler(nil), nil)
	pairBoth(t, a, b)

	conn, _, _, err := a.mgr.DialPairing(context.Background(), b.mgr.Addr(), "")
	if err != nil {
		t.Fatalf("DialPairing: %v", err)
	}
	defer conn.Close()
	ctx := context.Background()
	// Hello claims b's own ID while presenting a's certificate.
	// Protocol must be the current version: the version check runs
	// before the identity check and would mask the mismatch.
	err = conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
		Type: proto.TypeHello, Protocol: proto.ProtocolVersion, DeviceID: b.id.DeviceID,
	}})
	if err != nil {
		t.Fatalf("write hello: %v", err)
	}
	msg, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read error response: %v", err)
	}
	if msg.Header.Type != proto.TypeError || msg.Header.Code != proto.CodeBadMessage {
		t.Fatalf("got %s/%s, want error/bad_message", msg.Header.Type, msg.Header.Code)
	}
}

func TestUnknownPeerClosedWhenPairingClosed(t *testing.T) {
	b := newTestNode(t, Options{})
	b.serve(t, echoHandler(nil), nil) // no pairing gate at all
	stranger := newTestNode(t, Options{})

	conn, _, _, err := stranger.mgr.DialPairing(context.Background(), b.mgr.Addr(), "")
	if err != nil {
		t.Fatalf("DialPairing handshake: %v", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := conn.ReadMessage(ctx); err == nil {
		t.Fatal("untrusted connection was readable; want it closed before any protocol data")
	}
}

func TestRevokedPeerCannotReconnect(t *testing.T) {
	a := newTestNode(t, Options{})
	b := newTestNode(t, Options{})
	a.serve(t, echoHandler(nil), nil)
	b.serve(t, echoHandler(nil), nil)
	pairBoth(t, a, b)
	if err := b.st.RevokePeer(a.id.DeviceID); err != nil {
		t.Fatalf("RevokePeer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.mgr.Dial(ctx, b.id.DeviceID); err == nil {
		t.Fatal("revoked peer could still dial in")
	}
}

func TestBackoffDelays(t *testing.T) {
	run := func(maxD time.Duration, want []time.Duration) {
		var delays []time.Duration
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		opts := Options{
			BackoffBase: time.Second,
			BackoffMax:  maxD,
			JitterRand:  func() float64 { return 0.5 }, // factor exactly 1.0
			Sleep: func(c context.Context, d time.Duration) error {
				delays = append(delays, d)
				if len(delays) >= len(want) {
					cancel()
				}
				return c.Err()
			},
		}
		n := newTestNode(t, opts)
		err := n.st.PutPeer(store.Peer{
			DeviceID: "peer", CertFingerprint: "fp",
			Addresses: []string{"127.0.0.1:1"}, // closed port: instant refusal
		})
		// Candidates look the peer up by ID; use a real generated ID.
		if err != nil {
			t.Fatalf("PutPeer: %v", err)
		}
		peerID := identity.DeviceIDFromCertDER([]byte("backoff-peer"))
		_ = n.st.PutPeer(store.Peer{DeviceID: peerID, CertFingerprint: "fp", Addresses: []string{"127.0.0.1:1"}})
		_, err = n.mgr.DialWithBackoff(ctx, peerID)
		if err == nil {
			t.Fatal("DialWithBackoff to a dead port returned no error")
		}
		if len(delays) != len(want) {
			t.Fatalf("delays = %v, want %v", delays, want)
		}
		for i := range want {
			if delays[i] != want[i] {
				t.Errorf("delay[%d] = %v, want %v", i, delays[i], want[i])
			}
		}
	}
	run(60*time.Second, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second})
	run(3*time.Second, []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second})
}

func TestBandwidthLimit(t *testing.T) {
	a := newTestNode(t, Options{})
	b := newTestNode(t, Options{})
	a.cfg.BandwidthLimitKBps = 512
	// Rebuild a's manager with the limit (NewManager reads cfg).
	a.mgr = NewManager(a.id, a.cfg, a.st, logx.New(io.Discard, logx.LevelDebug), Options{})
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
	for i := range payload {
		payload[i] = byte(i)
	}
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
	elapsed := time.Since(start)
	// 2 MiB at 512 KiB/s with 1 MiB burst: at least (2-1)/0.5 = 2s, allow 10% slack.
	if elapsed < 1800*time.Millisecond {
		t.Errorf("sent 2 MiB in %v; bandwidth limit not enforced (want >= ~2s)", elapsed)
	}
	if elapsed > 15*time.Second {
		t.Errorf("sent 2 MiB in %v; suspiciously slow", elapsed)
	}
}

func TestMaxConnsPerPeer(t *testing.T) {
	a := newTestNode(t, Options{})
	b := newTestNode(t, Options{MaxConnsPerPeer: 2})
	a.serve(t, echoHandler(nil), nil)
	b.serve(t, echoHandler(nil), nil)
	pairBoth(t, a, b)

	ctx := context.Background()
	c1, err := a.mgr.Dial(ctx, b.id.DeviceID)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	c2, err := a.mgr.Dial(ctx, b.id.DeviceID)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer c2.Close()
	if _, err := a.mgr.Dial(ctx, b.id.DeviceID); err == nil {
		t.Fatal("third connection accepted beyond MaxConnsPerPeer=2")
	}
	c1.Close()
	// Once the server notices c1 is gone, a new dial must succeed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c3, err := a.mgr.Dial(ctx, b.id.DeviceID)
		if err == nil {
			c3.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("redial after freeing a slot never succeeded: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestStaticPeerDial(t *testing.T) {
	a := newTestNode(t, Options{})
	b := newTestNode(t, Options{})
	a.serve(t, echoHandler(nil), nil)
	b.serve(t, echoHandler(nil), nil)
	// First address dead, second the real one (no mDNS anywhere).
	err := a.st.PutPeer(store.Peer{
		DeviceID: b.id.DeviceID, CertFingerprint: b.id.Fingerprint,
		Addresses: []string{"127.0.0.1:1", b.mgr.Addr()},
	})
	if err != nil {
		t.Fatalf("PutPeer: %v", err)
	}
	putPeer(t, b.st, a)
	conn, err := a.mgr.Dial(context.Background(), b.id.DeviceID)
	if err != nil {
		t.Fatalf("Dial via static fallback: %v", err)
	}
	defer conn.Close()
	if conn.PeerID() != b.id.DeviceID {
		t.Errorf("PeerID = %s", conn.PeerID())
	}
	if got := a.mgr.Candidates(b.id.DeviceID); len(got) != 2 || got[0] != "127.0.0.1:1" {
		t.Errorf("Candidates = %v, want static addresses in order", got)
	}
}

func TestDiscoveryFiltering(t *testing.T) {
	self := identity.DeviceIDFromCertDER([]byte("self-cert"))
	other := identity.DeviceIDFromCertDER([]byte("other-cert"))
	mk := func(id string) rawEntry {
		return rawEntry{Port: "22000", Text: []string{"id=" + id, "v=1", "name=x"},
			IPv4: []string{"10.0.0.9"}, IPv6: []string{"fe80::1"}}
	}
	if _, ok := candidateFromRaw(mk(self), self); ok {
		t.Error("own record not filtered")
	}
	if _, ok := candidateFromRaw(mk("NOT-A-DEVICE-ID"), self); ok {
		t.Error("record with invalid device id not filtered")
	}
	c, ok := candidateFromRaw(mk(other), self)
	if !ok {
		t.Fatal("valid record filtered")
	}
	if c.Addr != "10.0.0.9:22000" {
		t.Errorf("Addr = %s, want IPv4 preferred", c.Addr)
	}
	v6 := mk(other)
	v6.IPv4 = nil
	c, ok = candidateFromRaw(v6, self)
	if !ok || c.Addr != "[fe80::1]:22000" {
		t.Errorf("IPv6 fallback = %+v, %v", c, ok)
	}
	tr := truncateRunes(strings.Repeat("é", 40), 64)
	if len(tr) > 64 || !utf8.ValidString(tr) {
		t.Errorf("truncateRunes: %d bytes, valid=%v", len(tr), utf8.ValidString(tr))
	}
}

func TestMDNSRealDiscovery(t *testing.T) {
	if strings.TrimSpace(os.Getenv("FILESYNC_TEST_MDNS")) != "1" {
		t.Skip("set FILESYNC_TEST_MDNS=1 to run the real mDNS test (needs multicast)")
	}
	// Reserve real ports: mDNS announces the configured port, so ":0"
	// cannot be used here.
	freePort := func() int {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve port: %v", err)
		}
		defer ln.Close()
		return ln.Addr().(*net.TCPAddr).Port
	}
	mkNode := func() *testNode {
		n := newTestNode(t, DefaultOptions())
		n.cfg.ListenAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort()))
		n.mgr = NewManager(n.id, n.cfg, n.st, logx.New(io.Discard, logx.LevelDebug), DefaultOptions())
		return n
	}
	a := mkNode()
	b := mkNode()
	a.serve(t, echoHandler(nil), nil)
	b.serve(t, echoHandler(nil), nil)
	pairBoth(t, a, b)
	deadline := time.Now().Add(15 * time.Second)
	for {
		found := false
		for _, c := range a.mgr.Candidates(b.id.DeviceID) {
			if c == b.mgr.Addr() {
				found = true
			}
		}
		_ = found
		// The mDNS candidate shows up through Candidates(peerID).
		if len(a.mgr.Candidates(b.id.DeviceID)) > 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mDNS discovery never surfaced the peer")
		}
		time.Sleep(200 * time.Millisecond)
	}
	conn, err := a.mgr.Dial(context.Background(), b.id.DeviceID)
	if err != nil {
		t.Fatalf("Dial after mDNS discovery: %v", err)
	}
	conn.Close()
}
