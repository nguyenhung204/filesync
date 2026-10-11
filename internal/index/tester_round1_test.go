package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesync/internal/safefile"
)

// Tester round 1: "file being written by another app is not indexed",
// exercised against the REAL filesystem instead of an injected stat
// that always disagrees. The injected Sleep hook plays the role of the
// other app: it appends to the file during the 1s stability window,
// between the walk stat and the second stat. The scanner must skip the
// file as unstable (with ErrNotStable), and a later scan — once the
// writer has stopped — must index it normally (deferred, not dropped).
func TestTester_ScannerSkipsFileMutatedDuringStabilityWindow(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "growing.log", "start")
	target := filepath.Join(root, "growing.log")

	mutated := false
	sleep := func(d time.Duration) {
		f, err := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Errorf("mutation open: %v", err)
			return
		}
		if _, err := f.WriteString("-more"); err != nil {
			t.Errorf("mutation write: %v", err)
		}
		f.Close()
		mutated = true
	}
	sc := NewScanner(root, nil, Options{
		StabilityCheck: true,
		Interval:       time.Second,
		Sleep:          sleep,
	})
	res, err := sc.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !mutated {
		t.Fatal("sleep hook never ran; stability window not exercised")
	}
	if len(res.Entries) != 0 {
		t.Fatalf("file mutated mid-scan was indexed: %+v", res.Entries)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Path != "growing.log" || res.Skipped[0].Reason != "unstable" {
		t.Fatalf("skipped = %+v, want one unstable skip for growing.log", res.Skipped)
	}
	if !errors.Is(res.Skipped[0].Err, safefile.ErrNotStable) {
		t.Fatalf("skip Err = %v, want ErrNotStable", res.Skipped[0].Err)
	}

	// Writer has stopped: the next scan indexes the finished file.
	sc2 := NewScanner(root, nil, Options{StabilityCheck: true, Interval: time.Millisecond})
	res2, err := sc2.Scan()
	if err != nil {
		t.Fatalf("Scan #2: %v", err)
	}
	if len(res2.Entries) != 1 || res2.Entries[0].Path != "growing.log" || res2.Entries[0].Size != int64(len("start-more")) {
		t.Fatalf("settled file not indexed on rescan: %+v (skipped %+v)", res2.Entries, res2.Skipped)
	}
}

// Tester round 1: the builder's watcher test covers a subdirectory
// created one level deep. This test goes one level further — a
// directory created INSIDE the newly created directory must also end
// up watched, and a file written there must produce a WatchChanged
// with the full nested path.
func TestTester_WatcherNestedNewSubdirectoryIsWatched(t *testing.T) {
	w, root := newTestWatcher(t, WatchOptions{Debounce: 50 * time.Millisecond, RescanInterval: -1})
	time.Sleep(100 * time.Millisecond)

	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, w, WatchChanged, 10*time.Second)
	if ev.Path != "sub" {
		t.Fatalf("first changed path = %q, want sub", ev.Path)
	}
	inner := filepath.Join(sub, "inner")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	ev = waitEvent(t, w, WatchChanged, 10*time.Second)
	if ev.Path != "sub/inner" {
		t.Fatalf("second changed path = %q, want sub/inner", ev.Path)
	}
	if err := os.WriteFile(filepath.Join(inner, "deep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev = waitEvent(t, w, WatchChanged, 10*time.Second)
	if ev.Path != "sub/inner/deep.txt" {
		t.Fatalf("nested changed path = %q, want sub/inner/deep.txt", ev.Path)
	}
}
