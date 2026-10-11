package store

import (
	"bytes"
	"testing"

	"filesync/internal/chunk"
	"filesync/internal/vclock"
)

// Tester round 1: reopening the DB a second (and third) time must run
// migration idempotently and preserve EVERY category of data, not just
// one entry: entries (incl. tombstone), peers (incl. revoked), journal
// values, and transfer journals with the bitmap byte-for-byte.
func TestTester_StoreReopenTwicePreservesAllData(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	hash := [32]byte{9, 8, 7}
	vec := vclock.VersionVector{"devA": 3, "devB": 1}
	if err := s1.UpsertEntry(Entry{Path: "keep.txt", Size: 4, MtimeNs: 111, Mode: 0o755,
		Chunks: []chunk.ChunkMeta{{Index: 0, SHA256: hash, Size: 4}}, Vector: vec, UpdatedBy: "devA"}); err != nil {
		t.Fatal(err)
	}
	if err := s1.UpsertEntry(Entry{Path: "gone.txt", Tombstone: true, TombstoneTs: 222, Vector: vec, UpdatedBy: "devB"}); err != nil {
		t.Fatal(err)
	}
	if err := s1.PutPeer(Peer{DeviceID: "devA", Name: "Alpha", Addresses: []string{"10.0.0.2:9000"}, PairedAt: 42}); err != nil {
		t.Fatal(err)
	}
	if err := s1.PutPeer(Peer{DeviceID: "devB", Name: "Beta"}); err != nil {
		t.Fatal(err)
	}
	if err := s1.RevokePeer("devB"); err != nil {
		t.Fatal(err)
	}
	if err := s1.JournalSet("note", []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	bitmap := []byte{0xA5, 0x00, 0xFF, 0x5A}
	if err := s1.PutTransfer(TransferJournal{Path: "big.bin", Size: 1 << 20, TotalChunks: 4, Bitmap: bitmap, UpdatedBy: "devA"}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}

	for reopen := 2; reopen <= 3; reopen++ {
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("Open #%d (migration must be idempotent): %v", reopen, err)
		}
		if v, err := s.UserVersion(); err != nil || v != 1 {
			t.Fatalf("reopen #%d: user_version = %d, %v; want 1", reopen, v, err)
		}
		if mode, err := s.JournalMode(); err != nil || mode != "wal" {
			t.Fatalf("reopen #%d: journal_mode = %q, %v; want wal", reopen, mode, err)
		}
		entries, err := s.ListEntries()
		if err != nil || len(entries) != 2 {
			t.Fatalf("reopen #%d: entries = %+v, %v", reopen, entries, err)
		}
		keep, ok, err := s.GetEntry("keep.txt")
		if err != nil || !ok || keep.Size != 4 || keep.MtimeNs != 111 || keep.Mode != 0o755 ||
			len(keep.Chunks) != 1 || keep.Chunks[0].SHA256 != hash || keep.Vector.Get("devA") != 3 || keep.Vector.Get("devB") != 1 {
			t.Fatalf("reopen #%d: keep.txt mismatch: %+v ok=%v err=%v", reopen, keep, ok, err)
		}
		gone, ok, err := s.GetEntry("gone.txt")
		if err != nil || !ok || !gone.Tombstone || gone.TombstoneTs != 222 {
			t.Fatalf("reopen #%d: tombstone mismatch: %+v ok=%v err=%v", reopen, gone, ok, err)
		}
		peers, err := s.ListPeers()
		if err != nil || len(peers) != 2 || peers[0].DeviceID != "devA" || peers[1].DeviceID != "devB" {
			t.Fatalf("reopen #%d: peers = %+v, %v", reopen, peers, err)
		}
		if peers[0].Revoked || !peers[1].Revoked {
			t.Fatalf("reopen #%d: revoked flags lost: %+v", reopen, peers)
		}
		if len(peers[0].Addresses) != 1 || peers[0].Addresses[0] != "10.0.0.2:9000" {
			t.Fatalf("reopen #%d: addresses lost: %+v", reopen, peers[0].Addresses)
		}
		val, ok, err := s.JournalGet("note")
		if err != nil || !ok || !bytes.Equal(val, []byte{1, 2, 3}) {
			t.Fatalf("reopen #%d: journal = %v ok=%v err=%v", reopen, val, ok, err)
		}
		// Returned journal value must be an independent copy.
		val[0] = 99
		val2, _, _ := s.JournalGet("note")
		if val2[0] != 1 {
			t.Fatalf("reopen #%d: journal value aliases caller slice", reopen)
		}
		tr, ok, err := s.GetTransfer("big.bin")
		if err != nil || !ok || tr.Size != 1<<20 || tr.TotalChunks != 4 || !bytes.Equal(tr.Bitmap, bitmap) {
			t.Fatalf("reopen #%d: transfer mismatch: %+v ok=%v err=%v", reopen, tr, ok, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", reopen, err)
		}
	}
}
