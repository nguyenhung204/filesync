package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesync/internal/store"
)

func testClock(start time.Time) (func() time.Time, func(time.Duration)) {
	cur := start
	return func() time.Time { return cur }, func(d time.Duration) { cur = cur.Add(d) }
}

func newManager(t *testing.T) (*Manager, string, *store.Store) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	m := NewManager(root, st, 5, 0)
	now, _ := testClock(time.Unix(1_700_000_000, 0))
	m.Now = now
	return m, root, st
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHistory_ArchiveAndKeepLimit(t *testing.T) {
	m, root, _ := newManager(t)
	now, advance := testClock(time.Unix(1_700_000_000, 0))
	m.Now = now
	m.Keep = 3
	for i := 0; i < 5; i++ {
		writeFile(t, root, "doc.txt", strings.Repeat("v", i+1))
		if _, err := m.Archive("doc.txt"); err != nil {
			t.Fatalf("Archive #%d: %v", i, err)
		}
		advance(time.Second)
	}
	versions, err := m.ListVersions("doc.txt")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("kept %d versions, want 3 (keep limit)", len(versions))
	}
	// The surviving versions are the three most recent contents (3,4,5 bytes).
	for i, v := range versions {
		if v.Size != int64(i+3) {
			t.Fatalf("version %d size = %d, want %d", i, v.Size, i+3)
		}
	}
	// Archiving a missing file is a no-op, not an error.
	name, err := m.Archive("ghost.txt")
	if err != nil || name != "" {
		t.Fatalf("Archive missing = %q, %v", name, err)
	}
}

func TestHistory_QuotaEvictsOldestButKeepsNewest(t *testing.T) {
	m, root, _ := newManager(t)
	now, advance := testClock(time.Unix(1_700_000_000, 0))
	m.Now = now
	m.Keep = 100
	m.QuotaBytes = 10
	writeFile(t, root, "a.txt", "12345678") // 8 bytes
	if _, err := m.Archive("a.txt"); err != nil {
		t.Fatal(err)
	}
	advance(time.Second)
	writeFile(t, root, "b.txt", "abcdefgh") // another 8 -> total 16 > quota 10
	if _, err := m.Archive("b.txt"); err != nil {
		t.Fatal(err)
	}
	va, _ := m.ListVersions("a.txt")
	vb, _ := m.ListVersions("b.txt")
	if len(va) != 0 {
		t.Fatalf("oldest version not evicted for quota: %+v", va)
	}
	if len(vb) != 1 {
		t.Fatalf("newest version wrongly evicted: %+v", vb)
	}
}

func TestHistory_RestoreArchivesCurrentFirst(t *testing.T) {
	m, root, _ := newManager(t)
	now, advance := testClock(time.Unix(1_700_000_000, 0))
	m.Now = now
	writeFile(t, root, "f.txt", "old-content")
	archived, err := m.Archive("f.txt")
	if err != nil || archived == "" {
		t.Fatalf("Archive: %q %v", archived, err)
	}
	advance(time.Second)
	writeFile(t, root, "f.txt", "new-content")
	versions, _ := m.ListVersions("f.txt")
	if err := m.Restore("f.txt", versions[0].Name); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "f.txt"))
	if string(data) != "old-content" {
		t.Fatalf("restored content = %q", data)
	}
	// The pre-restore live content was archived, so restore is undoable.
	versions, _ = m.ListVersions("f.txt")
	found := false
	for _, v := range versions {
		if v.Size == int64(len("new-content")) {
			found = true
		}
	}
	if !found {
		t.Fatal("pre-restore content was not archived")
	}
}
