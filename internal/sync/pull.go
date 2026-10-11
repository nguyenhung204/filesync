package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"filesync/internal/chunk"
	"filesync/internal/ignore"
	"filesync/internal/logx"
	"filesync/internal/proto"
	"filesync/internal/safefile"
	"filesync/internal/store"
	"filesync/internal/validate"
	"filesync/internal/vclock"
)

// pullOutcome classifies how one file pull ended.
type pullOutcome int

const (
	pullDone     pullOutcome = iota // dest entry committed
	pullDeferred                    // locked: retry next round, not an error
	pullAborted                     // cancelled mid-flight: journal kept, not an error
	pullFailed                      // hard error recorded in LastErrors
)

// maxChunkAttempts is the per-chunk, per-round retry budget (Flow 2
// step 4): a chunk that fails verification is re-requested at most 3
// times in one round before the file is given up (journal kept).
const maxChunkAttempts = 3

// pullTarget describes one receive: content comes from the peer's
// entry src (its Path is the source path on the peer), and lands at
// destPath with the given final vector/author.
type pullTarget struct {
	src           store.Entry
	destPath      string
	destVector    vclock.VersionVector
	destUpdatedBy string
}

func (t pullTarget) destEntry() store.Entry {
	e := t.src
	e.Path = t.destPath
	e.Vector = t.destVector
	e.UpdatedBy = t.destUpdatedBy
	e.Tombstone = false
	e.TombstoneTs = 0
	return e
}

// executePull runs the Flow 2 receive pipeline for one file.
func (e *Engine) executePull(ctx context.Context, s *session, tgt pullTarget) pullOutcome {
	peer := s.peerID()
	dest := tgt.destEntry()

	if _, err := validate.ValidateRelPath(tgt.destPath, e.cfg.MaxPathLength); err != nil {
		e.recordError(peer, tgt.destPath, "bad_remote_path", err.Error())
		e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, State: "failed", Err: "bad_remote_path"})
		return pullFailed
	}
	e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, State: "receiving"})

	// Shortcut: the local file already holds exactly the wanted
	// content (only the vector was behind, or a previous finalize
	// crashed before its journal cleanup). No bytes move; the entry
	// is adopted with a re-read stat (D9).
	if e.tryAdoptLocal(ctx, tgt, dest) {
		e.clearPullState(peer, tgt.destPath)
		return pullDone
	}

	// Was the destination's parent directory there when we started?
	// Flow 2 step 6a uses this to tell "never existed" (WriteAtomic
	// will create it) from "deleted mid-pull" (abort, keep journal).
	destAbs := filepath.Join(e.cfg.SyncDir, filepath.FromSlash(tgt.destPath))
	parentAbs := filepath.Dir(destAbs)
	_, parentStatErr := os.Stat(parentAbs)
	parentExisted := parentStatErr == nil

	stg, err := e.prepareStaging(dest)
	if err != nil {
		e.recordError(peer, tgt.destPath, "internal", err.Error())
		e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, State: "failed", Err: "internal"})
		return pullFailed
	}
	reserved := int64(0)
	defer func() {
		stg.Close()
		e.budgetRelease(reserved)
	}()

	// Delta fill: chunks the current local file already has (same
	// position, same hash) are copied into staging locally instead
	// of being requested — verified chunk by chunk, never trusted.
	e.fillFromLocal(stg, dest)
	if err := e.persistJournal(dest, stg.bitmap); err != nil {
		e.log.Warn(ctx, "sync: persist journal after local fill failed", logx.Err(err))
	}

	needed := stg.bitmap.Missing()
	var remaining int64
	for _, ix := range needed {
		remaining += int64(dest.Chunks[ix].Size)
	}
	e.updatePullState(peer, tgt.destPath, func(ps *PullState) {
		ps.BytesDone = dest.Size - remaining
	})

	// D12 in-flight budget before the first request.
	if remaining > 0 {
		if !e.budgetReserve(remaining) {
			e.recordError(peer, tgt.destPath, "disk_full",
				fmt.Sprintf("need %d bytes, disk free does not cover in-flight pulls", remaining))
			e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, BytesDone: dest.Size - remaining, State: "failed", Err: "disk_full"})
			return pullFailed
		}
		reserved = remaining
	}
	// Point-in-time disk check as well (RequireSpace, Flow B).
	if err := safefile.RequireSpace(e.cfg.SyncDir, remaining); err != nil && remaining > 0 {
		e.recordError(peer, tgt.destPath, "disk_full", err.Error())
		e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, State: "failed", Err: "disk_full"})
		return pullFailed
	}

	out := e.requestChunks(ctx, s, tgt, dest, stg, &reserved)
	if out != pullDone {
		if out == pullDeferred || out == pullAborted {
			e.updatePullState(peer, tgt.destPath, func(ps *PullState) { ps.State = "deferred" })
		}
		return out
	}

	// Pre-finalize checks (Flow 2 step 6).
	if parentExisted {
		if _, err := os.Stat(parentAbs); err != nil {
			// Parent deleted mid-pull: keep journal + staging, do
			// NOT recreate a directory the user just deleted.
			e.log.Info(ctx, "sync: pull aborted, parent directory deleted", logx.String("path", tgt.destPath))
			e.updatePullState(peer, tgt.destPath, func(ps *PullState) { ps.State = "deferred" })
			return pullAborted
		}
	}
	if e.pathIgnored(tgt.destPath) {
		// The file became unwanted mid-flight: drop staging and
		// journal (different from disk-full — nothing to resume).
		stg.Close()
		e.discardStaging(tgt.destPath, stg.abs)
		e.clearPullState(peer, tgt.destPath)
		e.log.Info(ctx, "sync: pull cancelled, path now ignored", logx.String("path", tgt.destPath))
		return pullAborted
	}
	if !e.budgetRecheck(reserved) {
		e.recordError(peer, tgt.destPath, "disk_full", "disk space consumed by other pulls during transfer")
		e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, State: "failed", Err: "disk_full"})
		return pullFailed
	}

	return e.finalizePull(ctx, s, tgt, dest, stg)
}

// requestChunks runs the ask → verify → write loop until the staging
// bitmap is complete or the file is given up for this round.
func (e *Engine) requestChunks(ctx context.Context, s *session, tgt pullTarget, dest store.Entry, stg *staging, reserved *int64) pullOutcome {
	peer := s.peerID()
	pending := map[int]bool{}
	for _, ix := range stg.bitmap.Missing() {
		pending[ix] = true
	}
	attempts := map[int]int{}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return pullAborted
		}
		batch := make([]int, 0, len(pending))
		for ix := range pending {
			batch = append(batch, ix)
		}
		sort.Ints(batch)
		if len(batch) > proto.MaxChunkIndexes {
			batch = batch[:proto.MaxChunkIndexes]
		}
		chans := s.registerWaiters(tgt.src.Path, batch)
		err := s.conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
			Type: proto.TypeChunkRequest, Path: tgt.src.Path, Indexes: batch,
		}})
		if err != nil {
			s.unregisterWaiters(tgt.src.Path, batch, chans)
			return pullAborted // connection is dying; session ends
		}
		giveUp := false
		for _, ix := range batch {
			meta := dest.Chunks[ix]
			msg, werr := s.waitChunk(tgt.src.Path, chans[ix])
			if werr != nil {
				s.unregisterWaiters(tgt.src.Path, batch, chans)
				if errors.Is(werr, errPathFailed) {
					e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, State: "failed", Err: "stale_entry"})
					return pullFailed
				}
				return pullAborted
			}
			// Mandatory decompression path (job 004a): maxOut is
			// exactly one chunk, so a zip-bomb dies here.
			raw, derr := proto.DecompressChunkPayload(msg, chunk.ChunkSize)
			if derr != nil || msg.Header.SHA256 != meta.HexHash() || !chunk.VerifyChunk(raw, meta) {
				attempts[ix]++
				if attempts[ix] >= maxChunkAttempts {
					e.recordError(peer, tgt.destPath, "chunk_checksum",
						fmt.Sprintf("chunk %d failed verification %d times", ix, attempts[ix]))
					e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, State: "failed", Err: "chunk_checksum"})
					giveUp = true
					break
				}
				continue // stays pending; re-requested next pass
			}
			if _, err := stg.file.WriteAt(raw, meta.Offset()); err != nil {
				s.unregisterWaiters(tgt.src.Path, batch, chans)
				e.recordError(peer, tgt.destPath, "internal", fmt.Sprintf("write staging chunk %d: %v", ix, err))
				e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, State: "failed", Err: "internal"})
				return pullFailed
			}
			stg.bitmap.Set(ix)
			if err := e.persistJournal(dest, stg.bitmap); err != nil {
				e.log.Warn(ctx, "sync: persist journal failed", logx.Err(err))
			}
			e.addBytesReceived(int64(meta.Size))
			e.budgetRelease(int64(meta.Size))
			*reserved -= int64(meta.Size)
			delete(pending, ix)
			e.updatePullState(peer, tgt.destPath, func(ps *PullState) { ps.BytesDone += int64(meta.Size) })
		}
		s.unregisterWaiters(tgt.src.Path, batch, chans)
		if giveUp {
			return pullFailed
		}
	}
	return pullDone
}

// finalizePull verifies the assembled staging file end-to-end, then
// commits it with the Flow B order: archive → atomic write → upsert
// with re-read stat → clear journal → remove staging.
func (e *Engine) finalizePull(ctx context.Context, s *session, tgt pullTarget, dest store.Entry, stg *staging) pullOutcome {
	peer := s.peerID()
	stg.Close() // flush before hashing/reading

	metas, total, err := chunk.HashFile(stg.abs)
	if err != nil || total != dest.Size || !chunksEqual(metas, dest.Chunks) {
		// The assembled bytes are not what the entry promises
		// (corrupt staging, or the local copy source raced us):
		// destroy the staging — never finalize unverified data.
		e.discardStaging(tgt.destPath, stg.abs)
		e.recordError(peer, tgt.destPath, "chunk_checksum", "assembled file does not match entry chunk list")
		e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, State: "failed", Err: "chunk_checksum"})
		return pullFailed
	}

	if _, err := e.archive(tgt.destPath); err != nil {
		if safefile.IsLockedErr(err) {
			e.updatePullState(peer, tgt.destPath, func(ps *PullState) { ps.State = "deferred" })
			return pullDeferred
		}
		e.recordError(peer, tgt.destPath, "internal", fmt.Sprintf("archive before overwrite: %v", err))
		e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, State: "failed", Err: "internal"})
		return pullFailed
	}

	f, err := os.Open(stg.abs)
	if err != nil {
		e.recordError(peer, tgt.destPath, "internal", err.Error())
		return pullFailed
	}
	werr := safefile.WriteAtomicMaxLen(e.cfg.SyncDir, tgt.destPath, f, fs.FileMode(dest.Mode), e.cfg.MaxPathLength)
	f.Close()
	if werr != nil {
		if safefile.IsLockedErr(werr) {
			e.updatePullState(peer, tgt.destPath, func(ps *PullState) { ps.State = "deferred" })
			return pullDeferred
		}
		e.recordError(peer, tgt.destPath, "internal", fmt.Sprintf("atomic write: %v", werr))
		e.setPullState(peer, PullState{Path: tgt.destPath, PeerID: peer, BytesTotal: dest.Size, State: "failed", Err: "internal"})
		return pullFailed
	}

	_, info, err := safefile.RequireRegularFile(e.cfg.SyncDir, tgt.destPath, e.cfg.MaxPathLength)
	if err != nil {
		e.recordError(peer, tgt.destPath, "internal", fmt.Sprintf("stat after write: %v", err))
		return pullFailed
	}
	final := store.Entry{
		Path:      tgt.destPath,
		Size:      info.Size(),
		MtimeNs:   info.ModTime().UnixNano(),
		Mode:      uint32(info.Mode().Perm()),
		Chunks:    dest.Chunks,
		Vector:    dest.Vector,
		UpdatedBy: dest.UpdatedBy,
	}
	if err := e.st.UpsertEntry(final); err != nil {
		e.recordError(peer, tgt.destPath, "internal", fmt.Sprintf("upsert after write: %v", err))
		return pullFailed
	}
	e.discardStaging(tgt.destPath, stg.abs)
	e.clearPullState(peer, tgt.destPath)
	return pullDone
}

// tryAdoptLocal implements the no-transfer shortcut: when the local
// file's content already equals the destination entry's chunk list,
// adopt the destination entry with a re-read stat and clean any
// leftover journal/staging for the path.
func (e *Engine) tryAdoptLocal(ctx context.Context, tgt pullTarget, dest store.Entry) bool {
	local, found, err := e.st.GetEntry(tgt.destPath)
	if err != nil || !found || local.Tombstone || !chunksEqual(local.Chunks, dest.Chunks) {
		return false
	}
	abs, info, err := safefile.RequireRegularFile(e.cfg.SyncDir, tgt.destPath, e.cfg.MaxPathLength)
	if err != nil || info.Size() != dest.Size {
		return false
	}
	metas, total, err := chunk.HashFile(abs)
	if err != nil || total != dest.Size || !chunksEqual(metas, dest.Chunks) {
		return false
	}
	final := dest
	final.Size = info.Size()
	final.MtimeNs = info.ModTime().UnixNano()
	final.Mode = uint32(info.Mode().Perm())
	if err := e.st.UpsertEntry(final); err != nil {
		e.log.Warn(ctx, "sync: adopt local upsert failed", logx.Err(err))
		return false
	}
	e.discardStaging(tgt.destPath, e.stagingAbs(stagingFingerprint(dest)))
	return true
}

// fillFromLocal copies into staging every missing chunk the current
// destination file already holds at the same position, verifying
// each chunk before trusting it.
func (e *Engine) fillFromLocal(stg *staging, dest store.Entry) {
	local, found, err := e.st.GetEntry(dest.Path)
	if err != nil || !found || local.Tombstone {
		return
	}
	abs, _, err := safefile.RequireRegularFile(e.cfg.SyncDir, dest.Path, e.cfg.MaxPathLength)
	if err != nil {
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		return
	}
	defer f.Close()
	for _, ix := range stg.bitmap.Missing() {
		if ix >= len(local.Chunks) || ix >= len(dest.Chunks) {
			continue
		}
		want := dest.Chunks[ix]
		if local.Chunks[ix] != want {
			continue
		}
		buf := make([]byte, want.Size)
		if _, err := f.ReadAt(buf, want.Offset()); err != nil {
			continue
		}
		if !chunk.VerifyChunk(buf, want) {
			continue
		}
		if _, err := stg.file.WriteAt(buf, want.Offset()); err == nil {
			stg.bitmap.Set(ix)
		}
	}
}

// pathIgnored rebuilds the matcher chain for rel the way the scanner
// does (root .syncignore plus every ancestor directory's) and reports
// whether rel is ignored now (Flow 2 step 6b).
func (e *Engine) pathIgnored(rel string) bool {
	type frame struct {
		dir string // slash path of the matcher dir, "" = root
		m   *ignore.Matcher
	}
	frames := []frame{}
	if m, err := ignore.LoadFile(filepath.Join(e.cfg.SyncDir, ".syncignore")); err == nil && m != nil {
		frames = append(frames, frame{"", m})
	}
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		dirRel := strings.Join(parts[:i], "/")
		mpath := filepath.Join(e.cfg.SyncDir, filepath.FromSlash(dirRel), ".syncignore")
		if m, err := ignore.LoadFile(mpath); err == nil && m != nil {
			frames = append(frames, frame{dirRel, m})
		}
	}
	for _, fr := range frames {
		sub := rel
		if fr.dir != "" {
			if !strings.HasPrefix(rel, fr.dir+"/") {
				continue
			}
			sub = rel[len(fr.dir)+1:]
		}
		if fr.m.Match(sub, false) {
			return true
		}
	}
	return false
}
