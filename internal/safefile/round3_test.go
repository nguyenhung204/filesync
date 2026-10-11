package safefile

import (
	"os"
	"path/filepath"
	"testing"
)

// Round-3 regression (security round-2 BLOCKER): PathWithinRoot fell
// back to a purely lexical judgement whenever EvalSymlinks(target)
// failed — which happens for EVERY not-yet-existing target — so a
// nonexistent target below a symlinked directory escaping the root was
// wrongly reported as inside. The fallback must resolve the deepest
// existing ancestor (evalExisting), like Resolve does.

// (a) Nonexistent target below a symlinked dir escaping the root → false.
func TestRound3_PathWithinRootNonexistentUnderSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "link", "brand-new.txt")
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("precondition: target must not exist (lstat err = %v)", err)
	}
	if PathWithinRoot(root, target) {
		t.Fatalf("PathWithinRoot(%q, %q) = true, want false: target resolves outside root through symlinked parent", root, target)
	}
}

// (b) Nonexistent target below a real directory inside the root → true.
func TestRound3_PathWithinRootNonexistentUnderRealDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "real", "brand-new.txt")
	if PathWithinRoot(root, target) != true {
		t.Fatalf("PathWithinRoot(%q, %q) = false, want true: target is a new file under a real in-root directory", root, target)
	}
}

// (c) Nonexistent target whose parent chain does not exist yet either
// (multi-segment tail) inside the root → true; the same shape with a
// symlink at a middle component of the tail's existing prefix → false.
func TestRound3_PathWithinRootNonexistentDeepTail(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	deep := filepath.Join(root, "a", "b", "c.txt")
	if PathWithinRoot(root, deep) != true {
		t.Fatalf("PathWithinRoot(%q, %q) = false, want true: multi-segment nonexistent tail under root", root, deep)
	}

	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	viaLink := filepath.Join(root, "link", "x", "y.txt")
	if PathWithinRoot(root, viaLink) {
		t.Fatalf("PathWithinRoot(%q, %q) = true, want false: middle component is a symlink escaping root", root, viaLink)
	}
}

// Lock-in (already correct before the fix): an existing file outside
// the root → false, and the root itself / an existing in-root file → true.
func TestRound3_PathWithinRootExistingPaths(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "existing.txt")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if PathWithinRoot(root, outsideFile) {
		t.Fatalf("PathWithinRoot(%q, %q) = true, want false: existing file outside root", root, outsideFile)
	}
	insideFile := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(insideFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !PathWithinRoot(root, insideFile) {
		t.Fatalf("PathWithinRoot(%q, %q) = false, want true: existing file inside root", root, insideFile)
	}
	if !PathWithinRoot(root, root) {
		t.Fatalf("PathWithinRoot(%q, %q) = false, want true: root itself", root, root)
	}
}
