package index

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesync/internal/chunk"
	"filesync/internal/store"
)

func writeFile(t *testing.T, root, rel, content string) os.FileInfo {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func seedSnapshot(t *testing.T, st *store.Store, root string, rels ...string) {
	t.Helper()
	sc := NewScanner(root, st, Options{})
	res, err := sc.Scan()
	if err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	if len(res.Entries) != len(rels) {
		t.Fatalf("seed scan entries = %d, want %d (%+v)", len(res.Entries), len(rels), res.Skipped)
	}
	for _, e := range res.Entries {
		if err := st.UpsertEntry(e); err != nil {
			t.Fatal(err)
		}
	}
}

func changeKinds(res ScanResult) map[string]ChangeKind {
	out := map[string]ChangeKind{}
	for _, c := range res.Changes {
		out[c.Path] = c.Kind
	}
	return out
}

func TestScanner_FirstScanEntriesAndHashes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "hello")
	writeFile(t, root, "sub/b.txt", "world!")
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	res, err := NewScanner(root, st, Options{}).Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Entries) != 2 || len(res.Skipped) != 0 {
		t.Fatalf("entries=%d skipped=%+v", len(res.Entries), res.Skipped)
	}
	if res.Entries[0].Path != "a.txt" || res.Entries[1].Path != "sub/b.txt" {
		t.Fatalf("entries not sorted: %+v", res.Entries)
	}
	want := sha256.Sum256([]byte("hello"))
	if res.Entries[0].Chunks[0].SHA256 != want {
		t.Fatal("chunk hash for a.txt does not match sha256(content)")
	}
	kinds := changeKinds(res)
	if kinds["a.txt"] != ChangeCreate || kinds["sub/b.txt"] != ChangeCreate {
		t.Fatalf("first scan changes: %+v", res.Changes)
	}
}

func TestScanner_FastPathSkipsHashingUnchangedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "stable content")
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	seedSnapshot(t, st, root, "a.txt")

	hashCalls := 0
	sc := NewScanner(root, st, Options{Hash: func(p string) ([]chunk.ChunkMeta, int64, error) {
		hashCalls++
		return chunk.HashFile(p)
	}})
	res, err := sc.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if hashCalls != 0 {
		t.Fatalf("unchanged file was re-hashed %d times", hashCalls)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("unchanged tree produced changes: %+v", res.Changes)
	}
	// Touch the content (size changes): exactly one hash, one modify.
	writeFile(t, root, "a.txt", "stable content, edited")
	res, err = sc.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if hashCalls != 1 {
		t.Fatalf("hash calls after edit = %d, want 1", hashCalls)
	}
	if changeKinds(res)["a.txt"] != ChangeModify {
		t.Fatalf("changes after edit: %+v", res.Changes)
	}
}

func TestScanner_RenameDetectedByHash(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "old.txt", "same content")
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	seedSnapshot(t, st, root, "old.txt")
	if err := os.Rename(filepath.Join(root, "old.txt"), filepath.Join(root, "new.txt")); err != nil {
		t.Fatal(err)
	}
	res, err := NewScanner(root, st, Options{}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 {
		t.Fatalf("changes = %+v, want exactly one rename", res.Changes)
	}
	c := res.Changes[0]
	if c.Kind != ChangeRename || c.Path != "new.txt" || c.OldPath != "old.txt" {
		t.Fatalf("change = %+v, want rename old.txt -> new.txt", c)
	}
}

func TestScanner_DeleteAndCreateDiff(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keep.txt", "k")
	writeFile(t, root, "del.txt", "d")
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	seedSnapshot(t, st, root, "keep.txt", "del.txt")
	os.Remove(filepath.Join(root, "del.txt"))
	writeFile(t, root, "brand.txt", "new stuff")
	res, err := NewScanner(root, st, Options{}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	kinds := changeKinds(res)
	if kinds["del.txt"] != ChangeDelete || kinds["brand.txt"] != ChangeCreate {
		t.Fatalf("changes: %+v", res.Changes)
	}
	if _, ok := kinds["keep.txt"]; ok {
		t.Fatal("unchanged file reported as changed")
	}
}

func TestScanner_RespectsSyncignoreAndPrunesMeta(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".syncignore", "*.log\nbuild/\n")
	writeFile(t, root, "keep.txt", "k")
	writeFile(t, root, "skip.log", "l")
	writeFile(t, root, "build/out.o", "o")
	writeFile(t, root, "sub/.syncignore", "secret.txt\n")
	writeFile(t, root, "sub/secret.txt", "s")
	writeFile(t, root, "sub/ok.txt", "ok")
	writeFile(t, root, ".filesync/trash/junk.txt", "j")
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	res, err := NewScanner(root, st, Options{}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, e := range res.Entries {
		paths[e.Path] = true
	}
	want := map[string]bool{"keep.txt": true, "sub/ok.txt": true}
	if len(paths) != len(want) {
		t.Fatalf("entries = %v, want %v", paths, want)
	}
	for p := range want {
		if !paths[p] {
			t.Fatalf("missing entry %q (all: %v)", p, paths)
		}
	}
}

func TestScanner_SymlinkEscapeAndInvalidNamesSkipped(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.txt", "s3cret")
	writeFile(t, root, "good.txt", "g")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	writeFile(t, root, "CON.txt", "reserved") // reserved on Windows -> invalid everywhere
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	res, err := NewScanner(root, st, Options{}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Path != "good.txt" {
		t.Fatalf("entries: %+v", res.Entries)
	}
	reasons := map[string]string{}
	for _, sk := range res.Skipped {
		reasons[sk.Path] = sk.Reason
	}
	if reasons["link.txt"] != "symlink_escape" {
		t.Fatalf("symlink skip = %q (all: %v)", reasons["link.txt"], reasons)
	}
	if reasons["CON.txt"] != "invalid_path" {
		t.Fatalf("reserved-name skip = %q", reasons["CON.txt"])
	}
}

func TestScanner_CaseCollisionSkipped(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Readme.md", "one")
	writeFile(t, root, "README.MD", "two")
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	res, err := NewScanner(root, st, Options{}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sk := range res.Skipped {
		if sk.Reason == "case_collision" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no case_collision skip (entries=%+v skipped=%+v)", res.Entries, res.Skipped)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("entries = %d, want 1 (only the first name wins)", len(res.Entries))
	}
}

func TestScanner_UnstableFileSkipped(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "f.txt", "content")
	statCalls := 0
	// The walk stats the file once (size 7 / mtime A); the stability pass
	// stats it again and sees it changed -> unstable.
	stat := func(p string) (int64, int64, error) {
		statCalls++
		return 999, 999, nil
	}
	sc := NewScanner(root, nil, Options{
		StabilityCheck: true,
		Interval:       time.Millisecond,
		Sleep:          func(time.Duration) {},
		Stat:           stat,
	})
	res, err := sc.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Reason != "unstable" {
		t.Fatalf("entries=%+v skipped=%+v", res.Entries, res.Skipped)
	}
	if statCalls == 0 {
		t.Fatal("injected stat was never used by the stability pass")
	}
}

func TestScanner_BadRootFails(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	if _, err := NewScanner(filepath.Join(t.TempDir(), "missing"), st, Options{}).Scan(); err == nil {
		t.Fatal("missing root accepted")
	}
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", "x")
	if _, err := NewScanner(filepath.Join(dir, "file.txt"), st, Options{}).Scan(); err == nil {
		t.Fatal("file-as-root accepted")
	}
}

func TestScanner_ThousandFiles(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 1000; i++ {
		writeFile(t, root, fmt.Sprintf("d%03d/f%03d.txt", i/100, i), fmt.Sprintf("content-%d", i))
	}
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	start := time.Now()
	res, err := NewScanner(root, st, Options{}).Scan()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1000 {
		t.Fatalf("entries = %d, want 1000", len(res.Entries))
	}
	// Verify a sample hash against the content directly.
	var sample *FileEntry
	for i := range res.Entries {
		if res.Entries[i].Path == "d005/f555.txt" {
			sample = &res.Entries[i]
		}
	}
	if sample == nil {
		t.Fatal("sample entry missing")
	}
	want := sha256.Sum256([]byte("content-555"))
	if sample.Chunks[0].SHA256 != want {
		t.Fatal("sample chunk hash mismatch")
	}
	t.Logf("scanned 1000 files in %v", elapsed)
}
