package daemon

import (
	"context"
	"time"

	"filesync/internal/index"
	"filesync/internal/logx"
)

// watchLoop turns watcher events into scan+commit cycles; the
// watcher's own 30s ticker covers dropped events (WatchRescan).
func (d *Daemon) watchLoop(ctx context.Context, watcher *index.Watcher, scanner *index.Scanner) {
	var debounce *time.Timer
	var debounceC <-chan time.Time
	schedule := func() {
		if debounce == nil {
			debounce = time.NewTimer(300 * time.Millisecond)
			debounceC = debounce.C
			return
		}
		if !debounce.Stop() {
			select {
			case <-debounce.C:
			default:
			}
		}
		debounce.Reset(300 * time.Millisecond)
		debounceC = debounce.C
	}
	for {
		select {
		case <-ctx.Done():
			if debounce != nil {
				debounce.Stop()
			}
			return
		case ev, ok := <-watcher.Events():
			if !ok {
				return
			}
			if ev.Kind == index.WatchRescan {
				// The watcher already ran the full scan for a
				// rescan event? No — Rescan() is the caller's job.
				if res, err := watcher.Rescan(); err == nil {
					d.commitResult(ctx, res)
				}
			} else {
				schedule()
			}
		case <-debounceC:
			debounceC = nil
			d.scanCommit(ctx, scanner)
		}
	}
}

func (d *Daemon) commitResult(ctx context.Context, res index.ScanResult) {
	d.scanMu.Lock()
	defer d.scanMu.Unlock()
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
