package sync

import (
	"os"
	"path/filepath"
	"testing"

	"filesync/internal/store"
	"filesync/internal/vclock"
)

// setupConflictedNode builds a node whose store+disk hold a resolved
// conflict pair: canonical f.txt (content "winner", author BRAVO)
// and conflict copy (content "loser", author ALPHA), both entries
// carrying the merged vector {ALPHA:1, BRAVO:1}.
func setupConflictedNode(t *testing.T) (*testNode, string) {
	t.Helper()
	n := newTestNode(t)
	merged := vv(map[string]uint64{"ALPHA000": 1, "BRAVO000": 1})
	loser := store.Entry{
		Path: "f.txt", Size: 5, MtimeNs: 1700000000000000000, Mode: 0o644,
		Chunks: metasOf([]byte("loser")), Vector: vv(map[string]uint64{"ALPHA000": 1}), UpdatedBy: "ALPHA000",
	}
	cp := ConflictPath(loser)
	writeFile(t, n.root, "f.txt", []byte("winner"), 0o644)
	writeFile(t, n.root, cp, []byte("loser"), 0o644)
	canon := store.Entry{
		Path: "f.txt", Size: 6, MtimeNs: 1700000001000000000, Mode: 0o644,
		Chunks: metasOf([]byte("winner")), Vector: merged, UpdatedBy: "BRAVO000",
	}
	conf := store.Entry{
		Path: cp, Size: 5, MtimeNs: loser.MtimeNs, Mode: 0o644,
		Chunks: loser.Chunks, Vector: merged, UpdatedBy: "ALPHA000",
	}
	if err := n.st.UpsertEntry(canon); err != nil {
		t.Fatal(err)
	}
	if err := n.st.UpsertEntry(conf); err != nil {
		t.Fatal(err)
	}
	return n, cp
}

func TestResolveConflictKeepMine(t *testing.T) {
	n, cp := setupConflictedNode(t)
	if err := n.eng.ResolveConflict("f.txt", KeepMine); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, n.root, "f.txt"); string(got) != "winner" {
		t.Fatalf("canonical = %q, want winner kept", got)
	}
	if _, err := os.Stat(filepath.Join(n.root, filepath.FromSlash(cp))); !os.IsNotExist(err) {
		t.Fatal("conflict copy still at live path after keep-mine")
	}
	// The loser is in trash, not destroyed.
	trash, err := n.hist.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, te := range trash {
		if te.Path == cp {
			found = true
		}
	}
	if !found {
		t.Fatalf("loser not in trash: %+v", trash)
	}
	e, _, _ := n.st.GetEntry(cp)
	if !e.Tombstone {
		t.Fatal("conflict path has no tombstone after resolve")
	}
	c, _, _ := n.st.GetEntry("f.txt")
	if c.Vector.Get(n.id.DeviceID) != 1 || c.Vector.Compare(vv(map[string]uint64{"ALPHA000": 1, "BRAVO000": 1})) != vclock.Dominates {
		t.Fatalf("canonical vector = %v, want merged+increment(self)", c.Vector)
	}
}

func TestResolveConflictKeepTheirs(t *testing.T) {
	n, cp := setupConflictedNode(t)
	if err := n.eng.ResolveConflict("f.txt", KeepTheirs); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, n.root, "f.txt"); string(got) != "loser" {
		t.Fatalf("canonical = %q, want conflict content kept", got)
	}
	if _, err := os.Stat(filepath.Join(n.root, filepath.FromSlash(cp))); !os.IsNotExist(err) {
		t.Fatal("conflict copy still at live path after keep-theirs")
	}
	// The replaced canonical content is preserved in version history.
	versions, err := n.hist.ListVersions("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatal("replaced canonical content not archived to versions")
	}
	e, _, _ := n.st.GetEntry("f.txt")
	if e.UpdatedBy != "ALPHA000" {
		t.Fatalf("canonical UpdatedBy = %q, want content author ALPHA", e.UpdatedBy)
	}
	if !chunksEqual(e.Chunks, metasOf([]byte("loser"))) {
		t.Fatal("canonical chunks do not match kept content")
	}
}
