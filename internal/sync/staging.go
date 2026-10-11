package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"filesync/internal/chunk"
	"filesync/internal/history"
	"filesync/internal/store"
)

// Staging lives under <SyncDir>/.filesync/partial/ (D2). The staging
// file name is a fingerprint of the destination entry, so a staging
// file proves by its name which (path, size, chunk list) it belongs
// to; when the destination entry changes, the old staging file simply
// becomes an orphan and is reaped by CleanupStaleStaging.
//
// The transfer journal (store.PutTransfer) cannot carry the
// fingerprint (its record shape belongs to the store package), so the
// engine keeps it under its own journal key "sync:fp:<path>", written
// BEFORE the transfer journal — a crash between the two leaves an fp
// record with no journal, which cleanup treats as stale, never the
// reverse (a journal claiming chunks whose staging cannot be named).

const fpJournalPrefix = "sync:fp:"

func stagingFingerprint(e store.Entry) string {
	h := sha256.New()
	h.Write([]byte(e.Path))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(e.Size, 10)))
	h.Write([]byte{0})
	for _, c := range e.Chunks {
		h.Write(c.SHA256[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (e *Engine) partialDir() string {
	return filepath.Join(e.cfg.SyncDir, history.MetaDir, "partial")
}

func (e *Engine) stagingAbs(fp string) string {
	return filepath.Join(e.partialDir(), fp+".part")
}

// staging is one prepared receive buffer for a pull.
type staging struct {
	abs    string
	fp     string
	bitmap *chunk.Bitmap
	file   *os.File
}

// prepareStaging opens (or creates) the staging file for destEntry
// and returns the received-chunk bitmap: resumed from the transfer
// journal when path/size/chunk-count and the fingerprint all match
// (Flow 2 step 3), otherwise started fresh — a mismatched journal or
// staging file is discarded rather than trusted.
func (e *Engine) prepareStaging(destEntry store.Entry) (*staging, error) {
	fp := stagingFingerprint(destEntry)
	abs := e.stagingAbs(fp)
	if err := os.MkdirAll(e.partialDir(), 0o755); err != nil {
		return nil, fmt.Errorf("sync: mkdir partial dir: %w", err)
	}

	resume := false
	var bm *chunk.Bitmap
	if j, ok, err := e.st.GetTransfer(destEntry.Path); err == nil && ok &&
		j.Size == destEntry.Size && j.TotalChunks == len(destEntry.Chunks) {
		if savedFP, fpOK, ferr := e.st.JournalGet(fpJournalPrefix + destEntry.Path); ferr == nil &&
			fpOK && string(savedFP) == fp {
			candidate := chunk.NewBitmap(len(destEntry.Chunks))
			if err := candidate.UnmarshalBinary(j.Bitmap); err == nil && candidate.Len() == len(destEntry.Chunks) {
				if _, serr := os.Stat(abs); serr == nil {
					bm = candidate
					resume = true
				}
			}
		}
	}
	if !resume {
		// Fresh start: any old journal/staging for this path belongs
		// to a superseded entry — drop both rather than mix chunks
		// from two different contents.
		_ = e.st.ClearTransfer(destEntry.Path)
		_ = e.st.JournalDelete(fpJournalPrefix + destEntry.Path)
		_ = os.Remove(abs)
		bm = chunk.NewBitmap(len(destEntry.Chunks))
	}

	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sync: open staging %s: %w", destEntry.Path, err)
	}
	if !resume {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, fmt.Errorf("sync: truncate staging %s: %w", destEntry.Path, err)
		}
	}
	st := &staging{abs: abs, fp: fp, bitmap: bm, file: f}
	if err := e.persistJournal(destEntry, bm); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// persistJournal records the fingerprint first, then the transfer
// journal with the current bitmap (Flow B: journal before first chunk
// and after every chunk).
func (e *Engine) persistJournal(destEntry store.Entry, bm *chunk.Bitmap) error {
	if err := e.st.JournalSet(fpJournalPrefix+destEntry.Path, []byte(stagingFingerprint(destEntry))); err != nil {
		return fmt.Errorf("sync: persist staging fingerprint %s: %w", destEntry.Path, err)
	}
	raw, err := bm.MarshalBinary()
	if err != nil {
		return fmt.Errorf("sync: encode bitmap %s: %w", destEntry.Path, err)
	}
	if err := e.st.PutTransfer(store.TransferJournal{
		Path:        destEntry.Path,
		Size:        destEntry.Size,
		TotalChunks: len(destEntry.Chunks),
		Bitmap:      raw,
		UpdatedBy:   destEntry.UpdatedBy,
	}); err != nil {
		return fmt.Errorf("sync: persist transfer journal %s: %w", destEntry.Path, err)
	}
	return nil
}

func (s *staging) Close() {
	if s.file != nil {
		_ = s.file.Sync()
		_ = s.file.Close()
		s.file = nil
	}
}

// discard removes the staging file and both journal records for path.
func (e *Engine) discardStaging(path, abs string) {
	_ = os.Remove(abs)
	_ = e.st.ClearTransfer(path)
	_ = e.st.JournalDelete(fpJournalPrefix + path)
}

// CleanupStaleStaging reaps orphaned staging files at startup (D2):
// a .part file survives only when a live transfer journal names its
// fingerprint; journals without a fingerprint record are stale and
// cleared. User data is never touched — only FileSync's own partial
// tree.
func (e *Engine) CleanupStaleStaging() error {
	journals, err := e.st.ListTransfers()
	if err != nil {
		return fmt.Errorf("sync: cleanup staging: %w", err)
	}
	valid := make(map[string]bool)
	for _, j := range journals {
		fp, ok, err := e.st.JournalGet(fpJournalPrefix + j.Path)
		if err != nil || !ok {
			// Journal without a fingerprint: cannot prove which
			// staging it owns — treat as stale.
			_ = e.st.ClearTransfer(j.Path)
			continue
		}
		valid[string(fp)] = true
	}
	ents, err := os.ReadDir(e.partialDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("sync: cleanup staging: %w", err)
	}
	for _, ent := range ents {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".part") {
			continue
		}
		fp := strings.TrimSuffix(name, ".part")
		if !valid[fp] {
			_ = os.Remove(filepath.Join(e.partialDir(), name))
		}
	}
	return nil
}
