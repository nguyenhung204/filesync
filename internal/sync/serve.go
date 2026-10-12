package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"filesync/internal/chunk"
	"filesync/internal/logx"
	"filesync/internal/proto"
	"filesync/internal/safefile"
	"filesync/internal/store"
)

// sendIndex answers an IndexRequest from the session's round-start
// snapshot, in batches of at most proto.MaxIndexBatch entries.
// The snapshot (not the live store) is authoritative for the whole
// round: both peers must plan from generation-consistent indexes,
// or symmetric operations like conflict resolution diverge.
func (e *Engine) sendIndex(s *session, ctx context.Context) {
	entries := s.snapshotList()
	const payloadBudget = 3 << 20 // headroom under proto.MaxIndexPayloadBytes
	var batch []store.Entry
	batchBytes := 0
	flush := func(more bool) bool {
		if len(batch) == 0 && more {
			return true
		}
		msg, err := proto.IndexMessage(batch, more)
		if err != nil {
			e.log.Error(ctx, "sync: build index batch", logx.Err(err))
			return false
		}
		if err := s.conn.WriteMessage(ctx, msg); err != nil {
			return false
		}
		batch = nil
		batchBytes = 0
		return true
	}
	for _, en := range entries {
		raw, err := json.Marshal(en)
		if err != nil {
			continue
		}
		if len(batch) > 0 && (len(batch) >= proto.MaxIndexBatch || batchBytes+len(raw) > payloadBudget) {
			if !flush(true) {
				return
			}
		}
		batch = append(batch, en)
		batchBytes += len(raw)
	}
	flush(false)
}

// serveChunkRequest answers one ChunkRequest. The requested entry
// comes from the round snapshot (that is what the peer planned
// against); chunk bytes are resolved by content hash: the file at
// the entry's path is tried first, and if our own round execution
// has already moved or overwritten it (conflict resolution,
// rename, trash), any other local file whose entry contains the
// same chunk hash serves instead. Chunks are content-addressed, so
// a hash-verified read from any path is byte-identical by
// construction.
func (e *Engine) serveChunkRequest(s *session, ctx context.Context, m proto.Message) {
	req := m.Header
	fail := func(code, format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		_ = s.conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
			Type: proto.TypeError, Code: code, Message: msg,
			Path: req.Path, AckType: proto.TypeChunkRequest,
		}})
	}
	src, ok := s.snapshotEntry(req.Path)
	if !ok {
		// Spec Flow 1 step 4: a request against a generation the
		// peer planned from but we no longer serve is a stale
		// entry — bad_message, not an internal failure.
		fail(proto.CodeBadMessage, "stale entry: %s not in round index", req.Path)
		return
	}
	for _, ix := range req.Indexes {
		if ix < 0 || ix >= len(src.Chunks) {
			fail(proto.CodeBadMessage, "stale entry: chunk index %d out of range for %s", ix, req.Path)
			return
		}
		meta := src.Chunks[ix]
		data, err := e.readChunkByHash(src, meta)
		if err != nil {
			fail(proto.CodeInternal, "chunk unavailable: %s#%d", req.Path, ix)
			return
		}
		payload, enc := proto.CompressChunkPayload(data)
		out := proto.Message{Header: proto.Header{
			Type: proto.TypeChunkData, Path: req.Path, Index: ix,
			Size: meta.Size, SHA256: meta.HexHash(), Encoding: enc,
		}, Payload: payload}
		if err := s.conn.WriteMessage(ctx, out); err != nil {
			return
		}
		e.addBytesSent(int64(meta.Size))
		// Display-only send progress (job 009 D3): the serving
		// side otherwise has no per-file progress surface.
		e.noteSendChunk(s.peerID(), req.Path, src.Size, int64(meta.Size))
	}
}

// readChunkByHash returns the bytes of one chunk, verified against
// its SHA-256, from the entry's own file if possible, else from
// any other local entry containing the same chunk.
func (e *Engine) readChunkByHash(src store.Entry, meta chunk.ChunkMeta) ([]byte, error) {
	if data, err := e.readChunkAt(src.Path, meta); err == nil {
		return data, nil
	}
	entries, err := e.st.ListEntries()
	if err != nil {
		return nil, err
	}
	for _, other := range entries {
		if other.Tombstone || other.Path == src.Path {
			continue
		}
		for _, cm := range other.Chunks {
			if cm.SHA256 == meta.SHA256 && cm.Size == meta.Size {
				if data, err := e.readChunkAt(other.Path, cm); err == nil {
					return data, nil
				}
				break
			}
		}
	}
	return nil, fmt.Errorf("sync: chunk %s not found locally", meta.HexHash())
}

func (e *Engine) readChunkAt(path string, meta chunk.ChunkMeta) ([]byte, error) {
	abs, info, err := safefile.RequireRegularFile(e.cfg.SyncDir, path, e.cfg.MaxPathLength)
	if err != nil {
		return nil, err
	}
	if info.Size() < meta.Offset()+int64(meta.Size) {
		return nil, fmt.Errorf("sync: file %s too small for chunk %d", path, meta.Index)
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("sync: open %s: %w", path, err)
	}
	defer f.Close()
	buf := make([]byte, meta.Size)
	if _, err := f.ReadAt(buf, meta.Offset()); err != nil {
		return nil, fmt.Errorf("sync: read %s chunk %d: %w", path, meta.Index, err)
	}
	if !chunk.VerifyChunk(buf, meta) {
		return nil, fmt.Errorf("sync: chunk %d of %s does not match its hash", meta.Index, path)
	}
	return buf, nil
}
