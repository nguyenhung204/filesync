package sync

import (
	"os"
	"testing"

	"filesync/internal/index"
	"filesync/internal/store"
)

func TestCommitSkipsUnstable(t *testing.T) {
	n := newTestNode(t)
	// Preload the store as if gone.txt and keep.txt were committed.
	for _, p := range []string{"gone.txt", "keep.txt"} {
		e := live(p, vv(map[string]uint64{n.id.DeviceID: 1}), n.id.DeviceID, meta(1, 3))
		if err := n.st.UpsertEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	// The scanner reports a delete for gone.txt BUT also lists it
	// as skipped/unstable: commit must not turn that into a delete.
	res := index.ScanResult{
		Changes: []index.Change{{Kind: index.ChangeDelete, Path: "gone.txt"}},
		Skipped: []index.Skip{{Path: "gone.txt", Reason: "unstable"}},
	}
	changed, err := n.eng.CommitScan(res)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("commit reported a change for a skipped delete")
	}
	e, found, err := n.st.GetEntry("gone.txt")
	if err != nil || !found {
		t.Fatalf("entry gone: found=%v err=%v", found, err)
	}
	if e.Tombstone {
		t.Fatal("unstable file was tombstoned — scanner skip was treated as delete")
	}
}

func TestCommitVectorLifecycle(t *testing.T) {
	n := newTestNode(t)
	self := n.id.DeviceID
	vecOf := func(path string) (store.Entry, bool) {
		e, found, err := n.st.GetEntry(path)
		if err != nil {
			t.Fatal(err)
		}
		return e, found
	}
	expectVec := func(path string, want uint64) store.Entry {
		t.Helper()
		e, found := vecOf(path)
		if !found {
			t.Fatalf("no entry for %s", path)
		}
		if got := e.Vector.Get(self); got != want {
			t.Fatalf("%s vector[%s] = %d, want %d (full %v)", path, self, got, want, e.Vector)
		}
		if e.UpdatedBy != self {
			t.Fatalf("%s UpdatedBy = %q, want self", path, e.UpdatedBy)
		}
		return e
	}

	writeFile(t, n.root, "f.txt", []byte("one"), 0o644)
	if !n.scanCommit(t) {
		t.Fatal("create not committed")
	}
	expectVec("f.txt", 1)

	writeFile(t, n.root, "f.txt", []byte("two!"), 0o644)
	n.scanCommit(t)
	expectVec("f.txt", 2)

	// Rename carries the vector to the new path, and leaves a
	// tombstone with the same incremented vector at the old path
	// (the delete half of the rename — without it, peers that have
	// not seen the rename yet would make us pull the old file back).
	if err := os.Rename(n.root+"/f.txt", n.root+"/g.txt"); err != nil {
		t.Fatal(err)
	}
	n.scanCommit(t)
	expectVec("g.txt", 3)
	old := expectVec("f.txt", 3)
	if !old.Tombstone {
		t.Fatal("old path has no tombstone after rename")
	}

	// Delete becomes a tombstone continuing the vector.
	if err := os.Remove(n.root + "/g.txt"); err != nil {
		t.Fatal(err)
	}
	n.scanCommit(t)
	e := expectVec("g.txt", 4)
	if !e.Tombstone || e.TombstoneTs == 0 {
		t.Fatalf("delete not tombstoned: %+v", e)
	}

	// Re-create after delete continues the tombstone's vector.
	writeFile(t, n.root, "g.txt", []byte("back"), 0o644)
	n.scanCommit(t)
	e = expectVec("g.txt", 5)
	if e.Tombstone {
		t.Fatal("recreated file still tombstone")
	}
}

func TestCommitModeOnlyChange(t *testing.T) {
	n := newTestNode(t)
	self := n.id.DeviceID
	writeFile(t, n.root, "run.sh", []byte("#!/bin/sh\n"), 0o644)
	n.scanCommit(t)
	if err := os.Chmod(n.root+"/run.sh", 0o755); err != nil {
		t.Fatal(err)
	}
	if !n.scanCommit(t) {
		t.Fatal("mode-only change was not committed")
	}
	e, found, err := n.st.GetEntry("run.sh")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if e.Mode != 0o755 {
		t.Fatalf("mode = %o, want 755", e.Mode)
	}
	if e.Vector.Get(self) != 2 {
		t.Fatalf("vector = %v, want self=2", e.Vector)
	}
	// A further scan with no changes must not bump anything.
	if n.scanCommit(t) {
		t.Fatal("unchanged tree reported changed")
	}
}
