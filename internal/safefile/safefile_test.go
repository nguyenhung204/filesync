package safefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSafeFile_WriteAtomicReplacesAndKeepsMode(t *testing.T) {
	root := t.TempDir()
	if err := WriteAtomic(root, "a/b.txt", strings.NewReader("hello"), 0o644); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "a", "b.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("content = %q, %v", data, err)
	}
	// Replace with new content and an executable mode.
	if err := WriteAtomic(root, "a/b.txt", strings.NewReader("v2-longer"), 0o755); err != nil {
		t.Fatalf("WriteAtomic replace: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(root, "a", "b.txt"))
	if string(data) != "v2-longer" {
		t.Fatalf("after replace: %q", data)
	}
	info, _ := os.Stat(filepath.Join(root, "a", "b.txt"))
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 755 (exec bit preserved)", info.Mode().Perm())
	}
	// No temp files left behind.
	leftovers := 0
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if strings.HasPrefix(d.Name(), TempPrefix) {
			leftovers++
		}
		return nil
	})
	if leftovers != 0 {
		t.Fatalf("%d temp leftovers", leftovers)
	}
}

func TestSafeFile_WriteAtomicRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"../evil.txt", "/abs.txt", "a/../../b.txt", `C:\x.txt`} {
		if err := WriteAtomic(root, rel, strings.NewReader("x"), 0o644); err == nil {
			t.Fatalf("WriteAtomic(%q) accepted", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("traversal write escaped the root")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestSafeFile_WriteAtomicFailureLeavesOldFileIntact(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(root, "f.txt", errReader{}, 0o644); err == nil {
		t.Fatal("expected read error")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "original" {
		t.Fatalf("target damaged by failed write: %q", data)
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), TempPrefix) {
			t.Fatalf("temp leftover after failure: %s", e.Name())
		}
	}
}

func TestSafeFile_CheckStableInjectable(t *testing.T) {
	// Injected stat: file grows between the two checks -> not stable;
	// the sleeper is a no-op recording the interval it was asked for.
	var slept time.Duration
	sizes := []int64{10, 11}
	calls := 0
	stat := func(path string) (int64, int64, error) {
		s := sizes[calls]
		calls++
		return s, 42, nil
	}
	stable, err := CheckStableWith("x", Stability{
		Interval: 250 * time.Millisecond,
		Stat:     stat,
		Sleep:    func(d time.Duration) { slept = d },
	})
	if err != nil || stable {
		t.Fatalf("growing file: stable=%v err=%v, want false/nil", stable, err)
	}
	if slept != 250*time.Millisecond {
		t.Fatalf("sleeper got %v, want injected interval", slept)
	}
	stat2 := func(path string) (int64, int64, error) { return 7, 9, nil }
	stable, err = CheckStableWith("x", Stability{Stat: stat2, Sleep: func(time.Duration) {}})
	if err != nil || !stable {
		t.Fatalf("unchanged file: stable=%v err=%v, want true/nil", stable, err)
	}
	// Real file, zero interval: stable.
	p := filepath.Join(t.TempDir(), "real.txt")
	os.WriteFile(p, []byte("data"), 0o644)
	stable, err = CheckStableWith(p, Stability{Interval: 0})
	if err != nil || !stable {
		t.Fatalf("real stable file: %v %v", stable, err)
	}
	if _, err := CheckStableWith(filepath.Join(t.TempDir(), "missing"), Stability{}); err == nil {
		t.Fatal("missing file: want error")
	}
}

func TestSafeFile_RequireSpace(t *testing.T) {
	root := t.TempDir()
	if err := RequireSpace(root, 0); err != nil {
		t.Fatalf("zero size: %v", err)
	}
	if err := RequireSpace(root, 1); err != nil {
		t.Fatalf("1 byte: %v", err)
	}
	if err := RequireSpace(root, 1<<62); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("huge size: %v, want ErrDiskFull", err)
	}
	free, err := FreeSpace(root)
	if err != nil || free <= 0 {
		t.Fatalf("FreeSpace = %d, %v", free, err)
	}
	if err := RequireSpace(filepath.Join(root, "no-such-dir"), 1); err == nil {
		t.Fatal("nonexistent path accepted")
	}
}

func TestSafeFile_CleanTemps(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.MkdirAll(filepath.Join(root, ".filesync", "trash"), 0o755)
	os.WriteFile(filepath.Join(root, TempPrefix+"123"), []byte("partial"), 0o644)
	os.WriteFile(filepath.Join(root, "sub", TempPrefix+"456"), []byte("partial"), 0o644)
	os.WriteFile(filepath.Join(root, "keep.txt"), []byte("real"), 0o644)
	os.WriteFile(filepath.Join(root, ".filesync", "trash", TempPrefix+"notmine"), []byte("x"), 0o644)
	n, err := CleanTemps(root)
	if err != nil || n != 2 {
		t.Fatalf("CleanTemps = %d, %v; want 2", n, err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Fatal("real file removed by CleanTemps")
	}
	if _, err := os.Stat(filepath.Join(root, ".filesync", "trash", TempPrefix+"notmine")); err != nil {
		t.Fatal("CleanTemps descended into .filesync")
	}
}

func TestSafeFile_IsLockedErr(t *testing.T) {
	if !IsLockedErr(syscall.EBUSY) {
		t.Fatal("EBUSY not classified as locked")
	}
	if IsLockedErr(os.ErrNotExist) {
		t.Fatal("not-exist misclassified as locked")
	}
	if IsLockedErr(nil) {
		t.Fatal("nil misclassified as locked")
	}
}
