package index

import (
	"sort"

	"filesync/internal/chunk"
)

// diffSnapshots compares the previous snapshot with the current entries.
// Rename detection pairs a vanished path with a new path whose content
// hash and size are identical — per the domain rule, a rename (same hash
// at a new path) is applied without re-transferring data. Pairs are
// chosen deterministically (both sides sorted by path).
func diffSnapshots(old map[string]FileEntry, cur map[string]FileEntry) []Change {
	changes := []Change{}
	deleted := []string{}
	created := []string{}
	for p, oe := range old {
		ce, ok := cur[p]
		if !ok {
			deleted = append(deleted, p)
			continue
		}
		if oe.Size != ce.Size || !chunksEqual(oe.Chunks, ce.Chunks) {
			changes = append(changes, Change{Kind: ChangeModify, Path: p})
		}
	}
	for p := range cur {
		if _, ok := old[p]; !ok {
			created = append(created, p)
		}
	}
	sort.Strings(deleted)
	sort.Strings(created)

	usedCreated := map[string]bool{}
	renamedFrom := map[string]bool{}
	for _, d := range deleted {
		oe := old[d]
		if len(oe.Chunks) == 0 && oe.Size == 0 {
			continue // empty file has no hash identity to match a rename on
		}
		for _, c := range created {
			if usedCreated[c] {
				continue
			}
			ce := cur[c]
			if ce.Size == oe.Size && chunksEqual(oe.Chunks, ce.Chunks) {
				changes = append(changes, Change{Kind: ChangeRename, Path: c, OldPath: d})
				usedCreated[c] = true
				renamedFrom[d] = true
				break
			}
		}
	}
	for _, d := range deleted {
		if !renamedFrom[d] {
			changes = append(changes, Change{Kind: ChangeDelete, Path: d})
		}
	}
	for _, c := range created {
		if !usedCreated[c] {
			changes = append(changes, Change{Kind: ChangeCreate, Path: c})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		return changes[i].Kind < changes[j].Kind
	})
	return changes
}

func chunksEqual(a, b []chunk.ChunkMeta) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
