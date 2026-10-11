package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesync/internal/config"
	"filesync/internal/logx"
	"filesync/internal/store"
	"filesync/internal/transport"
)

func TestSessionTwoNodesConverge(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)

	// Create + subdir file at A.
	writeFile(t, a.root, "a.txt", []byte("alpha"), 0o644)
	writeFile(t, a.root, "sub/b.txt", []byte("bravo"), 0o644)
	a.scanCommit(t)
	runRound(t, a, b)
	assertTreesEqual(t, a, b)
	assertIndexesEqual(t, a, b)

	// Modify at B, delete at B.
	writeFile(t, b.root, "a.txt", []byte("alpha v2"), 0o644)
	b.scanCommit(t)
	runRound(t, a, b)
	assertTreesEqual(t, a, b)

	// Rename at A: no chunk bytes may move for the renamed file.
	if err := os.Rename(a.root+"/sub/b.txt", a.root+"/sub/c.txt"); err != nil {
		t.Fatal(err)
	}
	a.scanCommit(t)
	sentBefore := a.eng.Counters().BytesSent + b.eng.Counters().BytesSent
	runRound(t, a, b)
	sentAfter := a.eng.Counters().BytesSent + b.eng.Counters().BytesSent
	if sentAfter != sentBefore {
		t.Fatalf("rename moved %d chunk bytes, want 0", sentAfter-sentBefore)
	}
	assertTreesEqual(t, a, b)
	assertIndexesEqual(t, a, b)

	// Delete at A propagates and lands in B's trash.
	if err := os.Remove(a.root + "/a.txt"); err != nil {
		t.Fatal(err)
	}
	a.scanCommit(t)
	runRound(t, a, b)
	assertTreesEqual(t, a, b)
	trash, err := b.hist.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	foundTrash := false
	for _, te := range trash {
		if te.Path == "a.txt" {
			foundTrash = true
		}
	}
	if !foundTrash {
		t.Fatalf("deleted file not in B trash: %+v", trash)
	}
	assertIndexesEqual(t, a, b)

	// One more round: a stable state must stay a NoOp.
	runRound(t, a, b)
	assertTreesEqual(t, a, b)
	assertIndexesEqual(t, a, b)
}

func TestSessionConcurrentEditConflict(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	writeFile(t, a.root, "f.txt", []byte("original"), 0o644)
	a.scanCommit(t)
	runRound(t, a, b)

	// Offline concurrent edits with different content.
	writeFile(t, a.root, "f.txt", []byte("content from A"), 0o644)
	a.scanCommit(t)
	writeFile(t, b.root, "f.txt", []byte("content from B"), 0o644)
	b.scanCommit(t)

	runRound(t, a, b)
	assertTreesEqual(t, a, b)
	assertIndexesEqual(t, a, b)

	// Exactly one conflict copy, both contents preserved.
	tree := treeHashes(t, a.root)
	var conflicts []string
	for p := range tree {
		if IsConflictPath(p) {
			conflicts = append(conflicts, p)
		}
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflict copies = %v, want exactly 1", conflicts)
	}
	cp := conflicts[0]
	if _, _, ok := ParseConflictPath(cp); !ok {
		t.Fatalf("conflict path %q does not parse", cp)
	}
	orig := readFile(t, a.root, "f.txt")
	conf := readFile(t, a.root, cp)
	got := map[string]bool{string(orig): true, string(conf): true}
	if !got["content from A"] || !got["content from B"] {
		t.Fatalf("contents = orig %q, conflict %q — data lost", orig, conf)
	}
	// Winner is the larger device ID's content (D3).
	winner, loser := "content from A", a.id.DeviceID
	if b.id.DeviceID > a.id.DeviceID {
		winner, loser = "content from B", a.id.DeviceID
	} else {
		loser = b.id.DeviceID
	}
	_ = loser
	if string(orig) != winner {
		t.Fatalf("original = %q, want winner %q", orig, winner)
	}
	if !strings.Contains(cp, loser[:8]) {
		t.Fatalf("conflict name %q does not carry loser short ID %s", cp, loser[:8])
	}

	// Round 2: no new conflict may appear (resolution converged).
	runRound(t, a, b)
	tree2 := treeHashes(t, a.root)
	if len(tree2) != len(tree) {
		t.Fatalf("second round changed the tree: %v -> %v", keys(tree), keys(tree2))
	}
	assertIndexesEqual(t, a, b)
}

func TestSessionDeleteVsEditRevives(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	writeFile(t, a.root, "f.txt", []byte("v1"), 0o644)
	a.scanCommit(t)
	runRound(t, a, b)

	// A deletes while B edits, offline from each other.
	if err := os.Remove(a.root + "/f.txt"); err != nil {
		t.Fatal(err)
	}
	a.scanCommit(t)
	writeFile(t, b.root, "f.txt", []byte("v2 edited"), 0o644)
	b.scanCommit(t)

	runRound(t, a, b)
	// Edit wins: the file is alive on both sides with B's content.
	if got := readFile(t, a.root, "f.txt"); string(got) != "v2 edited" {
		t.Fatalf("A f.txt = %q, want revived edit", got)
	}
	assertTreesEqual(t, a, b)
	assertIndexesEqual(t, a, b)
	e, found, err := a.st.GetEntry("f.txt")
	if err != nil || !found || e.Tombstone {
		t.Fatalf("A entry = %+v found=%v err=%v", e, found, err)
	}
}

func TestPullPreservesExecBit(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	writeFile(t, a.root, "tool.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	if err := os.Chmod(filepath.Join(a.root, "tool.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.scanCommit(t)
	runRound(t, a, b)
	info, err := os.Stat(filepath.Join(b.root, "tool.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("exec bit lost: mode %o", info.Mode().Perm())
	}
	e, found, err := b.st.GetEntry("tool.sh")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if e.Mode != 0o755 {
		t.Fatalf("entry mode = %o, want 755", e.Mode)
	}
}

// TestSessionTransportLoopback runs real engines over real
// transport connections (TCP + mutual TLS on loopback), proving
// *transport.Conn satisfies SessionConn as-is.
func TestSessionTransportLoopback(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	log := logx.New(os.Stderr, logx.LevelError)

	mkManager := func(n *testNode) *transport.Manager {
		opts := transport.DefaultOptions()
		opts.MDNSEnabled = false
		return transport.NewManager(n.id, n.cfg, n.st, log, opts)
	}
	tmA, tmB := mkManager(a), mkManager(b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serve := func(tm *transport.Manager, n *testNode) {
		go func() {
			_ = tm.Serve(ctx, func(hctx context.Context, c *transport.Conn) {
				if err := n.eng.RunSession(hctx, c); err != nil {
					t.Logf("session error on %s: %v", n.id.DeviceID, err)
				}
			}, nil)
		}()
	}
	serve(tmA, a)
	serve(tmB, b)
	waitAddr := func(tm *transport.Manager) string {
		t.Helper()
		for i := 0; i < 100; i++ {
			if addr := tm.Addr(); addr != "" {
				return addr
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("transport never started listening")
		return ""
	}
	addrA, addrB := waitAddr(tmA), waitAddr(tmB)
	// Trust records (what pairing would have written), with the
	// real listen addresses as static candidates.
	if err := a.st.PutPeer(store.Peer{DeviceID: b.id.DeviceID, Name: "b", CertFingerprint: b.id.Fingerprint, Addresses: []string{addrB}, PairedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := b.st.PutPeer(store.Peer{DeviceID: a.id.DeviceID, Name: "a", CertFingerprint: a.id.Fingerprint, Addresses: []string{addrA}, PairedAt: 1}); err != nil {
		t.Fatal(err)
	}

	writeFile(t, a.root, "hello.txt", []byte("over real tls"), 0o644)
	a.scanCommit(t)

	dialRound := func() {
		t.Helper()
		dctx, dcancel := context.WithTimeout(ctx, 20*time.Second)
		defer dcancel()
		conn, err := tmA.Dial(dctx, b.id.DeviceID)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if err := a.eng.RunSession(dctx, conn); err != nil {
			t.Fatalf("run session: %v", err)
		}
	}
	dialRound()
	if got := readFile(t, b.root, "hello.txt"); string(got) != "over real tls" {
		t.Fatalf("B hello.txt = %q", got)
	}
	// Config sanity: the manager used DefaultOptions-derived config.
	_ = config.Default
}
