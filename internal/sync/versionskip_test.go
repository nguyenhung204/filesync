package sync

import (
	"bytes"
	"strings"
	"testing"

	"filesync/internal/logx"
	"filesync/internal/store"
)

// seedLiveEntry writes rel with content and registers it as a local
// entry authored by this node (vector {self:1}), so a scripted
// remote entry with a dominating vector triggers an overwrite pull.
func seedLiveEntry(t *testing.T, n *testNode, rel string, content []byte) {
	t.Helper()
	writeFile(t, n.root, rel, content, 0o644)
	self := n.id.DeviceID
	local := store.Entry{
		Path: rel, Size: int64(len(content)), MtimeNs: 1, Mode: 0o644,
		Chunks:    metasOf(content),
		Vector:    vv(map[string]uint64{self: 1}),
		UpdatedBy: self,
	}
	if err := n.st.UpsertEntry(local); err != nil {
		t.Fatal(err)
	}
}

// pullOverwrite drives one scripted round in which the peer's entry
// for rel dominates the local one, so the engine pulls content over
// the live file (through finalizePull → archive).
func pullOverwrite(t *testing.T, n *testNode, rel string, content []byte) {
	t.Helper()
	self := n.id.DeviceID
	remote := store.Entry{
		Path: rel, Size: int64(len(content)), MtimeNs: 2, Mode: 0o644,
		Chunks:    metasOf(content),
		Vector:    vv(map[string]uint64{self: 1, scriptID: 1}),
		UpdatedBy: scriptID,
	}
	p := newScriptPeer([]store.Entry{remote}, map[string][]byte{rel: content})
	runScriptedRound(t, n, p)
}

// Spec D1/D4/D6: a live file strictly over the quota is overwritten
// WITHOUT calling the archiver; the skip is recorded with the LIVE
// (old) size; the new content lands normally.
func TestArchiveSkippedForOversizedFileOnPull(t *testing.T) {
	n := newTestNode(t)
	old := pattern(100, 5)
	fresh := pattern(120, 9)
	seedLiveEntry(t, n, "big.txt", old)
	n.eng.cfg.VersionHistoryQuotaBytes = int64(len(old)) - 1
	calls := 0
	n.eng.archiveFn = func(rel string) (string, error) { calls++; return "v", nil }

	pullOverwrite(t, n, "big.txt", fresh)

	if calls != 0 {
		t.Fatalf("archiveFn called %d times, want 0 (skip must precede the seam)", calls)
	}
	if data := readFile(t, n.root, "big.txt"); !bytes.Equal(data, fresh) {
		t.Fatal("overwrite did not land after the archive skip")
	}
	count, last := n.eng.SkippedArchives()
	if count != 1 {
		t.Fatalf("SkippedArchives count = %d, want 1", count)
	}
	if last == nil || last.Path != "big.txt" || last.Size != int64(len(old)) || last.At <= 0 {
		t.Fatalf("last skip = %+v, want {big.txt %d <unixnano>}", last, len(old))
	}
	if len(n.eng.LastErrors()) != 0 {
		t.Fatalf("skip recorded as an error: %+v", n.eng.LastErrors())
	}
}

// Spec D1 boundary: size == quota archives; size == quota+1 skips;
// quota == 0 means unlimited (never skips).
func TestArchiveQuotaBoundary(t *testing.T) {
	old := pattern(100, 5)
	fresh := pattern(120, 9)
	for _, tc := range []struct {
		name      string
		quota     int64
		wantCalls int
		wantSkips int64
	}{
		{"size equals quota archives", int64(len(old)), 1, 0},
		{"size one over quota skips", int64(len(old)) - 1, 0, 1},
		{"zero quota is unlimited", 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newTestNode(t)
			seedLiveEntry(t, n, "f.bin", old)
			n.eng.cfg.VersionHistoryQuotaBytes = tc.quota
			calls := 0
			n.eng.archiveFn = func(rel string) (string, error) { calls++; return "v", nil }
			pullOverwrite(t, n, "f.bin", fresh)
			if calls != tc.wantCalls {
				t.Fatalf("archiveFn calls = %d, want %d", calls, tc.wantCalls)
			}
			if count, _ := n.eng.SkippedArchives(); count != tc.wantSkips {
				t.Fatalf("skip count = %d, want %d", count, tc.wantSkips)
			}
			if data := readFile(t, n.root, "f.bin"); !bytes.Equal(data, fresh) {
				t.Fatal("overwrite did not land")
			}
		})
	}
}

// Regression: a file within quota overwritten through the engine
// with the REAL history manager still lands in version history
// (pre-005 behavior, including job 002's rules inside Archive).
func TestArchiveWithinQuotaRealHistory(t *testing.T) {
	n := newTestNode(t)
	old := []byte("version one content, soon to be history")
	seedLiveEntry(t, n, "small.txt", old)
	n.eng.cfg.VersionHistoryQuotaBytes = 1 << 20
	// archiveFn stays nil: the real history.Manager runs.

	pullOverwrite(t, n, "small.txt", []byte("version two content"))

	vers, err := n.hist.ListVersions("small.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(vers) != 1 || vers[0].Size != int64(len(old)) {
		t.Fatalf("ListVersions = %+v, want exactly the old %d-byte version", vers, len(old))
	}
	if count, _ := n.eng.SkippedArchives(); count != 0 {
		t.Fatalf("skip count = %d, want 0 for an in-quota archive", count)
	}
}

// Spec D5 anti-spam: repeated skips of one path log the warning
// once; a successful archive of that path re-arms the warning, so
// the next skip logs again. The counter and lastSkip keep updating
// on every skip.
func TestArchiveSkipWarnOnceUntilRearmed(t *testing.T) {
	n := newTestNode(t)
	var buf bytes.Buffer
	cfg := n.cfg
	cfg.VersionHistoryQuotaBytes = 50
	eng := NewEngine(cfg, n.id, n.st, n.hist, logx.New(&buf, logx.LevelDebug), Options{})
	calls := 0
	eng.archiveFn = func(rel string) (string, error) { calls++; return "v", nil }
	warns := func() int {
		return strings.Count(buf.String(), "version archive skipped for large file")
	}

	writeFile(t, n.root, "f.bin", pattern(100, 3), 0o644)
	if _, err := eng.archive("f.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.archive("f.bin"); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("archiveFn called %d times during skips, want 0", calls)
	}
	if count, last := eng.SkippedArchives(); count != 2 || last == nil || last.Size != 100 {
		t.Fatalf("after 2 skips: count/last = %d/%+v, want 2/{f.bin 100}", count, last)
	}
	if warns() != 1 {
		t.Fatalf("warn lines after 2 skips of one path = %d, want 1", warns())
	}

	// Shrink below the quota: the archive succeeds and re-arms.
	writeFile(t, n.root, "f.bin", pattern(10, 3), 0o644)
	if _, err := eng.archive("f.bin"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("archiveFn calls = %d, want 1 (in-quota archive)", calls)
	}

	// Grow past the quota again: the next skip must warn again.
	writeFile(t, n.root, "f.bin", pattern(100, 3), 0o644)
	if _, err := eng.archive("f.bin"); err != nil {
		t.Fatal(err)
	}
	if count, _ := eng.SkippedArchives(); count != 3 {
		t.Fatalf("skip count = %d, want 3", count)
	}
	if warns() != 2 {
		t.Fatalf("warn lines after re-armed skip = %d, want 2", warns())
	}
}
