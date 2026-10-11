package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Round-2 regression (security BLOCKER S2a): MoveToTrash must not move a
// file that lives outside the root behind a symlinked directory.
func TestRound2_MoveToTrashRejectsSymlinkDirEscape(t *testing.T) {
	m, root, st := newManager(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("outside-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MoveToTrash("link/victim.txt", nil, "dev-a"); err == nil {
		t.Fatal("MoveToTrash through symlink dir succeeded, want error")
	}
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("outside file vanished after MoveToTrash attempt: %v", err)
	}
	if string(data) != "outside-data" {
		t.Fatalf("outside file content changed: %q", data)
	}
	trash, err := m.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(trash) != 0 {
		t.Fatalf("trash holds %d entries, want 0: %+v", len(trash), trash)
	}
	if _, ok, err := st.GetEntry("link/victim.txt"); err != nil || ok {
		t.Fatalf("tombstone recorded for escaped path: ok=%v err=%v", ok, err)
	}
}

// Round-2 regression (security BLOCKER S2b): Archive must not copy the
// target of a symlink file into version history.
func TestRound2_ArchiveRejectsSymlinkFile(t *testing.T) {
	m, root, _ := newManager(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Archive("leak.txt"); err == nil {
		t.Fatal("Archive of symlink file succeeded, want error")
	}
	versions, err := m.ListVersions("leak.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 0 {
		t.Fatalf("symlink target content leaked into history: %+v", versions)
	}
	// A regular file still archives normally.
	writeFile(t, root, "real.txt", "real-data")
	if _, err := m.Archive("real.txt"); err != nil {
		t.Fatalf("Archive regular file: %v", err)
	}
}

// Round-2 regression (code BLOCKER C1): restoring the OLDEST version
// while the file already has Keep versions must not let the archive of
// the live file prune the very version being restored.
func TestRound2_RestoreOldestVersionAtKeepLimit(t *testing.T) {
	m, root, _ := newManager(t)
	now, advance := testClock(time.Unix(1_700_000_000, 0))
	m.Now = now
	m.Keep = 5
	contents := []string{"content-1", "content-2", "content-3", "content-4", "content-5"}
	for _, c := range contents {
		writeFile(t, root, "doc.txt", c)
		if _, err := m.Archive("doc.txt"); err != nil {
			t.Fatalf("Archive %q: %v", c, err)
		}
		advance(time.Second)
	}
	versions, err := m.ListVersions("doc.txt")
	if err != nil || len(versions) != 5 {
		t.Fatalf("versions = %d, %v; want 5", len(versions), err)
	}
	oldest := versions[0]
	// Live file moves on; restoring the oldest version archives it first,
	// which is exactly what used to prune the restore target away.
	writeFile(t, root, "doc.txt", "content-6-live")
	if err := m.Restore("doc.txt", oldest.Name); err != nil {
		t.Fatalf("Restore oldest at keep limit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content-1" {
		t.Fatalf("restored content = %q, want content-1", data)
	}
	// The restored version itself is still in history, and the
	// pre-restore live content was archived (restore stays undoable).
	versions, err = m.ListVersions("doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	var sawTarget, sawLive bool
	for _, v := range versions {
		if v.Name == oldest.Name {
			sawTarget = true
		}
		if v.Size == int64(len("content-6-live")) {
			sawLive = true
		}
	}
	if !sawTarget {
		t.Fatal("restore target version disappeared from history")
	}
	if !sawLive {
		t.Fatal("pre-restore live content was not archived")
	}
	if len(versions) > 5 {
		t.Fatalf("versions = %d, want <= keep limit 5", len(versions))
	}
}
