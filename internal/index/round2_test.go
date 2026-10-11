package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Round-2 regression (security BLOCKER S3): a file swapped for a symlink
// to an outside file between the walk and the hash must be skipped, never
// hashed — even when the stability stat is fooled into reporting the
// original size/mtime.
func TestRound2_ScannerSkipsFileSwappedForSymlinkBeforeHash(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside-secret-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := writeFile(t, root, "target.txt", "legit")
	target := filepath.Join(root, "target.txt")

	statCalls := 0
	sc := NewScanner(root, nil, Options{
		StabilityCheck: true,
		Interval:       time.Millisecond,
		Sleep: func(time.Duration) {
			// The swap happens in the window after the walk collected
			// the candidate and before it is hashed (here: during the
			// stability pass over the tree).
			_ = os.Remove(target)
			_ = os.Symlink(filepath.Join(outside, "secret.txt"), target)
		},
		Stat: func(string) (int64, int64, error) {
			statCalls++
			// The pre-fix scanner trusted these numbers: they match
			// the walk exactly, so the file looked "stable".
			return info.Size(), info.ModTime().UnixNano(), nil
		},
	})
	res, err := sc.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if statCalls == 0 {
		t.Fatal("injected Stat was never called; stability pass did not run")
	}
	for _, e := range res.Entries {
		if e.Path == "target.txt" {
			t.Fatalf("swapped-in symlink was indexed (size %d = outside content); want it skipped", e.Size)
		}
	}
	foundSkip := false
	for _, sk := range res.Skipped {
		if sk.Path == "target.txt" {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Fatalf("no skip recorded for swapped file; skipped=%+v entries=%+v", res.Skipped, res.Entries)
	}
}

// Round-2 regression (code BLOCKER C2): a .filesync tree that appears
// AFTER the watcher started must neither be watched nor produce
// WatchChanged events (internal versions/trash churn is not user data).
func TestRound2_WatcherIgnoresMetaTreeCreatedAfterStart(t *testing.T) {
	w, root := newTestWatcher(t, WatchOptions{Debounce: 50 * time.Millisecond, RescanInterval: -1})
	time.Sleep(100 * time.Millisecond)

	meta := filepath.Join(root, ".filesync", "versions", "doc.txt")
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(meta, "123-doc.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".filesync", "trash"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".filesync", "trash", "456-old.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Collect everything the watcher emits while the meta tree churns.
	deadline := time.After(900 * time.Millisecond)
collect:
	for {
		select {
		case ev := <-w.Events():
			if ev.Kind == WatchChanged && (ev.Path == ".filesync" || strings.HasPrefix(ev.Path, ".filesync/")) {
				t.Fatalf("watcher emitted change for meta tree: %q", ev.Path)
			}
		case <-deadline:
			break collect
		}
	}

	// The watcher is still alive for real user files.
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, w, WatchChanged, 10*time.Second)
	if ev.Path != "real.txt" {
		t.Fatalf("changed path = %q, want real.txt", ev.Path)
	}
}
