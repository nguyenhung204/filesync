package index

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"filesync/internal/chunk"
	"filesync/internal/history"
	"filesync/internal/safefile"
	"filesync/internal/store"
	"filesync/internal/vclock"
)

// TestIntegration_SafeReceiveOrdering replays Flow B end to end at package
// level, in the exact order job 003's sync engine must use: journal ->
// receive chunks -> RequireSpace -> Archive -> WriteAtomic -> only then
// UpsertEntry -> ClearTransfer.
func TestIntegration_SafeReceiveOrdering(t *testing.T) {
	root := t.TempDir()
	cfgDir := t.TempDir()
	st, err := store.Open(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hm := history.NewManager(root, st, 5, 0)

	// Existing file at the destination.
	writeFile(t, root, "f.txt", "old")

	content := []byte("brand new content")
	metas, err := chunk.HashChunks(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	bm := chunk.NewBitmap(len(metas))
	bm.Set(0)
	raw, _ := bm.MarshalBinary()
	vec := vclock.New().Increment("devB")

	// 1. Journal first.
	if err := st.PutTransfer(store.TransferJournal{
		Path: "f.txt", Size: int64(len(content)), TotalChunks: len(metas), Bitmap: raw, UpdatedBy: "devB",
	}); err != nil {
		t.Fatal(err)
	}
	// 3. Space check.
	if err := safefile.RequireSpace(root, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	// 4. Archive the current version before overwriting.
	if _, err := hm.Archive("f.txt"); err != nil {
		t.Fatal(err)
	}
	// 5. Atomic write.
	if err := safefile.WriteAtomic(root, "f.txt", bytes.NewReader(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// 6. Store update only after the rename.
	info, _ := os.Stat(filepath.Join(root, "f.txt"))
	if err := st.UpsertEntry(store.Entry{
		Path: "f.txt", Size: info.Size(), MtimeNs: info.ModTime().UnixNano(),
		Mode: 0o644, Chunks: metas, Vector: vec, UpdatedBy: "devB",
	}); err != nil {
		t.Fatal(err)
	}
	// 7. Clear the journal.
	if err := st.ClearTransfer("f.txt"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(root, "f.txt"))
	if string(data) != string(content) {
		t.Fatalf("destination = %q", data)
	}
	versions, err := hm.ListVersions("f.txt")
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions after receive: %+v, %v (old version must be archived)", versions, err)
	}
	e, ok, _ := st.GetEntry("f.txt")
	if !ok || e.Size != int64(len(content)) || e.Vector.Get("devB") != 1 {
		t.Fatalf("entry after receive: %+v ok=%v", e, ok)
	}
	if transfers, _ := st.ListTransfers(); len(transfers) != 0 {
		t.Fatalf("journal not cleared: %+v", transfers)
	}
}

// TestIntegration_KillMidTransferRecovery simulates Flow D: a receiver
// killed mid-transfer leaves (a) a journal with a partial bitmap, (b) a
// leftover temp file, (c) the OLD destination file intact. On restart:
// the store reopens consistently, CleanTemps removes the temp, and the
// journal tells the engine exactly which chunks are missing.
func TestIntegration_KillMidTransferRecovery(t *testing.T) {
	root := t.TempDir()
	cfgDir := t.TempDir()

	func() {
		st, err := store.Open(cfgDir)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		writeFile(t, root, "big.bin", "original-complete-content")
		// Partial temp from the interrupted WriteAtomic/receive.
		os.WriteFile(filepath.Join(root, safefile.TempPrefix+"deadbeef"), []byte("partial junk"), 0o644)
		bm := chunk.NewBitmap(4)
		bm.Set(0)
		bm.Set(2)
		raw, _ := bm.MarshalBinary()
		if err := st.PutTransfer(store.TransferJournal{
			Path: "big.bin", Size: 4 << 20, TotalChunks: 4, Bitmap: raw, UpdatedBy: "devB",
		}); err != nil {
			t.Fatal(err)
		}
		// "kill -9": close without any cleanup (Close only releases the
		// handle; committed transactions are already durable).
	}()

	// Restart.
	st, err := store.Open(cfgDir)
	if err != nil {
		t.Fatalf("reopen after kill: %v", err)
	}
	defer st.Close()
	n, err := safefile.CleanTemps(root)
	if err != nil || n != 1 {
		t.Fatalf("CleanTemps = %d, %v; want 1", n, err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "big.bin"))
	if string(data) != "original-complete-content" {
		t.Fatalf("destination was truncated by the crash: %q", data)
	}
	transfers, err := st.ListTransfers()
	if err != nil || len(transfers) != 1 {
		t.Fatalf("transfers after restart: %+v, %v", transfers, err)
	}
	var bm chunk.Bitmap
	if err := bm.UnmarshalBinary(transfers[0].Bitmap); err != nil {
		t.Fatal(err)
	}
	if missing := bm.Missing(); len(missing) != 2 || missing[0] != 1 || missing[1] != 3 {
		t.Fatalf("resume missing = %v, want [1 3]", missing)
	}
	// The scan after restart sees a consistent tree.
	res, err := NewScanner(root, st, Options{}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Path != "big.bin" {
		t.Fatalf("scan after restart: %+v", res.Entries)
	}
}
