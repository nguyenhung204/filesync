package index

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"filesync/internal/store"
)

func newTestWatcher(t *testing.T, opts WatchOptions) (*Watcher, string) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sc := NewScanner(root, st, Options{})
	w, err := NewWatcher(sc, opts)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go w.Start(ctx)
	t.Cleanup(func() {
		cancel()
		w.Close()
	})
	return w, root
}

func waitEvent(t *testing.T, w *Watcher, want WatchKind, timeout time.Duration) WatchEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-w.Events():
			if ev.Kind == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %v event within %v", want, timeout)
			return WatchEvent{}
		}
	}
}

func TestWatcher_ReportsChangedPath(t *testing.T) {
	w, root := newTestWatcher(t, WatchOptions{Debounce: 50 * time.Millisecond, RescanInterval: -1})
	// Let the loop settle into its select before producing events.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, w, WatchChanged, 10*time.Second)
	if ev.Path != "new.txt" {
		t.Fatalf("changed path = %q, want new.txt", ev.Path)
	}
}

func TestWatcher_NewSubdirectoryIsWatched(t *testing.T) {
	w, root := newTestWatcher(t, WatchOptions{Debounce: 50 * time.Millisecond, RescanInterval: -1})
	time.Sleep(100 * time.Millisecond)
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, WatchChanged, 10*time.Second) // the mkdir itself
	if err := os.WriteFile(filepath.Join(sub, "deep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, w, WatchChanged, 10*time.Second)
	if ev.Path != "sub/deep.txt" {
		t.Fatalf("changed path = %q, want sub/deep.txt", ev.Path)
	}
}

func TestWatcher_OverflowThrottleAndCallback(t *testing.T) {
	var overflows atomic.Int32
	w, _ := newTestWatcher(t, WatchOptions{
		RescanInterval: time.Hour, // after the first, further overflow rescans are throttled
		OnOverflow:     func() { overflows.Add(1) },
	})
	// First overflow: callback + rescan event.
	w.signalOverflow()
	waitEvent(t, w, WatchRescan, 5*time.Second)
	// Second overflow inside the throttle window: callback still fires,
	// but no second rescan event is emitted.
	w.signalOverflow()
	select {
	case ev := <-w.Events():
		t.Fatalf("throttled overflow emitted an event: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
	if overflows.Load() != 2 {
		t.Fatalf("OnOverflow called %d times, want 2", overflows.Load())
	}
}

func TestWatcher_RescanDelegatesToScanner(t *testing.T) {
	w, root := newTestWatcher(t, WatchOptions{RescanInterval: -1})
	writeFile(t, root, "a.txt", "content")
	res, err := w.Rescan()
	if err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Path != "a.txt" {
		t.Fatalf("rescan entries: %+v", res.Entries)
	}
}
