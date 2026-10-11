package history

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"filesync/internal/safefile"
	"filesync/internal/vclock"
)

func TestHistory_MoveToTrashWritesTombstoneAfterMove(t *testing.T) {
	m, root, st := newManager(t)
	writeFile(t, root, "gone.txt", "bye")
	vec := vclock.New().Increment("devA")
	name, err := m.MoveToTrash("gone.txt", vec, "devA")
	if err != nil {
		t.Fatalf("MoveToTrash: %v", err)
	}
	if name == "" {
		t.Fatal("empty trash name")
	}
	if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("file still at original path")
	}
	data, err := os.ReadFile(filepath.Join(root, ".filesync", "trash", name))
	if err != nil || string(data) != "bye" {
		t.Fatalf("trash content = %q, %v", data, err)
	}
	e, ok, err := st.GetEntry("gone.txt")
	if err != nil || !ok {
		t.Fatalf("tombstone lookup: ok=%v err=%v", ok, err)
	}
	if !e.Tombstone || e.TombstoneTs == 0 || e.Vector.Get("devA") != 1 || e.UpdatedBy != "devA" {
		t.Fatalf("tombstone wrong: %+v", e)
	}
	entries, err := m.ListTrash()
	if err != nil || len(entries) != 1 || entries[0].Path != "gone.txt" || entries[0].Size != 3 {
		t.Fatalf("ListTrash: %+v, %v", entries, err)
	}
}

func TestHistory_MoveToTrashLockedFileWritesNoTombstone(t *testing.T) {
	m, root, st := newManager(t)
	writeFile(t, root, "locked.txt", "x")
	m.Rename = func(old, new string) error { return syscall.EBUSY }
	_, err := m.MoveToTrash("locked.txt", vclock.New(), "devA")
	if !errors.Is(err, safefile.ErrLocked) {
		t.Fatalf("err = %v, want wrap of ErrLocked", err)
	}
	if _, ok, _ := st.GetEntry("locked.txt"); ok {
		t.Fatal("tombstone written for a delete that did not happen")
	}
	if _, err := os.Stat(filepath.Join(root, "locked.txt")); err != nil {
		t.Fatal("locked file was moved despite the lock")
	}
}

func TestHistory_MoveToTrashRejectsTraversalAndDirs(t *testing.T) {
	m, root, _ := newManager(t)
	if _, err := m.MoveToTrash("../outside.txt", vclock.New(), "devA"); err == nil {
		t.Fatal("traversal accepted")
	}
	os.MkdirAll(filepath.Join(root, "dir"), 0o755)
	if _, err := m.MoveToTrash("dir", vclock.New(), "devA"); err == nil {
		t.Fatal("directory accepted (history handles files only)")
	}
}

func TestHistory_RestoreFromTrash(t *testing.T) {
	m, root, _ := newManager(t)
	writeFile(t, root, "back.txt", "return me")
	name, err := m.MoveToTrash("back.txt", vclock.New().Increment("devA"), "devA")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreFromTrash(name); err != nil {
		t.Fatalf("RestoreFromTrash: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "back.txt"))
	if string(data) != "return me" {
		t.Fatalf("restored = %q", data)
	}
	entries, _ := m.ListTrash()
	if len(entries) != 0 {
		t.Fatalf("trash not emptied by restore: %+v", entries)
	}
}

func TestHistory_PurgeExpiredByTTL(t *testing.T) {
	m, root, st := newManager(t)
	now, advance := testClock(time.Unix(1_700_000_000, 0))
	m.Now = now
	writeFile(t, root, "old.txt", "old")
	if _, err := m.MoveToTrash("old.txt", vclock.New(), "devA"); err != nil {
		t.Fatal(err)
	}
	advance(31 * 24 * time.Hour) // beyond the 30-day TTL
	writeFile(t, root, "fresh.txt", "fresh")
	if _, err := m.MoveToTrash("fresh.txt", vclock.New(), "devA"); err != nil {
		t.Fatal(err)
	}
	removed, err := m.PurgeExpired()
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the expired one)", removed)
	}
	entries, _ := m.ListTrash()
	if len(entries) != 1 || entries[0].Path != "fresh.txt" {
		t.Fatalf("remaining trash: %+v", entries)
	}
	// The expired tombstone is gone too: no resurrection source remains.
	if _, ok, _ := st.GetEntry("old.txt"); ok {
		t.Fatal("expired tombstone survived purge")
	}
	if e, ok, _ := st.GetEntry("fresh.txt"); !ok || !e.Tombstone {
		t.Fatal("fresh tombstone wrongly purged")
	}
}
