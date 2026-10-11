package index

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"filesync/internal/logx"
)

// WatchKind classifies a watcher notification.
type WatchKind int

const (
	// WatchChanged: one path changed (debounced); Path is set.
	WatchChanged WatchKind = iota
	// WatchRescan: too much changed (or the periodic safety net fired);
	// the caller should run a full Scan instead of chasing events.
	WatchRescan
)

func (k WatchKind) String() string {
	switch k {
	case WatchChanged:
		return "changed"
	case WatchRescan:
		return "rescan"
	}
	return "unknown"
}

// WatchEvent is one notification on the Watcher's channel.
type WatchEvent struct {
	Kind WatchKind
	Path string // slash path relative to root; only for WatchChanged
}

// WatchOptions configures a Watcher.
type WatchOptions struct {
	// Debounce is the quiet period after which pending changed paths are
	// flushed. 0 -> 200ms.
	Debounce time.Duration
	// RescanInterval both throttles overflow-driven rescans (at most one
	// per interval, Flow E) and drives the periodic safety-net rescan
	// that catches dropped events. 0 -> 30s; negative disables the
	// periodic ticker (overflow throttle still applies with 30s).
	RescanInterval time.Duration
	// OnOverflow, if set, is called when an overflow is detected and a
	// rescan is emitted.
	OnOverflow func()
	// Logger receives watcher-internal events (overflow, fsnotify
	// errors) that cannot be returned as values. Nil -> no logging.
	Logger *logx.Logger
	// QueueSize is the buffer of the events channel. 0 -> 1024.
	QueueSize int
}

// Watcher watches a root recursively with fsnotify. Construct with
// NewWatcher, run with Start, stop with Close.
type Watcher struct {
	scanner *Scanner
	root    string
	opts    WatchOptions
	fsw     *fsnotify.Watcher
	events  chan WatchEvent

	mu           sync.Mutex
	pending      map[string]struct{}
	flushTimer   *time.Timer
	lastRescan   time.Time
	rescanQueued bool
	closed       bool
	done         chan struct{}
}

// NewWatcher creates a Watcher for scanner's root and registers the whole
// existing tree with fsnotify (directories created later are added as
// their create events arrive).
func NewWatcher(scanner *Scanner, opts WatchOptions) (*Watcher, error) {
	if scanner == nil {
		return nil, fmt.Errorf("index: new watcher: nil scanner")
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 200 * time.Millisecond
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1024
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("index: new watcher: %w", err)
	}
	w := &Watcher{
		scanner: scanner,
		root:    scanner.root,
		opts:    opts,
		fsw:     fsw,
		events:  make(chan WatchEvent, opts.QueueSize),
		pending: map[string]struct{}{},
		done:    make(chan struct{}),
	}
	if err := w.addRecursive(scanner.root); err != nil {
		fsw.Close()
		return nil, err
	}
	return w, nil
}

// Events returns the notification channel.
func (w *Watcher) Events() <-chan WatchEvent { return w.events }

// Rescan runs a full scan — the caller's response to WatchRescan.
func (w *Watcher) Rescan() (ScanResult, error) { return w.scanner.Scan() }

// metaName is FileSync's metadata directory inside the root. The
// watcher never registers or reports anything inside a .filesync tree,
// at any depth: version history and trash churn is FileSync's own
// bookkeeping, not user content (reporting it would make the daemon
// react to its own writes).
const metaName = ".filesync"

// inMetaTree reports whether a slash-separated root-relative path is
// the metadata tree or lives anywhere underneath one.
func inMetaTree(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if part == metaName {
			return true
		}
	}
	return false
}

func (w *Watcher) addRecursive(root string) error {
	// Refuse to register anything for a tree that is (or sits inside)
	// the metadata tree — this also covers a .filesync directory that
	// appears only after the watcher started and arrives here through
	// its Create event.
	if rel, err := filepath.Rel(w.root, root); err == nil && rel != "." && inMetaTree(filepath.ToSlash(rel)) {
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == metaName && p != root {
				return filepath.SkipDir
			}
			if err := w.fsw.Add(p); err != nil {
				return fmt.Errorf("index: watcher add %q: %w", p, err)
			}
		}
		return nil
	})
}

// Start runs the event loop until ctx is cancelled or Close is called.
func (w *Watcher) Start(ctx context.Context) {
	var ticker *time.Ticker
	var tickC <-chan time.Time
	interval := w.opts.RescanInterval
	if interval == 0 {
		interval = 30 * time.Second
	}
	if w.opts.RescanInterval >= 0 {
		ticker = time.NewTicker(interval)
		tickC = ticker.C
		defer ticker.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.done:
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleFsEvent(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.logWarn("fsnotify error", err)
			w.signalOverflow()
		case <-tickC:
			// Periodic safety net: dropped events are healed by a
			// full rescan, exactly the >50k-files fallback of Flow E.
			w.emitRescan(false)
		}
	}
}

// Close stops the watcher and closes the events channel.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	close(w.done)
	if w.flushTimer != nil {
		w.flushTimer.Stop()
	}
	w.mu.Unlock()
	err := w.fsw.Close()
	close(w.events)
	if err != nil {
		return fmt.Errorf("index: watcher close: %w", err)
	}
	return nil
}

func (w *Watcher) handleFsEvent(ev fsnotify.Event) {
	// Only content/structure changes matter for sync.
	if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
		return
	}
	rel, err := filepath.Rel(w.root, ev.Name)
	if err != nil || rel == "." {
		return
	}
	relSlash := filepath.ToSlash(rel)
	// The metadata tree is filtered HERE as well as at registration
	// time, including when it appears after the watcher started: its
	// events are neither registered nor reported (addRecursive applies
	// the same filter as a second line of defense).
	if inMetaTree(relSlash) {
		return
	}
	if ev.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			// New directory: watch it and everything already inside.
			_ = w.addRecursive(ev.Name)
		}
	}
	w.queueChanged(relSlash)
}

// queueChanged records a changed path and (re)schedules the debounce
// flush. If too many distinct paths pile up, the watcher gives up on
// per-file events and escalates to a rescan — chasing tens of thousands
// of individual events is exactly the overflow case of Flow E.
func (w *Watcher) queueChanged(rel string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if len(w.pending) >= w.opts.QueueSize {
		w.pending = map[string]struct{}{}
		w.mu.Unlock()
		w.signalOverflow()
		w.mu.Lock()
		return
	}
	w.pending[rel] = struct{}{}
	if w.flushTimer != nil {
		w.flushTimer.Stop()
	}
	w.flushTimer = time.AfterFunc(w.opts.Debounce, w.flushPending)
}

func (w *Watcher) flushPending() {
	w.mu.Lock()
	paths := make([]string, 0, len(w.pending))
	for p := range w.pending {
		paths = append(paths, p)
	}
	w.pending = map[string]struct{}{}
	w.mu.Unlock()
	for _, p := range paths {
		w.emit(WatchEvent{Kind: WatchChanged, Path: p})
	}
}

// signalOverflow handles an overflow: it invokes OnOverflow and emits a
// WatchRescan, throttled so that within one RescanInterval at most one
// overflow-driven rescan is emitted (Flow E).
func (w *Watcher) signalOverflow() {
	if w.opts.OnOverflow != nil {
		w.opts.OnOverflow()
	}
	w.emitRescan(true)
}

// emitRescan emits a WatchRescan. The throttle applies ONLY to
// overflow-driven rescans (throttled == true): at most one per
// RescanInterval, further overflows just set rescanQueued. Periodic
// safety-net rescans (throttled == false) are NEVER throttled — they
// are the fallback that heals dropped events — but they still update
// lastRescan, the timestamp the overflow throttle measures against.
func (w *Watcher) emitRescan(throttled bool) {
	w.mu.Lock()
	interval := w.opts.RescanInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	now := time.Now()
	if throttled && !w.lastRescan.IsZero() && now.Sub(w.lastRescan) < interval {
		w.rescanQueued = true
		w.mu.Unlock()
		return
	}
	w.lastRescan = now
	w.rescanQueued = false
	w.mu.Unlock()
	w.emit(WatchEvent{Kind: WatchRescan})
}

// emit delivers an event without blocking the watcher loop: if consumers
// are too slow and the queue is full, changed events are dropped (they
// are subsumed by the next rescan) and a rescan is queued instead.
func (w *Watcher) emit(ev WatchEvent) {
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if closed {
		return
	}
	defer func() { _ = recover() }() // channel may be closed concurrently by Close
	select {
	case w.events <- ev:
	default:
		if ev.Kind == WatchChanged {
			go w.signalOverflow()
		}
	}
}

func (w *Watcher) logWarn(msg string, err error) {
	if w.opts.Logger == nil {
		return
	}
	w.opts.Logger.Warn(context.Background(), msg, logx.Err(err))
}
