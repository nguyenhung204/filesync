package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"filesync/internal/chunk"
	"filesync/internal/vclock"
)

// encodeEntry serializes the chunks (BLOB holding JSON, per the job 002
// decision: debuggable by eye, no second binary format to invent) and the
// version vector (canonical vclock JSON) for storage.
func encodeEntry(e Entry) (chunksBlob []byte, vectorText string, err error) {
	chunks := e.Chunks
	if chunks == nil {
		chunks = []chunk.ChunkMeta{}
	}
	chunksBlob, err = json.Marshal(chunks)
	if err != nil {
		return nil, "", fmt.Errorf("encode chunks for %q: %w", e.Path, err)
	}
	vec := e.Vector
	if vec == nil {
		vec = vclock.New()
	}
	vb, err := vec.MarshalJSON()
	if err != nil {
		return nil, "", fmt.Errorf("encode vector for %q: %w", e.Path, err)
	}
	return chunksBlob, string(vb), nil
}

// decodeEntry normalizes one row: chunks/vector are always non-nil on
// success, and corrupt stored data is an error — never a silent empty
// entry, never a panic.
func decodeEntry(path string, size, mtimeNs int64, mode uint32, chunksBlob []byte, vectorText string, tombstone int, tombstoneTs int64, updatedBy string) (Entry, error) {
	if tombstone != 0 && tombstone != 1 {
		return Entry{}, fmt.Errorf("store: entry %q: invalid tombstone value %d", path, tombstone)
	}
	chunks := []chunk.ChunkMeta{}
	if len(chunksBlob) > 0 {
		if err := json.Unmarshal(chunksBlob, &chunks); err != nil {
			return Entry{}, fmt.Errorf("store: entry %q: decode chunks: %w", path, err)
		}
		if chunks == nil {
			chunks = []chunk.ChunkMeta{}
		}
	}
	vec := vclock.New()
	if vectorText != "" {
		if err := vec.UnmarshalJSON([]byte(vectorText)); err != nil {
			return Entry{}, fmt.Errorf("store: entry %q: decode vector: %w", path, err)
		}
	}
	return Entry{
		Path:        path,
		Size:        size,
		MtimeNs:     mtimeNs,
		Mode:        mode,
		Chunks:      chunks,
		Vector:      vec,
		Tombstone:   tombstone == 1,
		TombstoneTs: tombstoneTs,
		UpdatedBy:   updatedBy,
	}, nil
}

const entryColumns = `path, size, mtime_ns, mode, chunks, vector, tombstone, tombstone_ts, updated_by`

func scanEntry(scanner interface{ Scan(...any) error }) (Entry, error) {
	var (
		e            Entry
		chunksBlob   []byte
		vectorText   string
		tombstoneInt int
		mode64       int64
	)
	if err := scanner.Scan(&e.Path, &e.Size, &e.MtimeNs, &mode64, &chunksBlob, &vectorText, &tombstoneInt, &e.TombstoneTs, &e.UpdatedBy); err != nil {
		return Entry{}, err
	}
	return decodeEntry(e.Path, e.Size, e.MtimeNs, uint32(mode64), chunksBlob, vectorText, tombstoneInt, e.TombstoneTs, e.UpdatedBy)
}

const upsertEntrySQL = `
INSERT INTO files (path, size, mtime_ns, mode, chunks, vector, tombstone, tombstone_ts, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
    size = excluded.size, mtime_ns = excluded.mtime_ns, mode = excluded.mode,
    chunks = excluded.chunks, vector = excluded.vector,
    tombstone = excluded.tombstone, tombstone_ts = excluded.tombstone_ts,
    updated_by = excluded.updated_by`

func entryExecArgs(e Entry) ([]any, error) {
	if e.Path == "" {
		return nil, errors.New("store: upsert entry: empty path")
	}
	if e.Size < 0 {
		return nil, fmt.Errorf("store: upsert entry %q: negative size %d", e.Path, e.Size)
	}
	chunksBlob, vectorText, err := encodeEntry(e)
	if err != nil {
		return nil, fmt.Errorf("store: upsert entry: %w", err)
	}
	return []any{
		e.Path, e.Size, e.MtimeNs, e.Mode, chunksBlob, vectorText,
		boolToInt(e.Tombstone), e.TombstoneTs, e.UpdatedBy,
	}, nil
}

// UpsertEntry inserts or fully replaces the row for e.Path in one
// transaction. Nil Chunks/Vector are stored as empty.
func (s *Store) UpsertEntry(e Entry) error {
	args, err := entryExecArgs(e)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(upsertEntrySQL, args...); err != nil {
		return fmt.Errorf("store: upsert entry %q: %w", e.Path, err)
	}
	return nil
}

// UpsertEntries writes many entries in ONE transaction (one fsync
// total instead of one per entry). The sync engine's CommitScan
// produces up to one entry per file in a tree; per-entry
// transactions made the initial commit of a 10k-file tree cost
// 10k fsyncs (≈200s on a disk with 20ms fsync latency, measured).
// Atomicity is a bonus: a scan commit lands whole or not at all.
func (s *Store) UpsertEntries(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: batch upsert: begin: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(upsertEntrySQL)
	if err != nil {
		return fmt.Errorf("store: batch upsert: prepare: %w", err)
	}
	defer stmt.Close()
	for _, e := range entries {
		args, err := entryExecArgs(e)
		if err != nil {
			return err
		}
		if _, err := stmt.Exec(args...); err != nil {
			return fmt.Errorf("store: batch upsert entry %q: %w", e.Path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: batch upsert: commit: %w", err)
	}
	return nil
}

// GetEntry returns the row for path; the bool is false when it does not
// exist (not an error).
func (s *Store) GetEntry(path string) (Entry, bool, error) {
	row := s.db.QueryRow(`SELECT `+entryColumns+` FROM files WHERE path = ?`, path)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("store: get entry %q: %w", path, err)
	}
	return e, true, nil
}

// ListEntries returns every row ordered by path ascending — always
// deterministic, so callers can diff snapshots reliably.
func (s *Store) ListEntries() ([]Entry, error) {
	rows, err := s.db.Query(`SELECT ` + entryColumns + ` FROM files ORDER BY path ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list entries: %w", err)
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list entries: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list entries: %w", err)
	}
	return out, nil
}

// DeleteEntry hard-deletes one row; a missing row is ErrNotFound.
// Business-level file deletion must NOT use this — it must upsert a
// tombstone (see history.Manager.MoveToTrash) so peers learn of the
// delete and the data lands in trash instead of vanishing.
func (s *Store) DeleteEntry(path string) error {
	res, err := s.db.Exec(`DELETE FROM files WHERE path = ?`, path)
	if err != nil {
		return fmt.Errorf("store: delete entry %q: %w", path, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete entry %q: %w", path, err)
	}
	if n == 0 {
		return fmt.Errorf("store: delete entry %q: %w", path, ErrNotFound)
	}
	return nil
}

// PurgeTombstones hard-deletes tombstone rows older than beforeTs
// (UnixNano) and returns how many were removed. This is the TTL-30-day
// reaper: once purged, a stale peer rejoining cannot resurrect the file
// from a tombstone that no longer exists locally.
func (s *Store) PurgeTombstones(beforeTs int64) (int, error) {
	res, err := s.db.Exec(`DELETE FROM files WHERE tombstone = 1 AND tombstone_ts < ?`, beforeTs)
	if err != nil {
		return 0, fmt.Errorf("store: purge tombstones: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge tombstones: %w", err)
	}
	return int(n), nil
}
