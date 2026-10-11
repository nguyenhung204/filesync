package safefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Round-2 regression (security BLOCKER S1): WriteAtomic must not write
// through a symlinked directory inside the root to a target outside it.
func TestRound2_WriteAtomicRejectsSymlinkDirEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// Overwriting an existing outside file through the symlink.
	if err := WriteAtomic(root, "link/victim.txt", strings.NewReader("pwned"), 0o644); err == nil {
		t.Fatal("WriteAtomic through symlink dir to outside file succeeded, want error")
	}
	data, err := os.ReadFile(filepath.Join(outside, "victim.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" {
		t.Fatalf("outside file was modified through symlink: %q", data)
	}
	// Creating a new outside file through the symlink.
	if err := WriteAtomic(root, "link/new.txt", strings.NewReader("pwned"), 0o644); err == nil {
		t.Fatal("WriteAtomic creating file through symlink dir succeeded, want error")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("file was created outside root through symlink (stat err = %v)", err)
	}
	// Resolve itself must report the escape.
	if _, err := Resolve(root, "link/victim.txt", 0); err == nil {
		t.Fatal("Resolve through symlink dir to outside succeeded, want error")
	} else if !errors.Is(err, ErrSymlinkEscape) {
		t.Fatalf("Resolve error = %v, want it to wrap ErrSymlinkEscape", err)
	}
	// A plain in-root write still works.
	if err := WriteAtomic(root, "ok.txt", strings.NewReader("fine"), 0o644); err != nil {
		t.Fatalf("WriteAtomic in-root: %v", err)
	}
}

// Round-2 regression (code NIT): CheckStableWith must classify a locked
// error from the SECOND stat the same way as from the first.
func TestRound2_CheckStableSecondStatLocked(t *testing.T) {
	calls := 0
	stat := func(string) (int64, int64, error) {
		calls++
		if calls == 2 {
			return 0, 0, syscall.EBUSY
		}
		return 10, 20, nil
	}
	_, err := CheckStableWith("x", Stability{Stat: stat, Sleep: func(time.Duration) {}})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second-stat locked error = %v, want it to wrap ErrLocked", err)
	}
}
