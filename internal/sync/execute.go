package sync

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"filesync/internal/logx"
	"filesync/internal/safefile"
	"filesync/internal/store"
	"filesync/internal/vclock"
)

// executeActions runs one round's plan. Store-only actions apply
// inline; pulls and conflict resolutions run concurrently, bounded
// by the engine's parallelism semaphore. A failure of one file never
// fails the others.
func (e *Engine) executeActions(ctx context.Context, s *session, actions []Action) {
	sem := make(chan struct{}, e.parallelism)
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn()
		}()
	}
	// Renames run first, inline: a rename's companion
	// AdoptTombstone for the old path (emitted by the planner in the
	// same plan) must land AFTER the rename has moved the file and
	// cleared the old entry, or it would be deleted again by the
	// rename's own bookkeeping.
	var rest []Action
	var renameFallbacks []Action
	for _, a := range actions {
		if a.Kind != ActionApplyRename {
			rest = append(rest, a)
			continue
		}
		if !e.applyRename(s, a) {
			renameFallbacks = append(renameFallbacks, a)
		}
	}
	for _, a := range renameFallbacks {
		a := a
		run(func() {
			e.executePull(ctx, s, pullTarget{
				src: a.Remote, destPath: a.Path,
				destVector: a.Remote.Vector, destUpdatedBy: a.Remote.UpdatedBy,
			})
		})
	}
	for _, a := range rest {
		a := a
		switch a.Kind {
		case ActionAdoptTombstone:
			// Record the delete even though we never had the file
			// (D8: this is what stops a third node resurrecting it).
			if err := e.st.UpsertEntry(a.Remote); err != nil {
				e.recordError(s.peerID(), a.Path, "internal", err.Error())
			}
		case ActionMergeVector:
			ne := a.Local
			ne.Vector = a.MergedVector
			ne.Tombstone = false
			ne.TombstoneTs = 0
			if err := e.st.UpsertEntry(ne); err != nil {
				e.recordError(s.peerID(), a.Path, "internal", err.Error())
			}
		case ActionMergeTombstone:
			ne := a.Local
			ne.Vector = a.MergedVector
			if a.Remote.TombstoneTs > ne.TombstoneTs {
				ne.TombstoneTs = a.Remote.TombstoneTs
			}
			if err := e.st.UpsertEntry(ne); err != nil {
				e.recordError(s.peerID(), a.Path, "internal", err.Error())
			}
		case ActionApplyDelete:
			e.applyDelete(s, a)
		case ActionPull:
			run(func() {
				vec := a.Remote.Vector
				if a.MergedVector != nil {
					vec = a.MergedVector
				}
				e.executePull(ctx, s, pullTarget{
					src: a.Remote, destPath: a.Path,
					destVector: vec, destUpdatedBy: a.Remote.UpdatedBy,
				})
			})
		case ActionResolveConflict:
			run(func() { e.executeResolveConflict(ctx, s, a) })
		}
	}
	wg.Wait()
}

// applyDelete applies a peer's winning tombstone: the file goes to
// trash via history (which upserts the tombstone with the peer's
// vector), or — when the file is already gone — the tombstone is
// adopted directly (spec risk 8).
func (e *Engine) applyDelete(s *session, a Action) {
	abs := filepath.Join(e.cfg.SyncDir, filepath.FromSlash(a.Path))
	if _, err := os.Lstat(abs); os.IsNotExist(err) {
		if err := e.st.UpsertEntry(a.Remote); err != nil {
			e.recordError(s.peerID(), a.Path, "internal", err.Error())
		}
		return
	}
	if _, err := e.hist.MoveToTrash(a.Path, a.Remote.Vector, a.Remote.UpdatedBy); err != nil {
		if safefile.IsLockedErr(err) {
			e.log.Info(s.ctx, "sync: delete deferred, file locked", logx.String("path", a.Path))
			return
		}
		e.recordError(s.peerID(), a.Path, "internal", err.Error())
	}
}

// applyRename performs the local rename for a detected remote
// rename: same bytes, new path, store updated with a re-read stat
// (D9). Returns false when the rename cannot be done and the caller
// should fall back to pulling.
func (e *Engine) applyRename(s *session, a Action) bool {
	srcAbs, _, err := safefile.RequireRegularFile(e.cfg.SyncDir, a.FromPath, e.cfg.MaxPathLength)
	if err != nil {
		return false
	}
	dstAbs, err := safefile.Resolve(e.cfg.SyncDir, a.Path, e.cfg.MaxPathLength)
	if err != nil {
		return false
	}
	if _, err := os.Lstat(dstAbs); err == nil {
		// A file sits at the destination although no live entry
		// owns it: preserve it in version history before replacing.
		if _, err := e.archive(a.Path); err != nil {
			if safefile.IsLockedErr(err) {
				e.log.Info(s.ctx, "sync: rename deferred, destination locked", logx.String("path", a.Path))
				return true // handled: deferred, do not fall back to pull
			}
			return false
		}
	}
	if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
		return false
	}
	if err := os.Rename(srcAbs, dstAbs); err != nil {
		if safefile.IsLockedErr(err) {
			e.log.Info(s.ctx, "sync: rename deferred, file locked", logx.String("path", a.Path))
			return true
		}
		return false
	}
	info, err := os.Stat(dstAbs)
	if err != nil {
		return false
	}
	ne := a.Remote
	ne.Size = info.Size()
	ne.MtimeNs = info.ModTime().UnixNano()
	ne.Mode = uint32(info.Mode().Perm())
	if err := e.st.DeleteEntry(a.FromPath); err != nil {
		// The destination entry is already committed; a stale
		// source entry self-heals at the next scan (the file is
		// gone from disk), but the store error must not pass
		// silently.
		e.recordError(s.peerID(), a.FromPath, "internal", err.Error())
	}
	if err := e.st.UpsertEntry(ne); err != nil {
		e.recordError(s.peerID(), a.Path, "internal", err.Error())
	}
	return true
}

// executeResolveConflict performs the D3 resolution for one pair.
// Both peers compute the same winner, loser and conflict path from
// the exchanged entries, so each side ends with: winner content at
// the original path, loser content at the conflict path, and the
// merged vector on both entries.
func (e *Engine) executeResolveConflict(ctx context.Context, s *session, a Action) {
	l, r := a.Local, a.Remote
	merged := a.MergedVector
	if merged == nil {
		merged = l.Vector.Merge(r.Vector)
	}
	loser := r
	if a.WinnerRemote {
		loser = l
	}

	// Find the conflict path to use: the base name, or accept an
	// existing copy that already holds the loser's content, or step
	// to the -2/-3 suffix when a different content got there first.
	conflictPath := a.ConflictPath
	preserved := false
	for n := 0; ; n++ {
		cand := conflictPath
		if n > 0 {
			cand = conflictPathSuffix(loser, n+1)
		}
		ex, found, err := e.st.GetEntry(cand)
		if err == nil && found && !ex.Tombstone {
			if chunksEqual(ex.Chunks, loser.Chunks) {
				conflictPath, preserved = cand, true
			} else {
				continue // occupied by different content; try next suffix
			}
		} else {
			conflictPath = cand
		}
		break
	}

	if !preserved {
		if a.WinnerRemote {
			// We lose: the loser's content is our current file —
			// copy it to the conflict path before the winner's
			// pull overwrites it. If the file no longer matches
			// the entry (user is editing it right now), defer the
			// whole conflict to the next round rather than
			// snapshot racing bytes.
			if !e.copyLocalToConflict(l, conflictPath, merged) {
				return
			}
		} else {
			// We win: the loser's content lives on the peer (at
			// its original path, not yet overwritten there) —
			// pull it into the conflict path like any other file.
			if out := e.executePull(ctx, s, pullTarget{
				src: r, destPath: conflictPath,
				destVector: merged, destUpdatedBy: r.UpdatedBy,
			}); out != pullDone {
				return // retry wholesale next round
			}
		}
	} else {
		// Content already preserved; make sure its entry carries
		// the merged vector so the next round is a NoOp.
		if ex, found, err := e.st.GetEntry(conflictPath); err == nil && found &&
			ex.Vector.Compare(merged) != vclock.Equal {
			ex.Vector = merged
			_ = e.st.UpsertEntry(ex)
		}
	}

	if a.WinnerRemote {
		e.executePull(ctx, s, pullTarget{
			src: r, destPath: a.Path,
			destVector: merged, destUpdatedBy: r.UpdatedBy,
		})
	} else {
		ne := l
		ne.Vector = merged
		if err := e.st.UpsertEntry(ne); err != nil {
			e.recordError(s.peerID(), a.Path, "internal", err.Error())
		}
	}
}

// copyLocalToConflict snapshots our losing file into the conflict
// path and upserts its entry. False means "defer to next round".
func (e *Engine) copyLocalToConflict(l store.Entry, conflictPath string, merged vclock.VersionVector) bool {
	abs, info, err := safefile.RequireRegularFile(e.cfg.SyncDir, l.Path, e.cfg.MaxPathLength)
	if err != nil || info.Size() != l.Size || info.ModTime().UnixNano() != l.MtimeNs {
		return false
	}
	f, err := os.Open(abs)
	if err != nil {
		return false
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil || !os.SameFile(info, opened) {
		return false
	}
	if err := safefile.WriteAtomicMaxLen(e.cfg.SyncDir, conflictPath, f, info.Mode(), e.cfg.MaxPathLength); err != nil {
		return false
	}
	_, winfo, err := safefile.RequireRegularFile(e.cfg.SyncDir, conflictPath, e.cfg.MaxPathLength)
	if err != nil {
		return false
	}
	ne := store.Entry{
		Path:      conflictPath,
		Size:      winfo.Size(),
		MtimeNs:   winfo.ModTime().UnixNano(),
		Mode:      uint32(winfo.Mode().Perm()),
		Chunks:    l.Chunks,
		Vector:    merged,
		UpdatedBy: l.UpdatedBy,
	}
	return e.st.UpsertEntry(ne) == nil
}
