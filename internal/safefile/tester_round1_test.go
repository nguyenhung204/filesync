package safefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tester round 1: the builder's failure test covers a reader error
// (failure BEFORE rename). This test forces the rename step itself to
// fail — the destination path is occupied by a non-empty directory, so
// os.Rename(temp, dst) must fail on Linux — and asserts the pre-existing
// destination is preserved byte-for-byte and no temp file leaks.
func TestTester_WriteAtomicRenameFailureKeepsOldTarget(t *testing.T) {
	root := t.TempDir()
	// Destination "f.txt" is a non-empty directory: rename of a regular
	// temp file onto it cannot succeed.
	dst := filepath.Join(root, "f.txt")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dst, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("old-target-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteAtomic(root, "f.txt", strings.NewReader("new-content"), 0o644); err == nil {
		t.Fatal("WriteAtomic onto a non-empty directory succeeded, want rename error")
	}

	// Old target preserved byte-for-byte.
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "old-target-content" {
		t.Fatalf("old target damaged by failed rename: %q, %v", data, err)
	}
	if info, err := os.Stat(dst); err != nil || !info.IsDir() {
		t.Fatalf("destination changed shape after failed rename: %v %v", info, err)
	}
	// No temp leftovers anywhere under root.
	leftovers := 0
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), TempPrefix) {
			leftovers++
		}
		return nil
	})
	if leftovers != 0 {
		t.Fatalf("%d temp leftovers after rename failure", leftovers)
	}
	// The failure is not sticky: a normal write afterwards still works.
	if err := WriteAtomic(root, "ok.txt", strings.NewReader("fine"), 0o644); err != nil {
		t.Fatalf("WriteAtomic after rename failure: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(root, "ok.txt"))
	if string(data) != "fine" {
		t.Fatalf("follow-up write content = %q", data)
	}
}
