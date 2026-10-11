package sync

import (
	"crypto/sha256"
	"sort"
	"testing"

	"filesync/internal/chunk"
	"filesync/internal/store"
	"filesync/internal/vclock"
)

func meta(seed byte, size int) chunk.ChunkMeta {
	return chunk.ChunkMeta{Index: 0, SHA256: sha256.Sum256([]byte{seed}), Size: size}
}

func vv(pairs map[string]uint64) vclock.VersionVector {
	v := vclock.New()
	for k, n := range pairs {
		v[k] = n
	}
	return v
}

func live(path string, vec vclock.VersionVector, by string, chunks ...chunk.ChunkMeta) store.Entry {
	if chunks == nil {
		chunks = []chunk.ChunkMeta{}
	}
	var size int64
	for _, c := range chunks {
		size += int64(c.Size)
	}
	return store.Entry{Path: path, Size: size, MtimeNs: 1000, Mode: 0o644, Chunks: chunks, Vector: vec, UpdatedBy: by}
}

func tomb(path string, vec vclock.VersionVector, by string, ts int64) store.Entry {
	return store.Entry{Path: path, Vector: vec, Tombstone: true, TombstoneTs: ts, UpdatedBy: by}
}

func actionKeys(actions []Action) []string {
	out := []string{}
	for _, a := range actions {
		out = append(out, string(a.Kind)+" "+a.Path)
	}
	sort.Strings(out)
	return out
}

func TestPlannerTable(t *testing.T) {
	cA := []chunk.ChunkMeta{meta(1, 10)}
	cB := []chunk.ChunkMeta{meta(2, 10)}
	base := vv(map[string]uint64{"A": 1})
	cases := []struct {
		name   string
		local  []store.Entry
		remote []store.Entry
		want   []string
	}{
		{"absent-live pulls",
			nil,
			[]store.Entry{live("f", base, "B", cA...)},
			[]string{"pull f"}},
		{"absent-tombstone adopts",
			nil,
			[]store.Entry{tomb("f", base, "B", 5)},
			[]string{"adopt_tombstone f"}},
		{"live-absent noop",
			[]store.Entry{live("f", base, "A", cA...)},
			nil, nil},
		{"tombstone-absent noop",
			[]store.Entry{tomb("f", base, "A", 5)},
			nil, nil},
		{"live-live equal same chunks noop",
			[]store.Entry{live("f", base, "A", cA...)},
			[]store.Entry{live("f", base, "B", cA...)},
			nil},
		{"live-live equal diff chunks conflict",
			[]store.Entry{live("f", base, "A", cA...)},
			[]store.Entry{live("f", base, "B", cB...)},
			[]string{"resolve_conflict f"}},
		{"live-live remote dominates pulls",
			[]store.Entry{live("f", base, "A", cA...)},
			[]store.Entry{live("f", vv(map[string]uint64{"A": 1, "B": 1}), "B", cB...)},
			[]string{"pull f"}},
		{"live-live local dominates noop",
			[]store.Entry{live("f", vv(map[string]uint64{"A": 2}), "A", cA...)},
			[]store.Entry{live("f", base, "B", cB...)},
			nil},
		{"live-live concurrent same chunks merges vector",
			[]store.Entry{live("f", vv(map[string]uint64{"A": 2}), "A", cA...)},
			[]store.Entry{live("f", vv(map[string]uint64{"B": 1}), "B", cA...)},
			[]string{"merge_vector f"}},
		{"live-live concurrent diff chunks conflict",
			[]store.Entry{live("f", vv(map[string]uint64{"A": 2}), "A", cA...)},
			[]store.Entry{live("f", vv(map[string]uint64{"B": 1}), "B", cB...)},
			[]string{"resolve_conflict f"}},
		{"live-tombstone remote dominates deletes",
			[]store.Entry{live("f", base, "A", cA...)},
			[]store.Entry{tomb("f", vv(map[string]uint64{"A": 1, "B": 1}), "B", 9)},
			[]string{"apply_delete f"}},
		{"live-tombstone local dominates noop",
			[]store.Entry{live("f", vv(map[string]uint64{"A": 2, "B": 1}), "A", cA...)},
			[]store.Entry{tomb("f", vv(map[string]uint64{"A": 1, "B": 1}), "B", 9)},
			nil},
		{"live-tombstone concurrent edit wins merge",
			[]store.Entry{live("f", vv(map[string]uint64{"A": 2}), "A", cA...)},
			[]store.Entry{tomb("f", vv(map[string]uint64{"A": 1, "B": 2}), "B", 9)},
			[]string{"merge_vector f"}},
		{"tombstone-live remote dominates revives pull",
			[]store.Entry{tomb("f", base, "A", 5)},
			[]store.Entry{live("f", vv(map[string]uint64{"A": 1, "B": 1}), "B", cA...)},
			[]string{"pull f"}},
		{"tombstone-live local dominates noop",
			[]store.Entry{tomb("f", vv(map[string]uint64{"A": 2}), "A", 5)},
			[]store.Entry{live("f", base, "B", cA...)},
			nil},
		{"tombstone-live concurrent edit wins pull merged",
			[]store.Entry{tomb("f", vv(map[string]uint64{"A": 2}), "A", 5)},
			[]store.Entry{live("f", vv(map[string]uint64{"A": 1, "B": 1}), "B", cA...)},
			[]string{"pull f"}},
		{"tombstone-tombstone merges",
			[]store.Entry{tomb("f", vv(map[string]uint64{"A": 2}), "A", 5)},
			[]store.Entry{tomb("f", vv(map[string]uint64{"A": 1, "B": 1}), "B", 9)},
			[]string{"merge_tombstone f"}},
		{"tombstone-tombstone identical noop",
			[]store.Entry{tomb("f", base, "A", 5)},
			[]store.Entry{tomb("f", base, "B", 5)},
			nil},
		{"rename detected",
			[]store.Entry{live("old.txt", base, "A", cA...)},
			[]store.Entry{live("new.txt", vv(map[string]uint64{"A": 1, "B": 1}), "B", cA...),
				tomb("old.txt", vv(map[string]uint64{"A": 1, "B": 1}), "B", 9)},
			[]string{"apply_rename new.txt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := actionKeys(Plan(tc.local, tc.remote, "SELF"))
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("actions = %v, want %v", got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("actions = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestPlannerConcurrentCreateIsConflict(t *testing.T) {
	// Both sides created the same path independently with different
	// content: fresh vectors, different authors → conflict, and the
	// winner is the larger UpdatedBy on BOTH nodes' computations.
	l := live("same.txt", vv(map[string]uint64{"ALPHA": 1}), "ALPHA", meta(1, 10))
	r := live("same.txt", vv(map[string]uint64{"BRAVO": 1}), "BRAVO", meta(2, 10))
	acts := Plan([]store.Entry{l}, []store.Entry{r}, "ALPHA")
	if len(acts) != 1 || acts[0].Kind != ActionResolveConflict {
		t.Fatalf("actions = %+v, want one resolve_conflict", acts)
	}
	if !acts[0].WinnerRemote {
		t.Fatal("BRAVO > ALPHA: remote should win")
	}
	if acts[0].ConflictPath != ConflictPath(l) {
		t.Fatalf("conflict path = %q, want loser-based %q", acts[0].ConflictPath, ConflictPath(l))
	}
	// Mirror computation on the other node must agree exactly.
	acts2 := Plan([]store.Entry{r}, []store.Entry{l}, "BRAVO")
	if len(acts2) != 1 || acts2[0].Kind != ActionResolveConflict || acts2[0].WinnerRemote {
		t.Fatalf("mirror actions = %+v, want resolve_conflict with local winner", acts2)
	}
	if acts2[0].ConflictPath != acts[0].ConflictPath {
		t.Fatalf("conflict paths differ: %q vs %q", acts2[0].ConflictPath, acts[0].ConflictPath)
	}
	if acts2[0].MergedVector.Compare(acts[0].MergedVector) != vclock.Equal {
		t.Fatal("merged vectors differ between nodes")
	}
}

func TestPlannerDeleteVsEdit(t *testing.T) {
	// A deleted (tombstone), B edited concurrently: edit wins — A's
	// plan pulls with the merged vector; B's plan only merges.
	del := tomb("f", vv(map[string]uint64{"A": 2}), "A", 100)
	edit := live("f", vv(map[string]uint64{"A": 1, "B": 1}), "B", meta(7, 10))
	actsA := Plan([]store.Entry{del}, []store.Entry{edit}, "A")
	if len(actsA) != 1 || actsA[0].Kind != ActionPull {
		t.Fatalf("A actions = %+v, want pull", actsA)
	}
	if actsA[0].MergedVector == nil || actsA[0].MergedVector.Get("A") != 2 || actsA[0].MergedVector.Get("B") != 1 {
		t.Fatalf("A merged vector = %v, want {A:2,B:1}", actsA[0].MergedVector)
	}
	actsB := Plan([]store.Entry{edit}, []store.Entry{del}, "B")
	if len(actsB) != 1 || actsB[0].Kind != ActionMergeVector {
		t.Fatalf("B actions = %+v, want merge_vector", actsB)
	}
}

func TestPlannerRenameNoTransfer(t *testing.T) {
	// A pure rename on the remote must plan as apply_rename — the
	// action that by construction moves zero chunk bytes.
	l := live("docs/a.txt", vv(map[string]uint64{"A": 1}), "A", meta(3, 42))
	r := live("docs/b.txt", vv(map[string]uint64{"A": 1, "B": 1}), "B", meta(3, 42))
	rt := tomb("docs/a.txt", vv(map[string]uint64{"A": 1, "B": 1}), "B", 7)
	acts := Plan([]store.Entry{l}, []store.Entry{r, rt}, "A")
	if len(acts) != 1 || acts[0].Kind != ActionApplyRename {
		t.Fatalf("actions = %+v, want apply_rename only", acts)
	}
	if acts[0].FromPath != "docs/a.txt" || acts[0].Path != "docs/b.txt" {
		t.Fatalf("rename action = %+v", acts[0])
	}
}

func TestPlannerIgnoresClockSkew(t *testing.T) {
	// MtimeNs lies in both directions; the winner must follow
	// UpdatedBy (vector authorship), never the clock.
	old := live("f", vv(map[string]uint64{"ALPHA": 1}), "ALPHA", meta(1, 10))
	old.MtimeNs = 1 // 1970
	newer := live("f", vv(map[string]uint64{"BRAVO": 1}), "BRAVO", meta(2, 10))
	newer.MtimeNs = 1 << 62 // year 2262
	if !ConflictWinnerIsRemote(old, newer) {
		t.Fatal("BRAVO should win by UpdatedBy despite ALPHA's smaller mtime")
	}
	old.MtimeNs, newer.MtimeNs = newer.MtimeNs, old.MtimeNs
	if !ConflictWinnerIsRemote(old, newer) {
		t.Fatal("winner changed when mtimes swapped — clock leaked into ordering")
	}
	if ConflictWinnerIsRemote(newer, old) {
		t.Fatal("mirror: ALPHA (now remote) must not win")
	}
}

func TestPlannerRejectsBadRemotePaths(t *testing.T) {
	remote := []store.Entry{
		live("../evil.txt", vv(map[string]uint64{"B": 1}), "B", meta(1, 10)),
		live("/abs.txt", vv(map[string]uint64{"B": 1}), "B", meta(1, 10)),
		live("ok.txt", vv(map[string]uint64{"B": 1}), "B", meta(1, 10)),
	}
	acts := Plan(nil, remote, "A")
	if len(acts) != 1 || acts[0].Kind != ActionPull || acts[0].Path != "ok.txt" {
		t.Fatalf("actions = %+v, want pull of ok.txt only", acts)
	}
}

func TestPlannerOfflineRejoinMerges(t *testing.T) {
	// Node A was offline while B edited twice and C (via B) once:
	// A's entry is strictly dominated → pull, no conflict, no
	// resurrection games. And a node rejoining with an entry the
	// peer has a tombstone for, where the tombstone dominates,
	// accepts the delete.
	stale := live("f", vv(map[string]uint64{"A": 1}), "A", meta(1, 10))
	fresh := live("f", vv(map[string]uint64{"A": 1, "B": 2, "C": 1}), "B", meta(9, 10))
	acts := Plan([]store.Entry{stale}, []store.Entry{fresh}, "A")
	if len(acts) != 1 || acts[0].Kind != ActionPull {
		t.Fatalf("actions = %+v, want pull", acts)
	}
	// Within-TTL resurrection guard: peer's tombstone dominates the
	// rejoining node's stale live entry → the delete is applied.
	gone := tomb("g", vv(map[string]uint64{"A": 1, "B": 1}), "B", 50)
	staleLive := live("g", vv(map[string]uint64{"A": 1}), "A", meta(1, 10))
	acts2 := Plan([]store.Entry{staleLive}, []store.Entry{gone}, "A")
	if len(acts2) != 1 || acts2[0].Kind != ActionApplyDelete {
		t.Fatalf("actions = %+v, want apply_delete", acts2)
	}
}
