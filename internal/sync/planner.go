package sync

import (
	"encoding/hex"
	"sort"
	"strings"

	"filesync/internal/chunk"
	"filesync/internal/history"
	"filesync/internal/store"
	"filesync/internal/validate"
	"filesync/internal/vclock"
)

// ActionKind classifies one planner decision for one path.
type ActionKind string

const (
	ActionPull            ActionKind = "pull"
	ActionAdoptTombstone  ActionKind = "adopt_tombstone"
	ActionApplyDelete     ActionKind = "apply_delete"
	ActionMergeVector     ActionKind = "merge_vector"
	ActionMergeTombstone  ActionKind = "merge_tombstone"
	ActionResolveConflict ActionKind = "resolve_conflict"
	ActionApplyRename     ActionKind = "apply_rename"
)

// Action is one planner decision. Plan returns only actionable
// decisions (the table's NoOp cells produce no Action), sorted by
// Path, so two nodes that exchanged indexes converge by construction.
type Action struct {
	Kind     ActionKind
	Path     string // local path concerned (destination for renames)
	FromPath string // ApplyRename only: local source path

	Local    store.Entry // local entry, when one exists
	HasLocal bool
	Remote   store.Entry // remote entry that drove the decision

	// MergedVector is the vector the destination entry must carry
	// after the action (conflict resolution, vector merges, edit-wins
	// pulls). Nil means "the remote entry's own vector".
	MergedVector vclock.VersionVector

	// ResolveConflict only: the deterministic conflict copy path
	// (D3) and which side keeps the original path.
	ConflictPath string
	WinnerRemote bool
}

// Plan decides, purely from the two indexes and version vectors
// (ADR-003: never wall clock), what the local node must do. It is a
// pure function of its inputs — both peers run it on the same pair of
// entries and reach mirror-image conclusions.
func Plan(local, remote []store.Entry, selfID string) []Action {
	return plan(local, remote, validate.DefaultMaxLen)
}

func plan(local, remote []store.Entry, maxLen int) []Action {
	localByPath := make(map[string]store.Entry, len(local))
	for _, e := range local {
		localByPath[e.Path] = e
	}
	remoteByPath := make(map[string]store.Entry, len(remote))
	for _, e := range remote {
		// Defense layer 2 (proto validation is layer 1): a remote
		// entry whose path fails validation is skipped entirely —
		// the engine records it as bad_remote_path before planning.
		// Entries inside our own metadata tree (.filesync/...) are
		// rejected here too: a peer must never plant files into
		// versions/ or trash/ (round-2 security BLOCKER 2).
		if _, err := validate.ValidateRelPath(e.Path, maxLen); err != nil || isMetaTreePath(e.Path) {
			continue
		}
		remoteByPath[e.Path] = e
	}

	actions := []Action{}
	handledLocal := make(map[string]bool)
	handledRemote := make(map[string]bool)

	// Rename detection runs BEFORE the per-path table (spec order):
	// identical content at a new path whose vector dominates ours
	// means the data is already here — move it, never re-transfer.
	remoteSorted := sortedEntries(remote)
	localSorted := sortedEntries(local)
	for _, r := range remoteSorted {
		if r.Tombstone || handledRemote[r.Path] || len(r.Chunks) == 0 {
			continue
		}
		if _, err := validate.ValidateRelPath(r.Path, maxLen); err != nil || isMetaTreePath(r.Path) {
			continue
		}
		// A live local entry at the destination blocks the rename:
		// per-path logic (possibly a conflict) owns that path.
		if le, ok := localByPath[r.Path]; ok && !le.Tombstone {
			continue
		}
		for _, l := range localSorted {
			p1 := l.Path
			if l.Tombstone || p1 == r.Path || handledLocal[p1] {
				continue
			}
			if l.Size != r.Size || !chunksEqual(l.Chunks, r.Chunks) {
				continue
			}
			if r.Vector.Compare(l.Vector) != vclock.Dominates {
				continue
			}
			// The old path must be gone (or a dominated tombstone)
			// in the remote index too — otherwise the remote still
			// believes in the old path and this is a copy, not a
			// rename.
			if re, ok := remoteByPath[p1]; ok {
				if !re.Tombstone {
					continue
				}
				// The tombstone must be covered by the rename's
				// vector: dominated by it, or equal to it (a rename
				// committed as delete+create in one step stamps
				// both entries with the same incremented vector).
				if rel := r.Vector.Compare(re.Vector); rel != vclock.Dominates && rel != vclock.Equal {
					continue
				}
			}
			actions = append(actions, Action{
				Kind: ActionApplyRename, Path: r.Path, FromPath: p1,
				Local: l, HasLocal: true, Remote: r,
			})
			// The old path's tombstone (the delete half of the
			// rename) is adopted in the same round, so both stores
			// converge immediately instead of a round later.
			if re, ok := remoteByPath[p1]; ok && re.Tombstone {
				actions = append(actions, Action{
					Kind: ActionAdoptTombstone, Path: p1, Remote: re,
				})
			}
			handledLocal[p1] = true
			handledRemote[r.Path] = true
			handledLocal[r.Path] = true
			break
		}
	}

	paths := make(map[string]bool)
	for p := range localByPath {
		paths[p] = true
	}
	for p := range remoteByPath {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	for _, p := range sorted {
		if handledRemote[p] || handledLocal[p] {
			continue
		}
		l, hasL := localByPath[p]
		r, hasR := remoteByPath[p]
		switch {
		case !hasL && hasR:
			if r.Tombstone {
				actions = append(actions, Action{Kind: ActionAdoptTombstone, Path: p, Remote: r})
			} else {
				actions = append(actions, Action{Kind: ActionPull, Path: p, Remote: r})
			}
		case hasL && !hasR:
			// D8: absence is not deletion. No action.
		default:
			if a, ok := planPair(l, r); ok {
				actions = append(actions, a)
			}
		}
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i].Path < actions[j].Path })
	return actions
}

// planPair applies the decision table to one path present on both
// sides. ok == false is the table's NoOp.
func planPair(l, r store.Entry) (Action, bool) {
	switch {
	case !l.Tombstone && !r.Tombstone:
		return planLiveLive(l, r)
	case !l.Tombstone && r.Tombstone:
		switch rel := r.Vector.Compare(l.Vector); rel {
		case vclock.Dominates:
			return Action{Kind: ActionApplyDelete, Path: l.Path, Local: l, HasLocal: true, Remote: r}, true
		case vclock.Concurrent:
			// Delete-vs-edit, edit wins (Flow 3 variant): keep the
			// live file, just merge the tombstone's vector in.
			return Action{Kind: ActionMergeVector, Path: l.Path, Local: l, HasLocal: true, Remote: r,
				MergedVector: l.Vector.Merge(r.Vector)}, true
		}
		return Action{}, false
	case l.Tombstone && !r.Tombstone:
		switch rel := r.Vector.Compare(l.Vector); rel {
		case vclock.Dominates, vclock.Equal:
			// The edit is newer than (or the peer state for) our
			// delete: the file lives again — pull it back.
			return Action{Kind: ActionPull, Path: l.Path, Local: l, HasLocal: true, Remote: r}, true
		case vclock.Concurrent:
			// Edit wins over our delete, with the merged vector so
			// the tombstone is dominated everywhere afterwards.
			return Action{Kind: ActionPull, Path: l.Path, Local: l, HasLocal: true, Remote: r,
				MergedVector: l.Vector.Merge(r.Vector)}, true
		}
		return Action{}, false
	default: // both tombstones
		merged := l.Vector.Merge(r.Vector)
		if merged.Compare(l.Vector) != vclock.Equal || r.TombstoneTs > l.TombstoneTs {
			return Action{Kind: ActionMergeTombstone, Path: l.Path, Local: l, HasLocal: true, Remote: r,
				MergedVector: merged}, true
		}
		return Action{}, false
	}
}

func planLiveLive(l, r store.Entry) (Action, bool) {
	switch rel := r.Vector.Compare(l.Vector); rel {
	case vclock.Equal:
		if chunksEqual(l.Chunks, r.Chunks) {
			return Action{}, false
		}
		// Same vector, different content: a previous round stopped
		// mid-resolution — resolve deterministically (D3).
		return conflictAction(l, r), true
	case vclock.Dominates:
		return Action{Kind: ActionPull, Path: l.Path, Local: l, HasLocal: true, Remote: r}, true
	case vclock.Dominated:
		return Action{}, false
	default: // Concurrent
		if chunksEqual(l.Chunks, r.Chunks) {
			return Action{Kind: ActionMergeVector, Path: l.Path, Local: l, HasLocal: true, Remote: r,
				MergedVector: l.Vector.Merge(r.Vector)}, true
		}
		return conflictAction(l, r), true
	}
}

// conflictAction builds the ResolveConflict decision for a
// concurrent (or diverged-equal) pair with different content.
func conflictAction(l, r store.Entry) Action {
	winnerRemote := ConflictWinnerIsRemote(l, r)
	loser := r
	if winnerRemote {
		loser = l
	}
	return Action{
		Kind: ActionResolveConflict, Path: l.Path,
		Local: l, HasLocal: true, Remote: r,
		MergedVector: l.Vector.Merge(r.Vector),
		ConflictPath: ConflictPath(loser),
		WinnerRemote: winnerRemote,
	}
}

// ConflictWinnerIsRemote applies D3: the entry whose author
// (UpdatedBy device ID) is lexicographically larger keeps the
// original path; ties (impossible for genuinely concurrent entries)
// fall back to comparing the concatenated chunk hashes.
func ConflictWinnerIsRemote(l, r store.Entry) bool {
	if l.UpdatedBy != r.UpdatedBy {
		return r.UpdatedBy > l.UpdatedBy
	}
	return chunkListKey(r.Chunks) > chunkListKey(l.Chunks)
}

func chunkListKey(chunks []chunk.ChunkMeta) string {
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(hex.EncodeToString(c.SHA256[:]))
	}
	return sb.String()
}

// isMetaTreePath reports whether p is, or lives anywhere under,
// a directory named like FileSync's own metadata directory
// (.filesync). The local scanner prunes such a directory at ANY
// depth and the watcher ignores events under one, so a local
// entry can never carry such a path; a REMOTE entry with one is
// a peer trying to write into our version history / trash /
// staging area (or plant dark matter no local scan would ever
// see again) and is rejected as bad_remote_path at every layer
// that consumes remote paths (session filter, planner, pull
// executor). Every segment is compared case-insensitively:
// Windows and macOS filesystems are case-insensitive by
// default, so ".Filesync" there IS the real metadata tree.
func isMetaTreePath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.EqualFold(seg, history.MetaDir) {
			return true
		}
	}
	return false
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

func sortedEntries(in []store.Entry) []store.Entry {
	out := append([]store.Entry(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
