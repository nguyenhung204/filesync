// Package store is FileSync's metadata source of truth: a SQLite database
// in WAL mode (via the pure-Go modernc.org/sqlite driver, keeping the
// binary static and CGO-free) holding the file index, paired peers, and
// resumable transfer journals.
//
// Crash-safety contract shared with the rest of FileSync: file data is
// always written, fsynced and renamed BEFORE the store is updated, so the
// store never points at content that is not durable on disk. Every store
// write is its own transaction; SQLite WAL recovery restores the last
// committed state after kill -9 or power loss.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"filesync/internal/chunk"
	"filesync/internal/vclock"
)

// ErrNotFound is returned by operations that require an existing row.
var ErrNotFound = errors.New("store: not found")

// Entry is the single shared FileEntry type for the whole project;
// internal/index aliases it instead of defining a parallel struct.
type Entry struct {
	Path        string               // clean slash-separated path, already validated
	Size        int64                // >= 0; 0 for tombstones
	MtimeNs     int64                // mtime UnixNano at hash time; 0 for tombstones
	Mode        uint32               // fs.FileMode.Perm() bits (incl. executable)
	Chunks      []chunk.ChunkMeta    // empty (non-nil after a DB read) for 0-byte files / tombstones
	Vector      vclock.VersionVector // non-nil after a DB read
	Tombstone   bool
	TombstoneTs int64  // UnixNano UTC of the delete; 0 when not a tombstone
	UpdatedBy   string // device ID that produced this state; "" if unknown
}

// Peer is a paired device as recorded at pairing time.
type Peer struct {
	DeviceID        string
	Name            string
	CertFingerprint string   // display form from identity.FingerprintFromCertDER
	Addresses       []string // JSON array in DB; nil is stored as an empty array
	Revoked         bool
	PairedAt        int64 // UnixNano UTC of pairing
}

// TransferJournal is the resume record for one in-flight transfer. Bitmap
// is the opaque chunk.Bitmap.MarshalBinary encoding; the store never
// interprets it.
type TransferJournal struct {
	Path        string
	Size        int64
	TotalChunks int
	Bitmap      []byte
	UpdatedBy   string
}

// schemaVersion is the highest migration this build knows how to apply.
const schemaVersion = 1

// migrations[v] upgrades the DB from version v to v+1 in one transaction.
var migrations = []string{
	// 0 -> 1: initial schema.
	`
CREATE TABLE files (
    path         TEXT PRIMARY KEY,
    size         INTEGER NOT NULL CHECK (size >= 0),
    mtime_ns     INTEGER NOT NULL DEFAULT 0,
    mode         INTEGER NOT NULL DEFAULT 0,
    chunks       BLOB    NOT NULL DEFAULT X'',
    vector       TEXT    NOT NULL DEFAULT '{}',
    tombstone    INTEGER NOT NULL DEFAULT 0 CHECK (tombstone IN (0, 1)),
    tombstone_ts INTEGER NOT NULL DEFAULT 0,
    updated_by   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_files_tombstone ON files(tombstone) WHERE tombstone = 1;
CREATE TABLE peers (
    device_id         TEXT PRIMARY KEY,
    name              TEXT NOT NULL DEFAULT '',
    cert_fingerprint  TEXT NOT NULL DEFAULT '',
    addresses         TEXT NOT NULL DEFAULT '[]',
    revoked           INTEGER NOT NULL DEFAULT 0 CHECK (revoked IN (0, 1)),
    paired_at         INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);
`,
}

// Store wraps the SQLite handle. It is safe for concurrent use; the
// single underlying connection serializes all access.
type Store struct {
	db   *sql.DB
	path string
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Open creates or migrates the database at <dir>/filesync.db. Opening is
// idempotent: opening an up-to-date database changes nothing. A database
// from a NEWER FileSync (user_version above what this build knows) is
// rejected rather than risk corrupting its schema.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("store: open: empty directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: open: mkdir %q: %w", dir, err)
	}
	// Tighten permissions even if the directory pre-existed looser.
	// Best-effort: on Windows Chmod only affects the read-only bit.
	_ = os.Chmod(dir, 0o700)

	path := filepath.Join(dir, "filesync.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	// SQLite allows a single writer even in WAL mode; pinning the pool to
	// one connection serializes our own access and avoids SQLITE_BUSY
	// from competing in-process transactions.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	// The DB holds the sync index (a map of the user's files) — treat it
	// as sensitive: 0600 on Unix, best-effort like config/identity.
	_ = os.Chmod(path, 0o600)
	return s, nil
}

// init applies the WAL/durability pragmas and runs pending migrations.
func (s *Store) init() error {
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		return fmt.Errorf("store: set journal_mode=WAL: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("store: journal_mode is %q, want wal", mode)
	}
	if _, err := s.db.Exec(`PRAGMA synchronous=FULL`); err != nil {
		return fmt.Errorf("store: set synchronous=FULL: %w", err)
	}
	return s.migrate()
}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	if v > schemaVersion {
		return fmt.Errorf("store: database schema version %d is newer than this build supports (%d); refusing to open", v, schemaVersion)
	}
	for ; v < schemaVersion; v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("store: migrate %d->%d: begin: %w", v, v+1, err)
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migrate %d->%d: %w", v, v+1, err)
		}
		// PRAGMA does not accept bound parameters; v is our own int.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migrate %d->%d: set user_version: %w", v, v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migrate %d->%d: commit: %w", v, v+1, err)
		}
	}
	return nil
}

// JournalMode reports the live journal mode (tests assert it is "wal").
func (s *Store) JournalMode() (string, error) {
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		return "", fmt.Errorf("store: read journal_mode: %w", err)
	}
	return mode, nil
}

// UserVersion reports the schema version stored in the database.
func (s *Store) UserVersion() (int, error) {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read user_version: %w", err)
	}
	return v, nil
}

// Close closes the database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// boolToInt converts a bool to the 0/1 stored in SQLite.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
