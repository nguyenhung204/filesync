package sync

import (
	"fmt"
	"os"

	"filesync/internal/safefile"
	"filesync/internal/store"
)

// KeepSide selects which copy of a conflict the user keeps at the
// original path when resolving manually (CLI `conflicts resolve`).
type KeepSide int

const (
	KeepMine KeepSide = iota
	KeepTheirs
)

// ResolveConflict resolves the unique conflict copy of originalPath.
// When several conflict copies exist for the same original it fails
// and the caller must disambiguate with ResolveConflictCopy. The
// losing copy always lands in trash/version history (project rule 1:
// manual resolution never destroys data either).
func (e *Engine) ResolveConflict(originalPath string, keep KeepSide) error {
	return e.ResolveConflictCopy(originalPath, "", keep)
}

// ResolveConflictCopy resolves one specific conflict copy
// (conflictPath may be "" to mean "the unique one"). The resolution
// is itself a local change: the canonical entry's vector becomes
// Merge(canonical, conflict).Increment(self) and the conflict path
// gets a normal delete tombstone, so the decision propagates to
// peers like any other edit on the next round.
func (e *Engine) ResolveConflictCopy(originalPath, conflictPath string, keep KeepSide) error {
	self := e.id.DeviceID
	entries, err := e.st.ListEntries()
	if err != nil {
		return fmt.Errorf("sync: resolve conflict: %w", err)
	}
	byPath := make(map[string]store.Entry, len(entries))
	var conflicts []store.Entry
	for _, en := range entries {
		byPath[en.Path] = en
		if en.Tombstone {
			continue
		}
		if orig, _, ok := ParseConflictPath(en.Path); ok && orig == originalPath {
			conflicts = append(conflicts, en)
		}
	}
	var k store.Entry
	switch {
	case conflictPath != "":
		found := false
		for _, c := range conflicts {
			if c.Path == conflictPath {
				k, found = c, true
			}
		}
		if !found {
			return fmt.Errorf("sync: resolve conflict: %q is not a conflict copy of %q", conflictPath, originalPath)
		}
	case len(conflicts) == 0:
		return fmt.Errorf("sync: resolve conflict: no conflict copy found for %q", originalPath)
	case len(conflicts) > 1:
		return fmt.Errorf("sync: resolve conflict: %d conflict copies for %q; name one with --conflict", len(conflicts), originalPath)
	default:
		k = conflicts[0]
	}
	c, ok := byPath[originalPath]
	if !ok || c.Tombstone {
		return fmt.Errorf("sync: resolve conflict: no live entry at %q", originalPath)
	}

	merged := c.Vector.Merge(k.Vector).Increment(self)
	tombstoneVector := k.Vector.Clone().Increment(self)

	switch keep {
	case KeepMine:
		// Canonical content stays; the conflict copy goes to trash
		// under a normal tombstone, then the canonical entry absorbs
		// the merged vector.
		if _, err := e.hist.MoveToTrash(k.Path, tombstoneVector, self); err != nil {
			return fmt.Errorf("sync: resolve conflict: %w", err)
		}
		c.Vector = merged
		if err := e.st.UpsertEntry(c); err != nil {
			return fmt.Errorf("sync: resolve conflict: upsert canonical: %w", err)
		}
	case KeepTheirs:
		// Archive the current canonical content, replace it with
		// the conflict copy's content, then trash the copy.
		if _, err := e.archive(c.Path); err != nil {
			return fmt.Errorf("sync: resolve conflict: %w", err)
		}
		abs, _, err := safefile.RequireRegularFile(e.cfg.SyncDir, k.Path, e.cfg.MaxPathLength)
		if err != nil {
			return fmt.Errorf("sync: resolve conflict: %w", err)
		}
		f, err := os.Open(abs)
		if err != nil {
			return fmt.Errorf("sync: resolve conflict: open conflict copy: %w", err)
		}
		// Stream the copy into the canonical path: conflict copies
		// can be GiB-sized, so the whole file must never sit in
		// RAM (WriteAtomicMaxLen takes any io.Reader).
		werr := safefile.WriteAtomicMaxLen(e.cfg.SyncDir, c.Path, f, os.FileMode(k.Mode), e.cfg.MaxPathLength)
		f.Close()
		if werr != nil {
			return fmt.Errorf("sync: resolve conflict: write canonical: %w", werr)
		}
		_, info, err := safefile.RequireRegularFile(e.cfg.SyncDir, c.Path, e.cfg.MaxPathLength)
		if err != nil {
			return fmt.Errorf("sync: resolve conflict: stat canonical: %w", err)
		}
		nc := store.Entry{
			Path:      c.Path,
			Size:      info.Size(),
			MtimeNs:   info.ModTime().UnixNano(),
			Mode:      uint32(info.Mode().Perm()),
			Chunks:    k.Chunks,
			Vector:    merged,
			UpdatedBy: k.UpdatedBy,
		}
		if err := e.st.UpsertEntry(nc); err != nil {
			return fmt.Errorf("sync: resolve conflict: upsert canonical: %w", err)
		}
		if _, err := e.hist.MoveToTrash(k.Path, tombstoneVector, self); err != nil {
			return fmt.Errorf("sync: resolve conflict: %w", err)
		}
	default:
		return fmt.Errorf("sync: resolve conflict: unknown keep side %d", int(keep))
	}
	return nil
}
