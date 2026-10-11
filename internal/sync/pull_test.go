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

	"filesync/internal/chunk"
	"filesync/internal/proto"
	"filesync/internal/safefile"
	"filesync/internal/store"
)

const scriptID = "SCRIPTPEER00000000000000000000000000"

// scriptPeer is a hand-driven SessionConn endpoint that speaks just
// enough of the session protocol to test the pull executor against
// controlled (including buggy/hostile) chunk serving.
type scriptPeer struct {
	conn    *fakeConn
	entries []store.Entry
	data    map[string][]byte
	metas   map[string][]chunk.ChunkMeta

	// chunkOverride, when set and returning non-nil, replaces the
	// honest chunk_data response for (path, ix).
	chunkOverride func(path string, ix int) *proto.Message
	// onChunk fires just before the response for (path, ix) is sent.
	onChunk func(path string, ix int)

	mu       sync.Mutex
	requests map[string][]int
}

func newScriptPeer(entries []store.Entry, contents map[string][]byte) *scriptPeer {
	p := &scriptPeer{
		entries:  entries,
		data:     contents,
		metas:    map[string][]chunk.ChunkMeta{},
		requests: map[string][]int{},
	}
	for path, data := range contents {
		p.metas[path] = metasOf(data)
	}
	return p
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func metasOf(b []byte) []chunk.ChunkMeta {
	metas, err := chunk.HashChunks(bytesReader(b))
	if err != nil {
		panic(err)
	}
	return metas
}

func (p *scriptPeer) requested(path string) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.requests[path]...)
}

func (p *scriptPeer) run(ctx context.Context) {
	for {
		m, err := p.conn.ReadMessage(ctx)
		if err != nil {
			return
		}
		switch m.Header.Type {
		case proto.TypeIndexRequest:
			payload, _ := json.Marshal(p.entries)
			_ = p.conn.WriteMessage(ctx, proto.Message{
				Header:  proto.Header{Type: proto.TypeIndex, More: false},
				Payload: payload,
			})
		case proto.TypeChunkRequest:
			path := m.Header.Path
			for _, ix := range m.Header.Indexes {
				p.mu.Lock()
				p.requests[path] = append(p.requests[path], ix)
				p.mu.Unlock()
				if p.onChunk != nil {
					p.onChunk(path, ix)
				}
				if p.chunkOverride != nil {
					if om := p.chunkOverride(path, ix); om != nil {
						_ = p.conn.WriteMessage(ctx, *om)
						continue
					}
				}
				meta := p.metas[path][ix]
				data := p.data[path][meta.Offset() : meta.Offset()+int64(meta.Size)]
				payload, enc := proto.CompressChunkPayload(data)
				_ = p.conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
					Type: proto.TypeChunkData, Path: path, Index: ix,
					Size: meta.Size, SHA256: meta.HexHash(), Encoding: enc,
				}, Payload: payload})
			}
		case proto.TypeAck:
			_ = p.conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
				Type: proto.TypeAck, AckType: proto.TypeIndex,
			}})
		}
	}
}

// runScriptedRound runs the node's engine against the script peer.
func runScriptedRound(t *testing.T, n *testNode, p *scriptPeer) {
	t.Helper()
	engConn, peerConn := newFakeConnPair(n.id.DeviceID, scriptID)
	p.conn = peerConn
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go p.run(ctx)
	if err := n.eng.RunSession(ctx, engConn); err != nil {
		t.Fatalf("run session: %v", err)
	}
}

func remoteEntry(path string, metas []chunk.ChunkMeta, size int64) store.Entry {
	return store.Entry{
		Path: path, Size: size, MtimeNs: 4242, Mode: 0o644,
		Chunks: metas, Vector: vv(map[string]uint64{scriptID: 1}), UpdatedBy: scriptID,
	}
}

func pattern(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed + byte(i%251)
	}
	return out
}

func TestPullResumeFromJournal(t *testing.T) {
	n := newTestNode(t)
	content := pattern(2*chunk.ChunkSize+100, 7)
	metas := metasOf(content)
	entry := remoteEntry("big.bin", metas, int64(len(content)))

	// Pre-seed journal + staging with chunk 0 already received.
	stg, err := n.eng.prepareStaging(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stg.file.WriteAt(content[:chunk.ChunkSize], 0); err != nil {
		t.Fatal(err)
	}
	stg.bitmap.Set(0)
	if err := n.eng.persistJournal(entry, stg.bitmap); err != nil {
		t.Fatal(err)
	}
	stg.Close()

	p := newScriptPeer([]store.Entry{entry}, map[string][]byte{"big.bin": content})
	runScriptedRound(t, n, p)

	got := p.requested("big.bin")
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("requested indexes = %v, want [1 2] (resume from journal)", got)
	}
	if data := readFile(t, n.root, "big.bin"); !bytes.Equal(data, content) {
		t.Fatal("resumed file content mismatch")
	}
	if _, ok, err := n.st.GetTransfer("big.bin"); err != nil || ok {
		t.Fatalf("journal not cleared: ok=%v err=%v", ok, err)
	}
	want := int64(metas[1].Size + metas[2].Size)
	if n.eng.Counters().BytesReceived != want {
		t.Fatalf("BytesReceived = %d, want %d (only missing chunks)", n.eng.Counters().BytesReceived, want)
	}
}

func TestPullBadChunkRetriesThenFails(t *testing.T) {
	n := newTestNode(t)
	good := []byte("good content")
	goodMetas := metasOf(good)
	bad := pattern(chunk.ChunkSize+10, 3)
	badMetas := metasOf(bad)
	entries := []store.Entry{
		remoteEntry("good.txt", goodMetas, int64(len(good))),
		remoteEntry("bad.bin", badMetas, int64(len(bad))),
	}
	p := newScriptPeer(entries, map[string][]byte{"good.txt": good, "bad.bin": bad})
	// The peer is buggy: bad.bin payloads are corrupted in flight
	// (header still claims the correct hash).
	p.chunkOverride = func(path string, ix int) *proto.Message {
		if path != "bad.bin" {
			return nil
		}
		meta := p.metas[path][ix]
		corrupt := bytes.Repeat([]byte{0xEE}, meta.Size)
		return &proto.Message{Header: proto.Header{
			Type: proto.TypeChunkData, Path: path, Index: ix,
			Size: meta.Size, SHA256: meta.HexHash(),
		}, Payload: corrupt}
	}
	runScriptedRound(t, n, p)

	if data := readFile(t, n.root, "good.txt"); !bytes.Equal(data, good) {
		t.Fatal("good file was not pulled")
	}
	if _, err := os.Stat(filepath.Join(n.root, "bad.bin")); !os.IsNotExist(err) {
		t.Fatal("bad file was finalized despite bad chunks")
	}
	reqs := p.requested("bad.bin")
	count0 := 0
	for _, ix := range reqs {
		if ix == 0 {
			count0++
		}
	}
	if count0 != maxChunkAttempts {
		t.Fatalf("chunk 0 requested %d times, want exactly %d", count0, maxChunkAttempts)
	}
	found := false
	for _, se := range n.eng.LastErrors() {
		if se.Path == "bad.bin" && se.Code == "chunk_checksum" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no chunk_checksum error recorded: %+v", n.eng.LastErrors())
	}
	if _, ok, err := n.st.GetTransfer("bad.bin"); err != nil || !ok {
		t.Fatal("journal for failed file should be kept for the next round")
	}
}

func TestPullDiskFullSkipsOnlyThatFile(t *testing.T) {
	n := newTestNode(t)
	n.eng.freeSpaceFn = func(string) (int64, error) { return 100, nil }
	small := []byte("tiny!")
	smallMetas := metasOf(small)
	bigMetas := []chunk.ChunkMeta{meta(9, chunk.ChunkSize)}
	entries := []store.Entry{
		remoteEntry("small.txt", smallMetas, int64(len(small))),
		remoteEntry("big.bin", bigMetas, chunk.ChunkSize),
	}
	p := newScriptPeer(entries, map[string][]byte{"small.txt": small})
	runScriptedRound(t, n, p)

	if data := readFile(t, n.root, "small.txt"); !bytes.Equal(data, small) {
		t.Fatal("small file blocked by big file's disk failure")
	}
	if got := p.requested("big.bin"); len(got) != 0 {
		t.Fatalf("big.bin was requested (%v) despite the budget refusal", got)
	}
	found := false
	for _, se := range n.eng.LastErrors() {
		if se.Path == "big.bin" && se.Code == "disk_full" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no disk_full error recorded: %+v", n.eng.LastErrors())
	}
}

func TestPullLockedFileDefers(t *testing.T) {
	n := newTestNode(t)
	self := n.id.DeviceID
	writeFile(t, n.root, "old.txt", []byte("old"), 0o644)
	oldMetas := metasOf([]byte("old"))
	local := store.Entry{
		Path: "old.txt", Size: 3, MtimeNs: 1, Mode: 0o644, Chunks: oldMetas,
		Vector: vv(map[string]uint64{self: 1}), UpdatedBy: self,
	}
	if err := n.st.UpsertEntry(local); err != nil {
		t.Fatal(err)
	}
	n.eng.archiveFn = func(rel string) (string, error) {
		return "", fmt.Errorf("simulated lock: %w", safefile.ErrLocked)
	}
	newContent := []byte("new content here")
	newMetas := metasOf(newContent)
	remote := store.Entry{
		Path: "old.txt", Size: int64(len(newContent)), MtimeNs: 2, Mode: 0o644, Chunks: newMetas,
		Vector: vv(map[string]uint64{self: 1, scriptID: 1}), UpdatedBy: scriptID,
	}
	p := newScriptPeer([]store.Entry{remote}, map[string][]byte{"old.txt": newContent})
	runScriptedRound(t, n, p)

	if data := readFile(t, n.root, "old.txt"); string(data) != "old" {
		t.Fatalf("locked file was overwritten: %q", data)
	}
	if _, ok, err := n.st.GetTransfer("old.txt"); err != nil || !ok {
		t.Fatal("journal should be kept for the deferred pull")
	}
	deferred := false
	for _, ps := range n.eng.ActivePulls() {
		if ps.Path == "old.txt" && ps.State == "deferred" {
			deferred = true
		}
	}
	if !deferred {
		t.Fatalf("pull state not deferred: %+v", n.eng.ActivePulls())
	}
	if len(n.eng.LastErrors()) != 0 {
		t.Fatalf("deferred (locked) pull recorded as hard error: %+v", n.eng.LastErrors())
	}
}

func TestPullParentDeletedAbortsFile(t *testing.T) {
	n := newTestNode(t)
	if err := os.MkdirAll(filepath.Join(n.root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := pattern(2*chunk.ChunkSize+10, 11)
	metas := metasOf(content)
	entry := remoteEntry("sub/big.bin", metas, int64(len(content)))
	p := newScriptPeer([]store.Entry{entry}, map[string][]byte{"sub/big.bin": content})
	p.onChunk = func(path string, ix int) {
		if ix == 1 {
			// The user deletes the parent directory mid-transfer.
			_ = os.RemoveAll(filepath.Join(n.root, "sub"))
		}
	}
	runScriptedRound(t, n, p)

	if _, err := os.Stat(filepath.Join(n.root, "sub", "big.bin")); !os.IsNotExist(err) {
		t.Fatal("file finalized into a deleted parent directory")
	}
	if _, ok, err := n.st.GetTransfer("sub/big.bin"); err != nil || !ok {
		t.Fatal("journal should be kept after parent-deleted abort")
	}
	for _, se := range n.eng.LastErrors() {
		if se.Path == "sub/big.bin" {
			t.Fatalf("parent-deleted abort recorded as hard error: %+v", se)
		}
	}
}

func TestPullIgnoredMidFlightCancels(t *testing.T) {
	n := newTestNode(t)
	content := pattern(chunk.ChunkSize+5, 13)
	metas := metasOf(content)
	entry := remoteEntry("ign.bin", metas, int64(len(content)))
	p := newScriptPeer([]store.Entry{entry}, map[string][]byte{"ign.bin": content})
	p.onChunk = func(path string, ix int) {
		if ix == 1 {
			_ = os.WriteFile(filepath.Join(n.root, ".syncignore"), []byte("ign.bin\n"), 0o644)
		}
	}
	runScriptedRound(t, n, p)

	if _, err := os.Stat(filepath.Join(n.root, "ign.bin")); !os.IsNotExist(err) {
		t.Fatal("ignored file was finalized")
	}
	if _, ok, err := n.st.GetTransfer("ign.bin"); err != nil || ok {
		t.Fatal("journal should be dropped when a pull is cancelled by ignore")
	}
	ents, _ := os.ReadDir(filepath.Join(n.root, ".filesync", "partial"))
	if len(ents) != 0 {
		t.Fatalf("staging left behind after ignore-cancel: %v", ents)
	}
}

func TestSessionBadIndexEntrySkipped(t *testing.T) {
	n := newTestNode(t)
	good := []byte("fine")
	goodMetas := metasOf(good)
	entries := []store.Entry{
		remoteEntry("../evil.txt", goodMetas, int64(len(good))),
		remoteEntry("good.txt", goodMetas, int64(len(good))),
	}
	p := newScriptPeer(entries, map[string][]byte{"good.txt": good})
	runScriptedRound(t, n, p)

	if data := readFile(t, n.root, "good.txt"); !bytes.Equal(data, good) {
		t.Fatal("good entry in the same batch as a bad one was not pulled")
	}
	if _, err := os.Stat(filepath.Join(n.root, "..", "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("path traversal entry escaped the sync root")
	}
	found := false
	for _, se := range n.eng.LastErrors() {
		if se.Code == "bad_remote_path" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bad_remote_path not recorded: %+v", n.eng.LastErrors())
	}
	if !strings.Contains(fmt.Sprint(n.eng.LastErrors()), "evil") {
		t.Fatalf("error does not name the bad path: %+v", n.eng.LastErrors())
	}
}
