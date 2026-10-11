package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filesync/internal/proto"
	"filesync/internal/store"
)

// TestSessionMetaTreeEntriesRejected is the regression test for
// the round-1 security BLOCKER 2: a scripted peer advertised
// entries inside our own metadata tree (.filesync/planted.txt,
// .filesync/versions/notes.txt/v9-fake,
// .filesync/trash/fake-deleted.txt) and the engine pulled and
// finalized all three with attacker content, upserted them into
// the local index, and would have re-advertised them to the rest
// of the mesh — forging entries in the version-history/trash
// safety net. The exact probe paths are replayed here: none may
// touch disk or the store, each must be recorded as
// bad_remote_path, and an honest entry in the same index must
// still sync.
func TestSessionMetaTreeEntriesRejected(t *testing.T) {
	n := newTestNode(t)
	good := []byte("fine")
	goodMetas := metasOf(good)
	pwn := []byte("PWNED")
	pwnMetas := metasOf(pwn)
	metaPaths := []string{
		".filesync/planted.txt",
		".filesync/versions/notes.txt/v9-fake",
		".filesync/trash/fake-deleted.txt",
	}
	entries := []store.Entry{
		remoteEntry(metaPaths[0], pwnMetas, int64(len(pwn))),
		remoteEntry(metaPaths[1], pwnMetas, int64(len(pwn))),
		remoteEntry(metaPaths[2], pwnMetas, int64(len(pwn))),
		remoteEntry("good.txt", goodMetas, int64(len(good))),
	}
	contents := map[string][]byte{"good.txt": good}
	for _, p := range metaPaths {
		contents[p] = pwn
	}
	p := newScriptPeer(entries, contents)
	runScriptedRound(t, n, p)

	if data := readFile(t, n.root, "good.txt"); !bytes.Equal(data, good) {
		t.Fatal("honest entry in the same index as meta-tree entries was not pulled")
	}
	for _, mp := range metaPaths {
		if _, err := os.Stat(filepath.Join(n.root, filepath.FromSlash(mp))); !os.IsNotExist(err) {
			t.Fatalf("peer planted a file in the metadata tree: %s", mp)
		}
		if _, found, err := n.st.GetEntry(mp); err != nil || found {
			t.Fatalf("meta-tree entry entered the local index: %s (found=%v err=%v)", mp, found, err)
		}
	}
	// No file anywhere under .filesync may carry the planted bytes
	// (staging/journal files for the honest pull are cleaned up).
	err := filepath.WalkDir(filepath.Join(n.root, ".filesync"), func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(path)
		if rerr == nil && bytes.Equal(data, pwn) {
			t.Fatalf("planted content found in metadata tree at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	recorded := map[string]bool{}
	for _, se := range n.eng.LastErrors() {
		if se.Code == "bad_remote_path" {
			recorded[se.Path] = true
		}
	}
	for _, mp := range metaPaths {
		if !recorded[mp] {
			t.Fatalf("no bad_remote_path recorded for %s: %+v", mp, n.eng.LastErrors())
		}
	}
}

// floodPeer streams index batches with More=true forever (valid
// entries, honest chunk serving for anything actually requested)
// until the connection dies. It models the round-1 IMPORTANT 2
// attacker: per-message limits all pass; only an accumulated cap
// stops the growth.
type floodPeer struct {
	conn      *fakeConn
	batchSize int
	done      chan struct{}

	mu   sync.Mutex
	sent int // entries streamed so far
}

func (p *floodPeer) run(ctx context.Context) {
	defer close(p.done)
	batch := make([]store.Entry, p.batchSize)
	for n := 0; ; n++ {
		// Re-stamp the batch with fresh paths so the receiver
		// cannot dedupe its way out of the growth.
		for i := range batch {
			batch[i] = remoteEntry(fmt.Sprintf("flood/b%d-%06d.txt", n, i), nil, 0)
		}
		payload, _ := json.Marshal(batch)
		err := p.conn.WriteMessage(ctx, proto.Message{
			Header:  proto.Header{Type: proto.TypeIndex, More: true},
			Payload: payload,
		})
		if err != nil {
			return
		}
		p.mu.Lock()
		p.sent += len(batch)
		p.mu.Unlock()
		// Answer whatever the engine asks while it is still
		// willing to talk (index requests, acks).
		select {
		case m := <-p.conn.inbox:
			if m.Header.Type == proto.TypeAck {
				_ = p.conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
					Type: proto.TypeAck, AckType: proto.TypeIndex,
				}})
			}
		default:
		}
	}
}

func (p *floodPeer) sentEntries() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent
}

// TestSessionIndexFloodCapped is the regression test for round-1
// IMPORTANT 2: session.dispatch appended every index batch to
// s.remote with no accumulated limit, so a peer streaming valid
// 1000-entry batches with More=true grew the heap ~56 MiB/s until
// the OOM killer beat the 60s index timeout. The round must now
// abort at the accumulated caps (entries AND payload bytes),
// quickly and with a named error; nothing from the flood may
// reach the store, and a normal round afterwards must still work.
func TestSessionIndexFloodCapped(t *testing.T) {
	n := newTestNode(t)
	engConn, peerConn := newFakeConnPair(n.id.DeviceID, scriptID)
	fp := &floodPeer{conn: peerConn, batchSize: proto.MaxIndexBatch, done: make(chan struct{})}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go fp.run(ctx)

	start := time.Now()
	err := n.eng.RunSession(ctx, engConn)
	elapsed := time.Since(start)
	// Stop the flood before asserting on its counters.
	_ = peerConn.Close()
	<-fp.done
	if err == nil {
		t.Fatal("flooded session round succeeded; want an index-limit abort")
	}
	if !strings.Contains(err.Error(), "index") || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("round error does not name the index limit: %v", err)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("flood abort took %v; the cap must bite well before the 60s index timeout", elapsed)
	}
	// The flood was cut at the caps, not at the peer's whim: the
	// peer streamed far more than any legitimate round needs, but
	// the engine stopped listening.
	if fp.sentEntries() <= maxIndexEntriesPerRound {
		t.Fatalf("peer only sent %d entries; test did not actually exceed the entry cap %d", fp.sentEntries(), maxIndexEntriesPerRound)
	}
	entries, lerr := n.st.ListEntries()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(entries) != 0 {
		t.Fatalf("flooded entries reached the store: %d", len(entries))
	}
	found := false
	for _, se := range n.eng.LastErrors() {
		if se.Code == "message_too_large" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no message_too_large recorded for the flood: %+v", n.eng.LastErrors())
	}

	// A normal round against an honest peer still works afterwards.
	good := []byte("after the flood")
	entries2 := []store.Entry{remoteEntry("after.txt", metasOf(good), int64(len(good)))}
	p := newScriptPeer(entries2, map[string][]byte{"after.txt": good})
	runScriptedRound(t, n, p)
	if data := readFile(t, n.root, "after.txt"); !bytes.Equal(data, good) {
		t.Fatal("honest round after a flood abort did not sync")
	}
}
