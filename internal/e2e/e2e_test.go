//go:build e2e

package e2e_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// TestE2EThreeNodeConverge: a full mesh of three daemons;
// create/edit/rename/delete at A must land identically on B and C
// within 10s of the last operation, and the deleted file must be
// in B's and C's trash (never unlinked outright).
func TestE2EThreeNodeConverge(t *testing.T) {
	a := newNode(t, "node-a")
	b := newNode(t, "node-b")
	c := newNode(t, "node-c")
	for _, n := range []*node{a, b, c} {
		n.start()
		defer n.stop()
	}
	pairMesh(t, a, b, c)

	a.writeFile("f1.txt", []byte("hello v1"))
	a.writeFile("dir/nested.txt", []byte("nested"))
	a.writeFile("to-delete.txt", []byte("delete me"))
	waitTreeEqual(t, 10*time.Second, a, b, c)

	lastOp := time.Now()
	// Edit.
	a.writeFile("f1.txt", []byte("hello v2 — edited"))
	// Rename.
	if err := os.Rename(filepath.Join(a.syncDir, "dir", "nested.txt"), filepath.Join(a.syncDir, "dir", "moved.txt")); err != nil {
		t.Fatal(err)
	}
	// Delete.
	if err := os.Remove(filepath.Join(a.syncDir, "to-delete.txt")); err != nil {
		t.Fatal(err)
	}
	waitTreeEqual(t, 10*time.Second, a, b, c)
	if elapsed := time.Since(lastOp); elapsed > 10*time.Second {
		t.Fatalf("convergence after last op took %v (> 10s budget)", elapsed)
	}
	// The deleted file lives in the trash of B and C.
	for _, n := range []*node{b, c} {
		found := false
		_ = filepath.Walk(filepath.Join(n.syncDir, ".filesync", "trash"), func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				found = true
			}
			return nil
		})
		if !found {
			t.Fatalf("node %s: deleted file not found in .filesync/trash", n.name)
		}
	}
}

// pairMesh pairs A-B, A-C, B-C (the second of each pair dials).
func pairMesh(t *testing.T, a, b, c *node) {
	t.Helper()
	pairNodes(t, a, b) // b dials a
	pairNodes(t, a, c) // c dials a
	pairNodes(t, b, c) // c dials b
}

// TestE2EConflictOffline: B is stopped; both sides edit the same
// file differently; after B restarts there must be exactly ONE
// conflict copy in BOTH trees, holding the loser's bytes, while
// the canonical path holds the D3 winner's bytes.
func TestE2EConflictOffline(t *testing.T) {
	a := newNode(t, "node-a")
	b := newNode(t, "node-b")
	a.start()
	b.start()
	defer a.stop()
	pairNodes(t, a, b)

	a.writeFile("shared.txt", []byte("base content"))
	waitTreeEqual(t, 10*time.Second, a, b)

	b.stop()
	a.writeFile("shared.txt", []byte("content X written by A while B was down"))
	// B edits the same file on disk while its daemon is down.
	b.writeFile("shared.txt", []byte("content Y written by B while it was down"))
	b.start()
	defer b.stop()

	conflictRe := regexp.MustCompile(`^shared \(conflict ([0-9A-Za-z]{8}) \d{8}T\d{6}Z\)\.txt$`)
	findConflict := func(n *node) (string, bool) {
		tree := n.treeHashes()
		for p := range tree {
			if conflictRe.MatchString(p) {
				return p, true
			}
		}
		return "", false
	}
	deadline := time.Now().Add(20 * time.Second)
	var cpA, cpB string
	for time.Now().Before(deadline) {
		var okA, okB bool
		cpA, okA = findConflict(a)
		cpB, okB = findConflict(b)
		if okA && okB && cpA == cpB {
			ta, tb := a.treeHashes(), b.treeHashes()
			if len(ta) == len(tb) && ta["shared.txt"] == tb["shared.txt"] && ta[cpA] == tb[cpB] {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if cpA == "" || cpA != cpB {
		t.Fatalf("conflict copies diverged: A=%q B=%q (trees A=%v B=%v)", cpA, cpB, a.treeHashes(), b.treeHashes())
	}
	// Exactly one conflict copy per tree.
	for _, n := range []*node{a, b} {
		count := 0
		for p := range n.treeHashes() {
			if conflictRe.MatchString(p) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("node %s has %d conflict copies, want exactly 1", n.name, count)
		}
	}
	canonA, _ := a.readFile("shared.txt")
	confA, _ := a.readFile(cpA)
	canonB, _ := b.readFile("shared.txt")
	confB, _ := b.readFile(cpB)
	if !bytes.Equal(canonA, canonB) || !bytes.Equal(confA, confB) {
		t.Fatal("A and B disagree on the resolution")
	}
	x := []byte("content X written by A while B was down")
	y := []byte("content Y written by B while it was down")
	got := map[string]bool{string(canonA): true, string(confA): true}
	if !got[string(x)] || !got[string(y)] {
		t.Fatalf("contents lost: canonical=%q conflict=%q (want X and Y both present)", canonA, confA)
	}
	// D3 winner: the author with the larger device ID wins the
	// canonical path; the loser is named in the conflict file.
	m := conflictRe.FindStringSubmatch(cpA)
	loserShort := m[1]
	// Winner = the larger device ID keeps the canonical path with
	// its content; the loser's short ID names the conflict copy.
	var wantCanon []byte
	var wantLoserShort string
	if a.deviceID > b.deviceID {
		wantCanon, wantLoserShort = x, b.deviceID[:8]
	} else {
		wantCanon, wantLoserShort = y, a.deviceID[:8]
	}
	if !bytes.Equal(canonA, wantCanon) {
		t.Fatalf("canonical = %q, want winner content %q (A=%s B=%s)", canonA, wantCanon, a.deviceID, b.deviceID)
	}
	if loserShort != wantLoserShort {
		t.Fatalf("conflict name loser = %q, want %q", loserShort, wantLoserShort)
	}
}
