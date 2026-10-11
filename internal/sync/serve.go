package sync

import (
	"context"
	"os"

	"filesync/internal/chunk"
	"filesync/internal/proto"
	"filesync/internal/safefile"
)

// serveChunkRequest answers one chunk_request from the store entry
// and the file on disk. The file must still match the entry we
// advertised (size + mtime), and every chunk is re-verified before
// sending — a file being edited under us yields a clean
// Error{bad_message, "stale entry"} for that file only, never bad
// bytes (session spec step 4).
func (e *Engine) serveChunkRequest(ctx context.Context, s *session, req proto.Message) {
	path := req.Header.Path
	stale := func() { s.sendError(proto.CodeBadMessage, "stale entry", path) }

	entry, found, err := e.st.GetEntry(path)
	if err != nil || !found || entry.Tombstone {
		stale()
		return
	}
	abs, info, err := safefile.RequireRegularFile(e.cfg.SyncDir, path, e.cfg.MaxPathLength)
	if err != nil || info.Size() != entry.Size || info.ModTime().UnixNano() != entry.MtimeNs {
		stale()
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		stale()
		return
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil || !os.SameFile(info, opened) {
		stale()
		return
	}
	for _, ix := range req.Header.Indexes {
		if ix < 0 || ix >= len(entry.Chunks) {
			s.sendError(proto.CodeBadMessage, "chunk index out of range", path)
			return
		}
		meta := entry.Chunks[ix]
		buf := make([]byte, meta.Size)
		if _, err := f.ReadAt(buf, meta.Offset()); err != nil {
			s.sendError(proto.CodeInternal, "read chunk failed", path)
			return
		}
		if !chunk.VerifyChunk(buf, meta) {
			stale()
			return
		}
		// Compression is mandatory on this path (job 004a): chunk
		// payloads always go through the proto helper, which emits
		// gzip only when it actually shrinks the chunk.
		payload, enc := proto.CompressChunkPayload(buf)
		msg := proto.Message{Header: proto.Header{
			Type:     proto.TypeChunkData,
			Path:     path,
			Index:    ix,
			Size:     meta.Size,
			SHA256:   meta.HexHash(),
			Encoding: enc,
		}, Payload: payload}
		if err := s.conn.WriteMessage(ctx, msg); err != nil {
			return // connection is dying; the session will end
		}
		e.addBytesSent(int64(meta.Size))
	}
}
