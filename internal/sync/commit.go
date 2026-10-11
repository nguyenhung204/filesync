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
func (e *Engine) CommitScan(res index.ScanResult) (bool, error) {
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

	changed := false
	upsert := func(en store.Entry) error {
		if err := e.st.UpsertEntry(en); err != nil {
			return fmt.Errorf("sync: commit scan: upsert %q: %w", en.Path, err)
		}
		storeByPath[en.Path] = en
		changed = true
		return nil
	}

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
			ne := old
			ne.Path = ch.Path
			ne.Size = se.Size
			ne.MtimeNs = se.MtimeNs
			ne.Mode = se.Mode
			ne.Chunks = se.Chunks
			ne.Vector = old.Vector.Clone().Increment(self)
			ne.UpdatedBy = self
			ne.Tombstone = false
			ne.TombstoneTs = 0
			if err := e.st.DeleteEntry(ch.OldPath); err != nil {
				return changed, fmt.Errorf("sync: commit scan: delete renamed-away %q: %w", ch.OldPath, err)
			}
			delete(storeByPath, ch.OldPath)
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
