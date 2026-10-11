// Package history owns the two data-preservation mechanisms inside a sync
// root: version history under .filesync/versions (the last N versions of
// each file, bounded by a total quota) and the trash under
// .filesync/trash (deleted files kept with a tombstone, purged after a
// 30-day TTL). Nothing in this package ever destroys the only copy of user
// data: overwrites archive first, deletes move to trash, and purges only
// remove FileSync-owned copies whose retention window has expired.
package history

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filesync/internal/safefile"
	"filesync/internal/store"
	"filesync/internal/validate"
)

// MetaDir is the FileSync metadata directory inside a sync root.
const MetaDir = ".filesync"

// TombstoneTTL is how long deleted files and their tombstones are kept
// before PurgeExpired may remove them.
const TombstoneTTL = 30 * 24 * time.Hour

// Manager operates on one sync root. Now and the rename hook are
// injectable so tests control time (TTL, timestamps in names) and can
// simulate locked files without depending on the wall clock or OS locks.
type Manager struct {
	Root       string
	Store      *store.Store // may be nil: tombstone/trash-bookkeeping calls then fail with an error, file ops still work where noted
	Keep       int          // versions kept per file (default 5)
	QuotaBytes int64        // total cap for the versions tree; 0 = unlimited
	MaxPathLen int          // 0 -> validate.DefaultMaxLen

	Now    func() time.Time            // nil -> time.Now
	Rename func(old, new string) error // nil -> os.Rename; test hook for locked files
}

// NewManager builds a Manager with the domain defaults (keep 5 versions).
func NewManager(root string, st *store.Store, keep int, quotaBytes int64) *Manager {
	if keep <= 0 {
		keep = 5
	}
	return &Manager{Root: root, Store: st, Keep: keep, QuotaBytes: quotaBytes}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) rename(old, new string) error {
	if m.Rename != nil {
		return m.Rename(old, new)
	}
	return os.Rename(old, new)
}

func (m *Manager) maxLen() int {
	if m.MaxPathLen > 0 {
		return m.MaxPathLen
	}
	return validate.DefaultMaxLen
}

// versionsDirFor maps rel "a/b.txt" to <root>/.filesync/versions/a/b.txt,
// a directory holding one file per archived version.
func (m *Manager) versionsDirFor(rel string) (string, error) {
	clean, err := validate.ValidateRelPath(rel, m.maxLen())
	if err != nil {
		return "", fmt.Errorf("history: %w", err)
	}
	return filepath.Join(m.Root, MetaDir, "versions", filepath.FromSlash(clean)), nil
}

func (m *Manager) trashDir() string {
	return filepath.Join(m.Root, MetaDir, "trash")
}

// VersionInfo describes one archived version of a file.
type VersionInfo struct {
	Name    string // base name inside the version directory (starts with a UnixNano timestamp)
	Rel     string // original slash path of the file
	Size    int64
	ModTime time.Time
}

// stampName builds "<unixnano>-<basename>", appending -2, -3, ... while
// the name is taken, so names are deterministic and sort chronologically.
func stampName(dir, base string, ts time.Time) string {
	stamp := fmt.Sprintf("%d-%s", ts.UnixNano(), base)
	name := stamp
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name
		}
		name = fmt.Sprintf("%s-%d", stamp, i)
	}
}

// Archive copies the current content of root/rel into version history
// BEFORE it is overwritten (Flow B step 4), then enforces the per-file
// keep count and the global quota by dropping the oldest versions —
// which are FileSync-owned copies, never the live file. A missing file
// is not an error: ("", nil) means there was nothing to archive.
func (m *Manager) Archive(rel string) (string, error) {
	src, err := safefile.Resolve(m.Root, rel, m.maxLen())
	if err != nil {
		return "", err
	}
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("history: archive %q: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("history: archive %q: not a regular file", rel)
	}
	dir, err := m.versionsDirFor(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("history: archive %q: mkdir: %w", rel, err)
	}
	clean, _ := validate.ValidateRelPath(rel, m.maxLen())
	name := stampName(dir, filepath.Base(filepath.FromSlash(clean)), m.now())
	dst := filepath.Join(dir, name)

	// Copy via a temp + rename inside the versions dir so a crash cannot
	// leave a half-written version that later counts as a good copy.
	f, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("history: archive %q: %w", rel, err)
	}
	defer f.Close()
	tmp, err := os.CreateTemp(dir, ".filesync-tmp-*")
	if err != nil {
		return "", fmt.Errorf("history: archive %q: %w", rel, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.ReadFrom(f); err != nil {
		tmp.Close()
		return "", fmt.Errorf("history: archive %q: copy: %w", rel, err)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return "", fmt.Errorf("history: archive %q: %w", rel, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("history: archive %q: %w", rel, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("history: archive %q: %w", rel, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", fmt.Errorf("history: archive %q: %w", rel, err)
	}
	// Preserve the original mtime on the copy: version ordering uses the
	// name stamp, but restores should carry the archived file's times.
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())

	if err := m.pruneKeep(rel); err != nil {
		return "", err
	}
	if err := m.enforceQuota(dst); err != nil {
		return "", err
	}
	return MetaDir + "/versions/" + clean + "/" + name, nil
}

// listVersionFiles returns the version file names for rel, oldest first
// (name order == time order thanks to the UnixNano prefix).
func (m *Manager) listVersionFiles(rel string) ([]string, error) {
	dir, err := m.versionsDirFor(rel)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("history: list versions %q: %w", rel, err)
	}
	names := []string{}
	for _, e := range ents {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".filesync-tmp-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ListVersions returns metadata for every archived version of rel,
// oldest first.
func (m *Manager) ListVersions(rel string) ([]VersionInfo, error) {
	dir, err := m.versionsDirFor(rel)
	if err != nil {
		return nil, err
	}
	names, err := m.listVersionFiles(rel)
	if err != nil {
		return nil, err
	}
	out := []VersionInfo{}
	for _, n := range names {
		info, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			return nil, fmt.Errorf("history: list versions %q: %w", rel, err)
		}
		out = append(out, VersionInfo{Name: n, Rel: rel, Size: info.Size(), ModTime: info.ModTime()})
	}
	return out, nil
}

func (m *Manager) pruneKeep(rel string) error {
	names, err := m.listVersionFiles(rel)
	if err != nil {
		return err
	}
	dir, err := m.versionsDirFor(rel)
	if err != nil {
		return err
	}
	for len(names) > m.Keep {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			return fmt.Errorf("history: prune version %q: %w", names[0], err)
		}
		names = names[1:]
	}
	return nil
}

// enforceQuota deletes the globally oldest version files until the
// versions tree fits the quota. The file at protect (just archived) is
// never deleted by this call, and a tree containing only it is left alone
// even if it alone exceeds the quota — dropping the newest copy to satisfy
// an accounting rule would defeat the purpose of archiving it.
func (m *Manager) enforceQuota(protect string) error {
	if m.QuotaBytes <= 0 {
		return nil
	}
	versionsRoot := filepath.Join(m.Root, MetaDir, "versions")
	for {
		var total int64
		var oldestPath string
		var oldestName string
		err := filepath.WalkDir(versionsRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
			if p != protect && (oldestPath == "" || d.Name() < oldestName) {
				oldestPath, oldestName = p, d.Name()
			}
			return nil
		})
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("history: enforce quota: %w", err)
		}
		if total <= m.QuotaBytes || oldestPath == "" {
			return nil
		}
		if err := os.Remove(oldestPath); err != nil {
			return fmt.Errorf("history: enforce quota: remove %q: %w", oldestPath, err)
		}
	}
}

// Restore puts an archived version back at its original path, atomically,
// archiving the current live file first so a restore is itself undoable.
// version is a Name from ListVersions.
func (m *Manager) Restore(rel, version string) error {
	if err := validate.ValidateSegment(version); err != nil {
		return fmt.Errorf("history: restore %q: invalid version name: %w", rel, err)
	}
	dir, err := m.versionsDirFor(rel)
	if err != nil {
		return err
	}
	src := filepath.Join(dir, version)
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("history: restore %q version %q: %w", rel, version, err)
	}
	if _, err := m.Archive(rel); err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("history: restore %q: %w", rel, err)
	}
	defer f.Close()
	if err := safefile.WriteAtomicMaxLen(m.Root, rel, f, info.Mode(), m.maxLen()); err != nil {
		return fmt.Errorf("history: restore %q: %w", rel, err)
	}
	return nil
}

// sidecar is the JSON metadata stored next to each trashed file.
type sidecar struct {
	Path      string          `json:"path"`
	TrashedTs int64           `json:"trashed_ts"` // UnixNano
	Size      int64           `json:"size"`
	UpdatedBy string          `json:"updated_by"`
	Vector    json.RawMessage `json:"vector,omitempty"`
}
