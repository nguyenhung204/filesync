// Package daemon wires the FileSync components into the
// long-running `filesync serve` process: single-instance lock,
// startup recovery, the sync engine, watcher, transport, pairing,
// per-peer session loops, the status file, and the pair mailbox.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"filesync/internal/config"
	"filesync/internal/history"
	"filesync/internal/identity"
	"filesync/internal/index"
	"filesync/internal/logx"
	"filesync/internal/pairing"
	"filesync/internal/safefile"
	"filesync/internal/store"
	fsync "filesync/internal/sync"
	"filesync/internal/transport"
)

const (
	statusInterval  = 2 * time.Second
	mailboxInterval = 500 * time.Millisecond
	reconcileEvery  = 2 * time.Second
	connRoundIdle   = 5 * time.Second // backstop only; dirty + peer activity wake sooner
)

// peerRuntime tracks one peer's connection bookkeeping for status
// and for the D1 dedupe decision.
type peerRuntime struct {
	connected     bool
	lastRoundUnix int64
	slotConn      *transport.Conn // the conn currently owning the peer slot
	slotInbound   bool
	cancelLoop    context.CancelFunc
}

type pendingEntry struct {
	pp      pairing.PendingPair
	addedAt time.Time
}

// Daemon is one `filesync serve` process. The zero value is not
// usable; build with New.
type Daemon struct {
	cfg config.Config
	log *logx.Logger

	// Test seam: replaces transport.DefaultOptions() when set.
	// Production leaves this nil.
	transportOpts *transport.Options

	mu            sync.Mutex
	id            *identity.Identity
	st            *store.Store
	eng           *fsync.Engine
	hist          *history.Manager // the same manager the engine uses (web UI history/restore, job 006 D9e)
	uiAddr        string           // web UI listen address once bound, "" otherwise
	tm            *transport.Manager
	pm            *pairing.Manager
	state         string // "", "running", "paused", "stopped"
	startedAtUnix int64
	listenAddr    string
	indexedFiles  int
	initialScanMs int64
	pausedCached  bool
	peerRT        map[string]*peerRuntime
	pendings      map[string]pendingEntry

	ratePrevAt    time.Time
	ratePrevBytes int64
	rateBps       int64

	dirtyMu sync.Mutex
	dirtyCh chan struct{} // closed+replaced on every committed change
	scanMu  sync.Mutex    // serializes Scan+CommitScan
}

func New(cfg config.Config, log *logx.Logger) (*Daemon, error) {
	if log == nil {
		log = logx.New(os.Stderr, logx.LevelInfo)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Daemon{
		cfg:      cfg,
		log:      log,
		peerRT:   map[string]*peerRuntime{},
		pendings: map[string]pendingEntry{},
		dirtyCh:  make(chan struct{}),
	}, nil
}

func (d *Daemon) rescanInterval() time.Duration {
	if d.cfg.RescanIntervalSeconds > 0 {
		return time.Duration(d.cfg.RescanIntervalSeconds) * time.Second
	}
	return 30 * time.Second
}

func (d *Daemon) paused() bool {
	if d.st == nil {
		return false
	}
	_, found, err := d.st.JournalGet(pauseJournalKey)
	return err == nil && found
}

// broadcastDirty wakes every session loop so it starts a new round.
func (d *Daemon) broadcastDirty() {
	d.dirtyMu.Lock()
	close(d.dirtyCh)
	d.dirtyCh = make(chan struct{})
	d.dirtyMu.Unlock()
}

func (d *Daemon) dirtyChan() <-chan struct{} {
	d.dirtyMu.Lock()
	defer d.dirtyMu.Unlock()
	return d.dirtyCh
}

// scanCommit runs one full scan + commit; on change it updates the
// indexed count and wakes all session loops.
func (d *Daemon) scanCommit(ctx context.Context, scanner *index.Scanner) {
	// While the engine is actively pulling, the watcher events
	// are overwhelmingly our OWN finalize writes (already indexed
	// by the pull path itself). A full rescan per coalesce window
	// would stat the whole tree and fight the pulls for the
	// store's single connection. External edits made during a
	// bulk transfer are still caught: the rescan tick and the
	// post-pull watcher events both run a scan once pulls drain.
	if len(d.eng.ActivePulls()) > 0 {
		return
	}
	d.scanMu.Lock()
	defer d.scanMu.Unlock()
	res, err := scanner.Scan()
	if err != nil {
		d.log.Warn(ctx, "daemon: scan failed", logx.Err(err))
		return
	}
	changed, err := d.eng.CommitScan(res)
	if err != nil {
		d.log.Warn(ctx, "daemon: commit scan failed", logx.Err(err))
		return
	}
	d.refreshIndexedFiles()
	if changed {
		d.broadcastDirty()
	}
}

// refreshIndexedFiles recounts live entries for the status file.
func (d *Daemon) refreshIndexedFiles() {
	entries, err := d.st.ListEntries()
	if err != nil {
		return
	}
	live := 0
	for _, e := range entries {
		if !e.Tombstone {
			live++
		}
	}
	d.mu.Lock()
	d.indexedFiles = live
	d.mu.Unlock()
}

// Run executes Flow 4 and blocks until ctx is cancelled or a fatal
// error occurs. A second live instance for the same config dir is
// a fatal error with the D7 message.
func (d *Daemon) Run(ctx context.Context) error {
	return d.RunWithReady(ctx, nil)
}

// RunWithReady is Run plus a readiness callback: onReady, when
// non-nil, is invoked exactly once after the first status write,
// when the daemon state is "running" and the transport is serving,
// so the Windows service host can report SCM RUNNING at the true
// ready point (job 008 spec D3).
func (d *Daemon) RunWithReady(ctx context.Context, onReady func()) error {
	if err := os.MkdirAll(d.cfg.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("filesync: create config dir: %w", err)
	}
	lock, err := acquireLock(d.cfg.ConfigDir)
	if err != nil {
		return err
	}
	defer lock.Release()

	id, _, err := identity.LoadOrCreate(d.cfg.ConfigDir)
	if err != nil {
		return fmt.Errorf("filesync: load identity: %w", err)
	}
	st, err := store.Open(d.cfg.ConfigDir)
	if err != nil {
		return fmt.Errorf("filesync: open store: %w", err)
	}
	defer st.Close()
	if err := os.MkdirAll(d.cfg.SyncDir, 0o755); err != nil {
		return fmt.Errorf("filesync: create sync dir: %w", err)
	}

	d.mu.Lock()
	d.id = id
	d.st = st
	d.startedAtUnix = time.Now().UnixNano()
	d.mu.Unlock()

	// Startup recovery: temp sweep, TTL purge, staging cleanup.
	if _, err := safefile.CleanTemps(d.cfg.SyncDir); err != nil {
		d.log.Warn(ctx, "daemon: clean temp files failed", logx.Err(err))
	}
	hist := history.NewManager(d.cfg.SyncDir, st, d.cfg.VersionHistoryKeep, d.cfg.VersionHistoryQuotaBytes)
	if n, err := hist.PurgeExpired(); err != nil {
		d.log.Warn(ctx, "daemon: purge expired failed", logx.Err(err))
	} else if n > 0 {
		d.log.Info(ctx, "daemon: purged expired trash/tombstones", logx.Int("count", n))
	}
	eng := fsync.NewEngine(d.cfg, id, st, hist, d.log, fsync.Options{})
	if err := eng.CleanupStaleStaging(); err != nil {
		d.log.Warn(ctx, "daemon: clean stale staging failed", logx.Err(err))
	}
	scanner := index.NewScanner(d.cfg.SyncDir, st, index.Options{
		MaxPathLen: d.cfg.MaxPathLength, StabilityCheck: true,
	})

	// Initial scan + commit (workload metrics come from here).
	t0 := time.Now()
	res, err := scanner.Scan()
	if err != nil {
		return fmt.Errorf("filesync: initial scan: %w", err)
	}
	if _, err := eng.CommitScan(res); err != nil {
		return fmt.Errorf("filesync: initial commit: %w", err)
	}
	initialMs := time.Since(t0).Milliseconds()
	liveCount := 0
	if entries, lerr := st.ListEntries(); lerr == nil {
		for _, e := range entries {
			if !e.Tombstone {
				liveCount++
			}
		}
	}

	// Transport strictly from DefaultOptions (D10).
	topts := transport.DefaultOptions()
	if os.Getenv("FILESYNC_DISABLE_MDNS") == "1" {
		topts.MDNSEnabled = false
	}
	if d.transportOpts != nil {
		topts = *d.transportOpts
	}
	tm := transport.NewManager(id, d.cfg, st, d.log, topts)
	pm := pairing.NewManager(id, st, d.log, tm, pairing.Options{})
	pm.OnPairProposed(func(pp pairing.PendingPair) { d.addPending(pp) })
	tm.SetPairingGate(pm)

	d.mu.Lock()
	d.eng = eng
	d.hist = hist
	d.tm = tm
	d.pm = pm
	d.initialScanMs = initialMs
	d.indexedFiles = liveCount
	d.pausedCached = d.paused()
	if d.pausedCached {
		d.state = "paused"
	} else {
		d.state = "running"
	}
	d.mu.Unlock()
	d.log.Info(ctx, "daemon: started",
		logx.String("device_id", id.DeviceID),
		logx.Int("indexed_files", liveCount),
		logx.Int64("initial_scan_ms", initialMs))

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- tm.Serve(rctx, d.handleSyncConn, pm.HandleConn) }()
	// Surface the real listen address in status as soon as bound;
	// bound closes once it is set, gating readiness below.
	bound := make(chan struct{})
	go func() {
		for {
			if addr := tm.Addr(); addr != "" {
				d.mu.Lock()
				d.listenAddr = addr
				d.mu.Unlock()
				close(bound)
				return
			}
			select {
			case <-rctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	watcher, err := index.NewWatcher(scanner, index.WatchOptions{
		RescanInterval: d.rescanInterval(),
		Logger:         d.log,
	})
	if err != nil {
		return fmt.Errorf("filesync: start watcher: %w", err)
	}
	go watcher.Start(rctx)
	defer watcher.Close()

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); d.watchLoop(rctx, watcher, scanner) }()
	go func() { defer wg.Done(); d.reconcileLoop(rctx) }()
	go func() { defer wg.Done(); d.statusLoop(rctx) }()
	go func() { defer wg.Done(); d.mailboxLoop(rctx) }()
	d.startWebUI(rctx, &wg)
	// Ready gate (job 008 spec D3): onReady fires only once the
	// transport is serving; pre-bind death/shutdown skips it.
	ready := false
	select {
	case <-bound:
		ready = true
	case err := <-serveErr:
		serveErr <- err // hand the outcome to the main select below
	case <-ctx.Done():
	}
	d.writeStatusTick() // first status, now with the listen address
	if onReady != nil && ready {
		onReady()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			runErr = fmt.Errorf("filesync: transport serve: %w", err)
		}
	}
	cancel()
	_ = watcher.Close()
	wg.Wait()

	d.mu.Lock()
	d.state = "stopped"
	d.mu.Unlock()
	d.writeStatusTick()
	d.log.Info(ctx, "daemon: stopped", logx.String("device_id", id.DeviceID))
	return runErr
}

// statusLoop writes status.json every statusInterval and refreshes
// the cached pause flag + byte rate.
func (d *Daemon) statusLoop(ctx context.Context) {
	t := time.NewTicker(statusInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.writeStatusTick()
		}
	}
}

func (d *Daemon) writeStatusTick() {
	d.mu.Lock()
	p := d.paused()
	d.pausedCached = p
	if d.state == "running" && p {
		d.state = "paused"
	} else if d.state == "paused" && !p {
		d.state = "running"
	}
	if d.eng != nil {
		c := d.eng.Counters()
		d.updateRateLocked(time.Now(), c.BytesSent+c.BytesReceived)
	}
	d.mu.Unlock()
	st := d.Status()
	if err := d.writeStatusFile(st); err != nil {
		d.log.Warn(context.Background(), "daemon: write status.json failed", logx.Err(err))
	}
}

func (d *Daemon) mailboxLoop(ctx context.Context) {
	t := time.NewTicker(mailboxInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.processMailbox(ctx)
		}
	}
}

// addPending / dropPending maintain the status pending-pair list.
// Entries age out after the pairing confirm TTL (pairing offers no
// disappearance callback; expiry is the disappearance signal).
func (d *Daemon) addPending(pp pairing.PendingPair) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pendings[pp.SessionID] = pendingEntry{pp: pp, addedAt: time.Now()}
}

func (d *Daemon) dropPending(sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.pendings, sessionID)
}

func (d *Daemon) pendingListLocked() []pairing.PendingPair {
	out := []pairing.PendingPair{}
	now := time.Now()
	for sid, pe := range d.pendings {
		if now.Sub(pe.addedAt) > pairing.ConfirmTTL {
			delete(d.pendings, sid)
			continue
		}
		out = append(out, pe.pp)
	}
	return out
}
