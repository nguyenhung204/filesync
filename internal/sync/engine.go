// Package sync is FileSync's two-way synchronization engine. It turns
// scanner output into versioned store entries (CommitScan), decides
// per-path actions from version vectors alone (Plan, ADR-003), and runs
// sync rounds over one trusted connection per peer (RunSession): both
// sides exchange full indexes, plan locally, and PULL what they miss —
// there is no push, a pull is how the other side's push looks.
//
// Data-loss discipline (project rule 1): every filesystem change the
// engine makes goes through the store-last order of Flow B (archive →
// atomic write → upsert with re-read stat), deletes go through history
// (trash + tombstone), and conflicts always keep both copies.
package sync

import (
	"sync"
	"time"

	"filesync/internal/config"
	"filesync/internal/history"
	"filesync/internal/identity"
	"filesync/internal/logx"
	"filesync/internal/safefile"
	"filesync/internal/store"
)

// Options tunes an Engine. Zero values get defaults from the config.
type Options struct {
	Parallelism int              // 0 -> cfg.Parallelism: max files pulled concurrently in one round
	Now         func() time.Time // nil -> time.Now; display/naming timestamps only, never ordering
	Logger      *logx.Logger
}

// Counters is the engine's byte/round accounting since engine start.
// Bytes count RAW chunk bytes (header Size of chunk_data), never the
// possibly-compressed wire payload (spec Flow 2 step 8).
type Counters struct {
	BytesSent, BytesReceived int64
	Rounds                   int64
	LastRoundUnixNano        int64
}

// PullState is one file's pull progress for daemon status reporting.
type PullState struct {
	Path, PeerID string
	BytesTotal   int64
	BytesDone    int64
	State        string // "queued" | "receiving" | "deferred" | "failed"
	Err          string // stable code when State == "failed" (disk_full, chunk_checksum, ...)
}

// SyncError is one recorded sync failure (bounded history, newest last).
type SyncError struct {
	AtUnixNano         int64
	Path, PeerID, Code string
	Message            string
}

const maxRecentErrors = 10

// Engine is one node's sync brain. It is safe for concurrent use;
// the daemon additionally guarantees one session per peer at a time.
type Engine struct {
	cfg  config.Config
	id   *identity.Identity
	st   *store.Store
	hist *history.Manager
	log  *logx.Logger
	now  func() time.Time

	parallelism int

	mu       sync.Mutex
	counters Counters
	pulls    map[string]*PullState
	errs     []SyncError

	budgetMu sync.Mutex
	inflight int64 // bytes still owed to in-flight pulls, all peers (D12)

	// Test seams (same-package tests only); nil means the real thing.
	freeSpaceFn func(path string) (int64, error)
	archiveFn   func(rel string) (string, error)
}

// NewEngine builds an Engine from the node's config, identity, store
// and history manager.
func NewEngine(cfg config.Config, id *identity.Identity, st *store.Store, hist *history.Manager, log *logx.Logger, opts Options) *Engine {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	par := opts.Parallelism
	if par <= 0 {
		par = cfg.Parallelism
	}
	if par <= 0 {
		par = 1
	}
	if log == nil {
		log = logx.New(discardWriter{}, logx.LevelError)
	}
	return &Engine{
		cfg:         cfg,
		id:          id,
		st:          st,
		hist:        hist,
		log:         log,
		now:         now,
		parallelism: par,
		pulls:       make(map[string]*PullState),
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// SelfID returns this node's device ID.
func (e *Engine) SelfID() string { return e.id.DeviceID }

// Root returns the sync root directory.
func (e *Engine) Root() string { return e.cfg.SyncDir }

// Counters returns a snapshot of the engine counters.
func (e *Engine) Counters() Counters {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.counters
}

// ActivePulls returns the current pull states, sorted by path, for
// status reporting. Finished pulls are removed; deferred and failed
// pulls stay visible until a later round replaces them.
func (e *Engine) ActivePulls() []PullState {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]PullState, 0, len(e.pulls))
	for _, p := range e.pulls {
		out = append(out, *p)
	}
	sortPullStates(out)
	return out
}

// LastErrors returns up to the 10 most recent sync errors, newest last.
func (e *Engine) LastErrors() []SyncError {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]SyncError, len(e.errs))
	copy(out, e.errs)
	return out
}

func (e *Engine) recordError(peerID, path, code, message string) {
	e.mu.Lock()
	e.errs = append(e.errs, SyncError{
		AtUnixNano: e.now().UnixNano(),
		Path:       path, PeerID: peerID, Code: code, Message: message,
	})
	if len(e.errs) > maxRecentErrors {
		e.errs = append([]SyncError(nil), e.errs[len(e.errs)-maxRecentErrors:]...)
	}
	e.mu.Unlock()
	e.log.Warn(nil, "sync: file error",
		logx.String("peer", peerID), logx.String("path", path),
		logx.String("code", code), logx.String("message", message))
}

func (e *Engine) addBytesSent(n int64) {
	e.mu.Lock()
	e.counters.BytesSent += n
	e.mu.Unlock()
}

func (e *Engine) addBytesReceived(n int64) {
	e.mu.Lock()
	e.counters.BytesReceived += n
	e.mu.Unlock()
}

func (e *Engine) noteRoundDone() {
	e.mu.Lock()
	e.counters.Rounds++
	e.counters.LastRoundUnixNano = e.now().UnixNano()
	e.mu.Unlock()
}

func pullKey(peerID, path string) string { return peerID + "\x00" + path }

func (e *Engine) setPullState(peerID string, ps PullState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := ps
	e.pulls[pullKey(peerID, ps.Path)] = &cp
}

func (e *Engine) updatePullState(peerID, path string, fn func(*PullState)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ps, ok := e.pulls[pullKey(peerID, path)]; ok {
		fn(ps)
	}
}

func (e *Engine) clearPullState(peerID, path string) {
	e.mu.Lock()
	delete(e.pulls, pullKey(peerID, path))
	e.mu.Unlock()
}

// --- in-flight byte budget (D12) ---

func (e *Engine) freeSpace(path string) (int64, error) {
	if e.freeSpaceFn != nil {
		return e.freeSpaceFn(path)
	}
	return safefile.FreeSpace(path)
}

// budgetReserve charges n bytes (one pull's remaining need) against
// the shared budget. It succeeds only when the filesystem can cover
// n plus everything already in flight: FreeSpace >= n + inflight.
func (e *Engine) budgetReserve(n int64) bool {
	if n <= 0 {
		return true
	}
	e.budgetMu.Lock()
	defer e.budgetMu.Unlock()
	free, err := e.freeSpace(e.cfg.SyncDir)
	if err != nil {
		return false
	}
	if free < n+e.inflight {
		return false
	}
	e.inflight += n
	return true
}

// budgetRelease returns n bytes to the budget (chunk received, pull
// finished or aborted).
func (e *Engine) budgetRelease(n int64) {
	if n <= 0 {
		return
	}
	e.budgetMu.Lock()
	e.inflight -= n
	if e.inflight < 0 {
		e.inflight = 0
	}
	e.budgetMu.Unlock()
}

// budgetRecheck verifies, just before finalize, that the disk still
// covers the OTHER pulls in flight (ownRemaining is this pull's own
// outstanding reservation, already on disk as staging).
func (e *Engine) budgetRecheck(ownRemaining int64) bool {
	e.budgetMu.Lock()
	others := e.inflight - ownRemaining
	e.budgetMu.Unlock()
	if others <= 0 {
		return true
	}
	free, err := e.freeSpace(e.cfg.SyncDir)
	if err != nil {
		return false
	}
	return free >= others
}

func (e *Engine) archive(rel string) (string, error) {
	if e.archiveFn != nil {
		return e.archiveFn(rel)
	}
	return e.hist.Archive(rel)
}

func sortPullStates(ps []PullState) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].Path < ps[j-1].Path; j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}
