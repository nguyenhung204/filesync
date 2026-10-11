package sync

import (
	"fmt"

	"filesync/internal/index"
	"filesync/internal/store"
	"filesync/internal/vclock"
)

// CommitScan folds one scanner result into the store, assigning
// version vectors to local changes (Flow 0). It returns true when the
// store changed and peers should be offered a new round.
//
// Vector discipline: every local change increments OUR counter on the
// previous vector — creates on a tombstone continue that tombstone's
// vector (the file was re-created after a delete), renames carry the
// old path's vector to the new path. The scanner's Skipped paths are
// never interpreted: an unstable file is not a delete.
func (e *Engine) CommitScan(res index.ScanResult) (changed bool, err error) {
	self := e.id.DeviceID

	stored, err := e.st.ListEntries()
	if err != nil {
		return false, fmt.Errorf("sync: commit scan: list entries: %w", err)
	}
	storeByPath := make(map[string]store.Entry, len(stored))
	for _, en := range stored {
		storeByPath[en.Path] = en
	}
	scanByPath := make(map[string]store.Entry, len(res.Entries))
	for _, en := range res.Entries {
		scanByPath[en.Path] = en
	}
	skipped := make(map[string]bool, len(res.Skipped))
	for _, sk := range res.Skipped {
		skipped[sk.Path] = true
	}

	// All writes are collected and flushed as ONE store
	// transaction at the end (see store.UpsertEntries): a scan can
	// touch one entry per file in the tree, and one transaction +
	// one fsync is the difference between 5s and 200s for a
	// 10k-file tree on a disk with slow fsync. The in-memory map
	// is updated eagerly so the logic below sees its own writes;
	// on a flush failure nothing was persisted and the next scan
	// simply re-detects the same changes.
	var pending []store.Entry
	upsert := func(en store.Entry) error {
		storeByPath[en.Path] = en
		pending = append(pending, en)
		changed = true
		return nil
	}
	defer func() {
		if len(pending) == 0 {
			return
		}
		if berr := e.st.UpsertEntries(pending); berr != nil {
			if err == nil {
				err = fmt.Errorf("sync: commit scan: batch upsert: %w", berr)
			}
			changed = false
		}
	}()

	for _, ch := range res.Changes {
		switch ch.Kind {
		case index.ChangeCreate, index.ChangeModify:
			se, ok := scanByPath[ch.Path]
			if !ok {
				continue
			}
			vec := vclock.New().Increment(self)
			if old, found := storeByPath[ch.Path]; found {
				// Create over a tombstone continues its vector;
				// modify continues the live entry's vector.
				vec = old.Vector.Clone().Increment(self)
			}
			se.Vector = vec
			se.UpdatedBy = self
			se.Tombstone = false
			se.TombstoneTs = 0
			if err := upsert(se); err != nil {
				return changed, err
			}
		case index.ChangeRename:
			se, ok := scanByPath[ch.Path]
			if !ok {
				continue
			}
			old, found := storeByPath[ch.OldPath]
			if !found || old.Tombstone {
				// The old entry vanished in between — treat the
				// new path as a plain create.
				se.Vector = vclock.New().Increment(self)
				se.UpdatedBy = self
				se.Tombstone = false
				se.TombstoneTs = 0
				if err := upsert(se); err != nil {
					return changed, err
				}
				continue
			}
			// A rename is delete+create with linked vectors: the
			// new path's entry AND a tombstone for the old path
			// both carry old.Vector.Increment(self). The tombstone
			// is load-bearing, not bookkeeping: without it the
			// renaming node itself would pull the old path back
			// from any peer that has not seen the rename yet (the
			// peer's live entry would look like a new file), and
			// the planner's rename rule explicitly contemplates a
			// dominated/equal tombstone at the old path. (This
			// refines Flow 0's literal "DeleteEntry(OldPath)":
			// delete+create semantics are preserved, the delete is
			// just recorded the way every other delete is.)
			vec := old.Vector.Clone().Increment(self)
			ts := store.Entry{
				Path:        ch.OldPath,
				Size:        0,
				MtimeNs:     0,
				Mode:        0,
				Chunks:      nil,
				Vector:      vec.Clone(),
				Tombstone:   true,
				TombstoneTs: e.now().UnixNano(),
				UpdatedBy:   self,
			}
			if err := upsert(ts); err != nil {
				return changed, err
			}
			ne := old
			ne.Path = ch.Path
			ne.Size = se.Size
			ne.MtimeNs = se.MtimeNs
			ne.Mode = se.Mode
			ne.Chunks = se.Chunks
			ne.Vector = vec
			ne.UpdatedBy = self
			ne.Tombstone = false
			ne.TombstoneTs = 0
			if err := upsert(ne); err != nil {
				return changed, err
			}
		case index.ChangeDelete:
			if skipped[ch.Path] {
				// The scanner could not read this path this time;
				// that is not evidence of deletion.
				continue
			}
			old, found := storeByPath[ch.Path]
			if !found || old.Tombstone {
				continue
			}
			ts := store.Entry{
				Path:        ch.Path,
				Size:        0,
				MtimeNs:     0,
				Mode:        0,
				Chunks:      nil,
				Vector:      old.Vector.Clone().Increment(self),
				Tombstone:   true,
				TombstoneTs: e.now().UnixNano(),
				UpdatedBy:   self,
			}
			if err := upsert(ts); err != nil {
				return changed, err
			}
		}
	}

	// Mode-only changes: the scanner fast path returns the stored
	// entry with a fresh Mode but the differ emits no Change (content
	// is identical) — Flow 0 step 2 folds them in here.
	for _, se := range res.Entries {
		old, found := storeByPath[se.Path]
		if !found || old.Tombstone {
			continue
		}
		if old.Mode == se.Mode || old.Size != se.Size || !chunksEqual(old.Chunks, se.Chunks) {
			continue
		}
		ne := old
		ne.Mode = se.Mode
		ne.Vector = old.Vector.Clone().Increment(self)
		ne.UpdatedBy = self
		if err := upsert(ne); err != nil {
			return changed, err
		}
	}
	return changed, nil
}
