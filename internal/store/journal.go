package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// The meta table backs both the free-form journal (JournalSet/Get/Delete)
// and the structured transfer journals used for resume after a crash.
// Transfer journals live under keys "transfer:<path>"; their values are
// JSON encodings of TransferJournal. The store treats Bitmap as opaque.

// JournalSet stores value under key, replacing any previous value. The
// value is copied.
func (s *Store) JournalSet(key string, value []byte) error {
	if key == "" {
		return errors.New("store: journal set: empty key")
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	_, err := s.db.Exec(`
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, cp)
	if err != nil {
		return fmt.Errorf("store: journal set %q: %w", key, err)
	}
	return nil
}

// JournalGet returns a copy of the value stored under key; the bool is
// false when the key does not exist.
func (s *Store) JournalGet(key string) ([]byte, bool, error) {
	var v []byte
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: journal get %q: %w", key, err)
	}
	out := make([]byte, len(v))
	copy(out, v)
	return out, true, nil
}

// JournalDelete removes a journal key; a missing key is not an error.
func (s *Store) JournalDelete(key string) error {
	if _, err := s.db.Exec(`DELETE FROM meta WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: journal delete %q: %w", key, err)
	}
	return nil
}

const transferKeyPrefix = "transfer:"

type transferRecord struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	TotalChunks int    `json:"total_chunks"`
	Bitmap      []byte `json:"bitmap"`
	UpdatedBy   string `json:"updated_by"`
}

// PutTransfer creates or replaces the resume journal for j.Path. The sync
// engine calls this before receiving the first chunk of a file and as the
// received-chunk bitmap grows, so a crash mid-transfer loses nothing but
// the chunks still in flight.
func (s *Store) PutTransfer(j TransferJournal) error {
	if j.Path == "" {
		return errors.New("store: put transfer: empty path")
	}
	rec := transferRecord{Path: j.Path, Size: j.Size, TotalChunks: j.TotalChunks, Bitmap: j.Bitmap, UpdatedBy: j.UpdatedBy}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("store: put transfer %q: %w", j.Path, err)
	}
	return s.JournalSet(transferKeyPrefix+j.Path, data)
}

// GetTransfer returns the journal for path; the bool is false when there
// is no transfer in flight for it.
func (s *Store) GetTransfer(path string) (TransferJournal, bool, error) {
	data, ok, err := s.JournalGet(transferKeyPrefix + path)
	if err != nil || !ok {
		return TransferJournal{}, ok, err
	}
	var rec transferRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return TransferJournal{}, false, fmt.Errorf("store: get transfer %q: decode: %w", path, err)
	}
	return TransferJournal(rec), true, nil
}

// ListTransfers returns every in-flight transfer journal, ordered by path
// ascending. At startup (Flow D) each returned journal is one transfer
// that was interrupted and can be resumed from its bitmap.
func (s *Store) ListTransfers() ([]TransferJournal, error) {
	rows, err := s.db.Query(`SELECT value FROM meta WHERE key LIKE 'transfer:%' ORDER BY key ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list transfers: %w", err)
	}
	defer rows.Close()
	out := []TransferJournal{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("store: list transfers: %w", err)
		}
		var rec transferRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("store: list transfers: decode: %w", err)
		}
		out = append(out, TransferJournal(rec))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list transfers: %w", err)
	}
	return out, nil
}

// ClearTransfer removes the journal for path once its transfer finished
// (or was deliberately abandoned). A missing journal is not an error.
func (s *Store) ClearTransfer(path string) error {
	return s.JournalDelete(transferKeyPrefix + path)
}
