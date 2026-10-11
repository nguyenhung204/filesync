package store

import (
	"fmt"
	"testing"

	"filesync/internal/vclock"
)

// TestUpsertEntriesBatch: the batch lands whole, replaces
// existing rows, and a bad entry aborts everything (no partial
// commit).
func TestUpsertEntriesBatch(t *testing.T) {
	s := openTestStore(t)
	mk := func(p string, size int64) Entry {
		return Entry{
			Path: p, Size: size, Mode: 0o644, UpdatedBy: "DEV",
			Vector: vclock.New().Increment("DEV"),
		}
	}
	var batch []Entry
	for i := 0; i < 50; i++ {
		batch = append(batch, mk(fmt.Sprintf("f%02d.txt", i), int64(i)))
	}
	if err := s.UpsertEntries(batch); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 50 {
		t.Fatalf("after batch: %d entries, want 50", len(entries))
	}
	// Replace semantics inside a later batch.
	if err := s.UpsertEntries([]Entry{mk("f00.txt", 999)}); err != nil {
		t.Fatal(err)
	}
	e, found, _ := s.GetEntry("f00.txt")
	if !found || e.Size != 999 {
		t.Fatalf("f00.txt = %+v found=%v, want size 999", e, found)
	}
	// A bad entry (negative size) aborts the whole batch.
	bad := []Entry{mk("good.txt", 1), {Path: "bad.txt", Size: -1}}
	if err := s.UpsertEntries(bad); err == nil {
		t.Fatal("batch with negative-size entry succeeded")
	}
	if _, found, _ := s.GetEntry("good.txt"); found {
		t.Fatal("partial batch commit: good.txt persisted despite abort")
	}
	// Empty batch is a no-op.
	if err := s.UpsertEntries(nil); err != nil {
		t.Fatal(err)
	}
}
