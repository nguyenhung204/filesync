package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"filesync/internal/safefile"
	"filesync/internal/store"
	"filesync/internal/validate"
	"filesync/internal/vclock"
)

// TrashEntry describes one file held in trash.
type TrashEntry struct {
	Name      string // base name inside .filesync/trash
	Path      string // original slash path of the file
	Size      int64
	TrashedTs int64 // UnixNano
	UpdatedBy string
}

// MoveToTrash implements Flow C: the file is moved into .filesync/trash
// FIRST and the tombstone is upserted into the store only after the data
// is safe. A crash between the two steps leaves the file preserved in
// trash with no tombstone — the next scan notices the missing path and
// the caller creates the tombstone then (self-healing). The reverse
// order is forbidden: a tombstone must never point at data that has not
// been preserved yet.
//
// History handles files only; callers decompose directory deletes into
// per-file calls. It returns the trash entry name. A locked file yields
// an error wrapping safefile.ErrLocked and NO tombstone is written, so
// the caller defers and retries instead of recording a delete that did
// not happen.
func (m *Manager) MoveToTrash(rel string, vector vclock.VersionVector, updatedBy string) (string, error) {
	if m.Store == nil {
		return "", fmt.Errorf("history: move to trash %q: no store configured", rel)
	}
	src, err := safefile.Resolve(m.Root, rel, m.maxLen())
	if err != nil {
		return "", err
	}
	info, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("history: move to trash %q: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("history: move to trash %q: not a regular file", rel)
	}
	dir := m.trashDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("history: move to trash %q: mkdir: %w", rel, err)
	}
	ts := m.now()
	clean := rel
	if c, verr := validate.ValidateRelPath(rel, m.maxLen()); verr == nil {
		clean = c
	}
	name := stampName(dir, filepath.Base(src), ts)
	dst := filepath.Join(dir, name)
	if err := m.rename(src, dst); err != nil {
		if safefile.IsLockedErr(err) {
			return "", fmt.Errorf("history: move to trash %q: %w (%v)", rel, safefile.ErrLocked, err)
		}
		return "", fmt.Errorf("history: move to trash %q: rename: %w", rel, err)
	}

	// Data is safe in trash; record the sidecar, then the tombstone.
	sc := sidecar{Path: clean, TrashedTs: ts.UnixNano(), Size: info.Size(), UpdatedBy: updatedBy}
	if vector != nil {
		vb, verr := vector.MarshalJSON()
		if verr != nil {
			return name, fmt.Errorf("history: move to trash %q: file is in trash as %q but encoding its vector failed: %w", rel, name, verr)
		}
		sc.Vector = vb
	}
	scData, err := json.Marshal(sc)
	if err != nil {
		return name, fmt.Errorf("history: move to trash %q: file is in trash as %q but encoding sidecar failed: %w", rel, name, err)
	}
	if err := os.WriteFile(dst+".json", scData, 0o600); err != nil {
		return name, fmt.Errorf("history: move to trash %q: file is in trash as %q but writing sidecar failed: %w", rel, name, err)
	}
	vec := vector
	if vec == nil {
		vec = vclock.New()
	}
	err = m.Store.UpsertEntry(store.Entry{
		Path:        clean,
		Size:        0,
		MtimeNs:     0,
		Chunks:      nil,
		Vector:      vec,
		Tombstone:   true,
		TombstoneTs: ts.UnixNano(),
		UpdatedBy:   updatedBy,
	})
	if err != nil {
		// Do NOT move the file back: a failed rollback could lose it a
		// second time. It stays discoverable in trash via ListTrash.
		return name, fmt.Errorf("history: move to trash %q: file is in trash as %q but recording tombstone failed: %w", rel, name, err)
	}
	return name, nil
}

// ListTrash lists held files, oldest first. Entries whose sidecar is
// missing or unreadable are still listed with what can be derived from
// the file itself (Path empty) — a damaged sidecar must not make user
// data invisible.
func (m *Manager) ListTrash() ([]TrashEntry, error) {
	dir := m.trashDir()
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []TrashEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("history: list trash: %w", err)
	}
	out := []TrashEntry{}
	for _, e := range ents {
		name := e.Name()
		if filepath.Ext(name) == ".json" || !e.Type().IsRegular() {
			continue
		}
		te := TrashEntry{Name: name}
		if info, err := e.Info(); err == nil {
			te.Size = info.Size()
			te.TrashedTs = info.ModTime().UnixNano()
		}
		if data, err := os.ReadFile(filepath.Join(dir, name+".json")); err == nil {
			var sc sidecar
			if json.Unmarshal(data, &sc) == nil {
				te.Path = sc.Path
				te.Size = sc.Size
				te.TrashedTs = sc.TrashedTs
				te.UpdatedBy = sc.UpdatedBy
			}
		}
		out = append(out, te)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TrashedTs != out[j].TrashedTs {
			return out[i].TrashedTs < out[j].TrashedTs
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// RestoreFromTrash moves a trashed file back to its original path (from
// its sidecar), atomically via a staging copy so the trash copy survives
// until the restore is durable. The caller clears the tombstone in the
// store afterwards — restoring data first, bookkeeping second, the same
// crash-safety order as everywhere else. name is a TrashEntry.Name.
func (m *Manager) RestoreFromTrash(name string) error {
	if err := validate.ValidateSegment(name); err != nil {
		return fmt.Errorf("history: restore from trash: invalid name: %w", err)
	}
	dir := m.trashDir()
	data, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		return fmt.Errorf("history: restore from trash %q: read sidecar: %w", name, err)
	}
	var sc sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return fmt.Errorf("history: restore from trash %q: decode sidecar: %w", name, err)
	}
	src := filepath.Join(dir, name)
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("history: restore from trash %q: %w", name, err)
	}
	// Archive whatever currently sits at the target so restoring never
	// silently destroys a newer live file.
	if _, err := m.Archive(sc.Path); err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("history: restore from trash %q: %w", name, err)
	}
	defer f.Close()
	if err := safefile.WriteAtomicMaxLen(m.Root, sc.Path, f, info.Mode(), m.maxLen()); err != nil {
		return fmt.Errorf("history: restore from trash %q: %w", name, err)
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("history: restore from trash %q: remove trash copy: %w", name, err)
	}
	_ = os.Remove(filepath.Join(dir, name+".json"))
	return nil
}

// PurgeExpired is the TTL reaper (domain rule: 30 days). It removes trash
// files whose sidecar timestamp is older than now-TombstoneTTL — those
// are FileSync-owned copies past their retention window — and purges the
// matching tombstones from the store so an offline peer rejoining later
// cannot resurrect the file. It returns the number of trash files
// removed. now is taken from the injectable clock.
func (m *Manager) PurgeExpired() (int, error) {
	cutoff := m.now().Add(-TombstoneTTL)
	entries, err := m.ListTrash()
	if err != nil {
		return 0, err
	}
	removed := 0
	dir := m.trashDir()
	for _, te := range entries {
		if te.TrashedTs < cutoff.UnixNano() {
			if err := os.Remove(filepath.Join(dir, te.Name)); err != nil {
				return removed, fmt.Errorf("history: purge trash %q: %w", te.Name, err)
			}
			_ = os.Remove(filepath.Join(dir, te.Name+".json"))
			removed++
		}
	}
	if m.Store != nil {
		if _, err := m.Store.PurgeTombstones(cutoff.UnixNano()); err != nil {
			return removed, fmt.Errorf("history: purge tombstones: %w", err)
		}
	}
	return removed, nil
}
