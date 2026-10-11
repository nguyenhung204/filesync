package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"filesync/internal/chunk"
	"filesync/internal/vclock"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_OpenCreatesWALDatabase(t *testing.T) {
	s := openTestStore(t)
	mode, err := s.JournalMode()
	if err != nil {
		t.Fatalf("JournalMode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	v, err := s.UserVersion()
	if err != nil {
		t.Fatalf("UserVersion: %v", err)
	}
	if v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
}

func TestStore_OpenEmptyDirFails(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded, want error")
	}
}

func TestStore_OpenIsIdempotentAndPreservesData(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	want := Entry{Path: "a/b.txt", Size: 3, MtimeNs: 99, Mode: 0o644,
		Chunks: []chunk.ChunkMeta{{Index: 0, Size: 3}}, Vector: vclock.New().Increment("devA")}
	if err := s1.UpsertEntry(want); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(dir) // reopening migrates nothing and keeps the data
	if err != nil {
		t.Fatalf("Open #2: %v", err)
	}
	defer s2.Close()
	got, ok, err := s2.GetEntry("a/b.txt")
	if err != nil || !ok {
		t.Fatalf("GetEntry: ok=%v err=%v", ok, err)
	}
	if got.Size != 3 || got.MtimeNs != 99 || got.Vector.Get("devA") != 1 || len(got.Chunks) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestStore_OpenRejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	s.Close()
	if _, err := Open(dir); err == nil {
		t.Fatal("Open on schema v99 succeeded, want downgrade-protection error")
	}
}

func TestStore_DatabaseFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	info, err := os.Stat(filepath.Join(dir, "filesync.db"))
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("db perm = %o, want 600", info.Mode().Perm())
	}
}

func TestStore_EntryRoundTrip(t *testing.T) {
	s := openTestStore(t)
	hash := [32]byte{1, 2, 3}
	in := Entry{
		Path: "docs/report.md", Size: 5, MtimeNs: 12345, Mode: 0o755,
		Chunks:    []chunk.ChunkMeta{{Index: 0, SHA256: hash, Size: 5}},
		Vector:    vclock.VersionVector{"devA": 2, "devB": 1},
		UpdatedBy: "devA",
	}
	if err := s.UpsertEntry(in); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	got, ok, err := s.GetEntry(in.Path)
	if err != nil || !ok {
		t.Fatalf("GetEntry: ok=%v err=%v", ok, err)
	}
	if got.Path != in.Path || got.Size != in.Size || got.MtimeNs != in.MtimeNs || got.Mode != in.Mode ||
		got.UpdatedBy != in.UpdatedBy || got.Tombstone {
		t.Fatalf("scalar mismatch: got %+v want %+v", got, in)
	}
	if len(got.Chunks) != 1 || got.Chunks[0] != in.Chunks[0] {
		t.Fatalf("chunks mismatch: %+v", got.Chunks)
	}
	if got.Vector.Compare(in.Vector) != vclock.Equal {
		t.Fatalf("vector mismatch: %v vs %v", got.Vector, in.Vector)
	}
	// Upsert replaces fully.
	in.Size = 9
	in.Chunks = []chunk.ChunkMeta{}
	if err := s.UpsertEntry(in); err != nil {
		t.Fatalf("UpsertEntry #2: %v", err)
	}
	got, _, _ = s.GetEntry(in.Path)
	if got.Size != 9 || len(got.Chunks) != 0 || got.Chunks == nil || got.Vector == nil {
		t.Fatalf("after replace: %+v (chunks nil=%v)", got, got.Chunks == nil)
	}
}

func TestStore_EntryNotFoundAndValidation(t *testing.T) {
	s := openTestStore(t)
	if _, ok, err := s.GetEntry("nope.txt"); err != nil || ok {
		t.Fatalf("GetEntry missing: ok=%v err=%v", ok, err)
	}
	if err := s.UpsertEntry(Entry{Path: ""}); err == nil {
		t.Fatal("empty path accepted")
	}
	if err := s.UpsertEntry(Entry{Path: "x", Size: -1}); err == nil {
		t.Fatal("negative size accepted")
	}
	if err := s.DeleteEntry("nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteEntry missing: %v, want ErrNotFound", err)
	}
}

func TestStore_ListEntriesSortedAndTombstonePurge(t *testing.T) {
	s := openTestStore(t)
	for _, p := range []string{"b.txt", "a.txt", "c/d.txt"} {
		if err := s.UpsertEntry(Entry{Path: p, Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertEntry(Entry{Path: "gone.txt", Tombstone: true, TombstoneTs: 100}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListEntries()
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 4 || entries[0].Path != "a.txt" || entries[3].Path != "gone.txt" {
		t.Fatalf("order/content: %+v", entries)
	}
	n, err := s.PurgeTombstones(200)
	if err != nil || n != 1 {
		t.Fatalf("PurgeTombstones(200) = %d, %v; want 1", n, err)
	}
	if _, ok, _ := s.GetEntry("gone.txt"); ok {
		t.Fatal("tombstone survived purge")
	}
	// A tombstone exactly at the cutoff is kept (< is strict).
	if err := s.UpsertEntry(Entry{Path: "edge.txt", Tombstone: true, TombstoneTs: 200}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PurgeTombstones(200); n != 0 {
		t.Fatalf("purge at cutoff removed %d, want 0", n)
	}
}

func TestStore_CorruptRowDataIsErrorNotPanic(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.db.Exec(`INSERT INTO files (path, size, chunks) VALUES ('bad.txt', 1, 'not json')`); err != nil {
		t.Fatalf("insert corrupt: %v", err)
	}
	if _, _, err := s.GetEntry("bad.txt"); err == nil {
		t.Fatal("corrupt chunks decoded without error")
	}
	if _, err := s.db.Exec(`UPDATE files SET chunks = X'', vector = 'not json' WHERE path = 'bad.txt'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetEntry("bad.txt"); err == nil {
		t.Fatal("corrupt vector decoded without error")
	}
}

func TestStore_ConcurrentUse(t *testing.T) {
	s := openTestStore(t)
	done := make(chan error, 8)
	for g := 0; g < 8; g++ {
		go func(g int) {
			for i := 0; i < 20; i++ {
				e := Entry{Path: "f.txt", Size: int64(g*100 + i)}
				if err := s.UpsertEntry(e); err != nil {
					done <- err
					return
				}
				if _, _, err := s.GetEntry("f.txt"); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}(g)
	}
	for g := 0; g < 8; g++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent use: %v", err)
		}
	}
}
