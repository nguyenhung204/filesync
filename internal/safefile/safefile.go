// Package safefile provides the only sanctioned ways to mutate files inside
// a sync root: atomic writes (temp file in the same directory -> fsync ->
// rename -> fsync parent dir), stability checks before hashing, free-space
// pre-checks, and cleanup of leftover temp files after a crash.
//
// No function in this package ever deletes or overwrites user data outside
// the temp-file mechanism: WriteAtomic replaces a file atomically (callers
// archive the previous version via internal/history first), and CleanTemps
// only removes files carrying the FileSync temp prefix.
package safefile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"filesync/internal/validate"
)

// TempPrefix marks every temporary file created by FileSync inside a sync
// root. CleanTemps removes only regular files whose base name has it.
const TempPrefix = ".filesync-tmp-"

// Sentinel errors, matchable with errors.Is.
var (
	// ErrLocked means the file is held exclusively by another process;
	// callers defer and retry instead of treating it as a hard failure.
	ErrLocked = errors.New("safefile: file locked")
	// ErrDiskFull means the destination does not have enough free space;
	// callers skip that one file and keep syncing the rest.
	ErrDiskFull = errors.New("safefile: insufficient disk space")
	// ErrNotStable means size or mtime changed between the two checks.
	ErrNotStable = errors.New("safefile: file not stable")
)

// Resolve joins a validated slash-separated relative path onto root and
// re-checks the result stays inside root (defense in depth on top of
// validate.ValidateRelPath). It returns the OS path.
func Resolve(root, rel string, maxLen int) (string, error) {
	clean, err := validate.ValidateRelPath(rel, maxLen)
	if err != nil {
		return "", fmt.Errorf("safefile: resolve %q: %w", rel, err)
	}
	if clean == "." {
		return root, nil
	}
	abs := filepath.Join(root, filepath.FromSlash(clean))
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("safefile: resolve root: %w", err)
	}
	absClean, err := filepath.Abs(abs)
	if err != nil {
		return "", fmt.Errorf("safefile: resolve %q: %w", rel, err)
	}
	if absClean != rootAbs && !strings.HasPrefix(absClean, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("safefile: resolve %q: path escapes root", rel)
	}
	return abs, nil
}

// IsLockedErr classifies OS errors that mean "another process holds the
// file": EBUSY / ETXTBSY on Unix, sharing violations surfaced as EACCES or
// EPERM on some platforms while a writer is active.
func IsLockedErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrLocked) {
		return true
	}
	return errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.ETXTBSY)
}

// wrapLocked converts a locked-class OS error into one wrapping ErrLocked.
func wrapLocked(op string, err error) error {
	if IsLockedErr(err) {
		return fmt.Errorf("safefile: %s: %w (%v)", op, ErrLocked, err)
	}
	return fmt.Errorf("safefile: %s: %w", op, err)
}

// WriteAtomic replaces root/rel with the content read from r, atomically:
// the data goes to a temp file in the destination directory, is fsynced,
// renamed over the target, and the parent directory is fsynced. A crash at
// any point leaves either the old file fully intact or the new file fully
// in place — never a truncated target. mode sets the permission bits of
// the new file (including the executable bit on Unix).
func WriteAtomic(root, rel string, r io.Reader, mode os.FileMode) error {
	return writeAtomic(root, rel, r, mode, validate.DefaultMaxLen)
}

// WriteAtomicMaxLen is WriteAtomic with a caller-configured path length
// limit (from config.MaxPathLength).
func WriteAtomicMaxLen(root, rel string, r io.Reader, mode os.FileMode, maxLen int) error {
	return writeAtomic(root, rel, r, mode, maxLen)
}

func writeAtomic(root, rel string, r io.Reader, mode os.FileMode, maxLen int) error {
	dst, err := Resolve(root, rel, maxLen)
	if err != nil {
		return err
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return wrapLocked("mkdir parent", err)
	}
	tmp, err := os.CreateTemp(dir, TempPrefix+"*")
	if err != nil {
		return wrapLocked("create temp", err)
	}
	tmpName := tmp.Name()
	// Any failure before the rename must not leave the temp file behind;
	// after a successful rename tmpName no longer exists and Remove is a
	// harmless no-op.
	defer os.Remove(tmpName)

	if err := tmp.Chmod(mode.Perm()); err != nil {
		tmp.Close()
		return fmt.Errorf("safefile: chmod temp: %w", err)
	}
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return fmt.Errorf("safefile: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("safefile: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("safefile: close temp: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return wrapLocked("rename temp", err)
	}
	syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so a rename inside it survives a crash.
// Best-effort: platforms that cannot fsync directories simply skip it.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// StatFunc abstracts the stat used by stability checks so tests can inject
// a changing file without sleeping. It returns size and mtime (UnixNano).
type StatFunc func(path string) (size int64, mtimeNs int64, err error)

// SleepFunc abstracts the wait between the two stability checks.
type SleepFunc func(d time.Duration)

// DefaultStat is the StatFunc backed by os.Stat.
func DefaultStat(path string) (int64, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	return info.Size(), info.ModTime().UnixNano(), nil
}

// Stability is the injectable configuration for CheckStableWith.
type Stability struct {
	Interval time.Duration // wait between the two checks (domain rule: 1s)
	Stat     StatFunc      // nil -> DefaultStat
	Sleep    SleepFunc     // nil -> time.Sleep
}

// CheckStable reports whether the file at path kept the same size and mtime
// across two checks 1 second apart (the domain stability rule). A file
// that is still being written by another application is not stable and
// must not be hashed or synced yet.
func CheckStable(path string) (bool, error) {
	return CheckStableWith(path, Stability{Interval: time.Second})
}

// CheckStableWith is CheckStable with injectable interval/stat/sleeper.
// It returns (false, nil) for a merely unstable file, (false, ErrNotStable
// wrapped) is NOT used: instability is a normal outcome, not an error.
// Errors are reserved for stat failures (missing file, permissions).
func CheckStableWith(path string, st Stability) (bool, error) {
	stat := st.Stat
	if stat == nil {
		stat = DefaultStat
	}
	sleep := st.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	size1, mtime1, err := stat(path)
	if err != nil {
		if IsLockedErr(err) {
			return false, fmt.Errorf("safefile: check stable %q: %w (%v)", path, ErrLocked, err)
		}
		return false, fmt.Errorf("safefile: check stable %q: %w", path, err)
	}
	if st.Interval > 0 {
		sleep(st.Interval)
	}
	size2, mtime2, err := stat(path)
	if err != nil {
		return false, fmt.Errorf("safefile: check stable %q: %w", path, err)
	}
	return size1 == size2 && mtime1 == mtime2, nil
}

// RequireSpace returns an error wrapping ErrDiskFull when the filesystem
// hosting path has fewer than size free bytes available. Callers check
// before receiving a file so a full disk stops that one file and leaves
// its previous state untouched. size <= 0 always passes.
func RequireSpace(path string, size int64) error {
	if size <= 0 {
		return nil
	}
	free, err := FreeSpace(path)
	if err != nil {
		return fmt.Errorf("safefile: require space: %w", err)
	}
	if free < size {
		return fmt.Errorf("safefile: require space: need %d bytes, %d free: %w", size, free, ErrDiskFull)
	}
	return nil
}

// CleanTemps removes leftover FileSync temp files (regular files whose
// base name starts with TempPrefix) anywhere under root, returning how
// many were removed. It runs at startup after a crash (Flow D): because
// renames are atomic, a leftover temp never corresponds to a truncated
// target — the target is either fully old or fully new. Directories and
// symlinks are never removed, and the .filesync metadata tree is skipped
// (it manages its own files).
func CleanTemps(root string) (int, error) {
	removed := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".filesync" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if strings.HasPrefix(d.Name(), TempPrefix) {
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("safefile: clean temp %q: %w", p, err)
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("safefile: clean temps under %q: %w", root, err)
	}
	return removed, nil
}
