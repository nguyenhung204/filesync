// Package index observes a sync root: Scanner produces a deterministic
// snapshot of the tree (entries, changes versus the stored snapshot, and
// per-file skips) and Watcher turns fsnotify events into debounced change
// notifications with a throttled full-rescan fallback on overflow.
//
// The scanner is READ-ONLY with respect to both the store and the tree:
// it never upserts entries, bumps vectors, or creates tombstones. Those
// decisions belong to the caller (sync engine / daemon), which owns the
// ordering rules; the scanner only reports what it saw.
package index

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filesync/internal/chunk"
	"filesync/internal/ignore"
	"filesync/internal/safefile"
	"filesync/internal/store"
	"filesync/internal/validate"
)

// FileEntry is the shared entry type, defined once in internal/store.
type FileEntry = store.Entry

// ChangeKind classifies a difference between the previous stored snapshot
// and the tree as scanned now.
type ChangeKind int

const (
	ChangeCreate ChangeKind = iota
	ChangeModify
	ChangeDelete
	ChangeRename
)

func (k ChangeKind) String() string {
	switch k {
	case ChangeCreate:
		return "create"
	case ChangeModify:
		return "modify"
	case ChangeDelete:
		return "delete"
	case ChangeRename:
		return "rename"
	}
	return "unknown"
}

// Change is one detected difference. For ChangeRename, Path is the new
// path and OldPath the previous one; the content (hash) is identical, so
// a rename never requires re-transferring data.
type Change struct {
	Kind    ChangeKind
	Path    string
	OldPath string
}

// Skip records one path the scanner could not index, with the reason.
// One bad file never fails a scan; only a bad root does.
type Skip struct {
	Path   string
	Reason string // stable machine-readable tag: "unstable", "symlink_escape", ...
	Err    error
}

// ScanResult is the deterministic output of one Scan: every slice is
// sorted (Entries/Changes/Skips by path) so callers and tests can compare
// results exactly.
type ScanResult struct {
	Entries []FileEntry
	Changes []Change
	Skipped []Skip
}

// HashFunc hashes one file; injectable so tests can count hash calls and
// prove the unchanged-file fast path does not re-hash.
type HashFunc func(path string) ([]chunk.ChunkMeta, int64, error)

// Options configures a Scanner.
type Options struct {
	MaxPathLen int // 0 -> validate.DefaultMaxLen

	// Stability checking: when enabled, Scan stats every candidate file
	// twice, one Interval apart (a single wait for the whole tree, not
	// per file), and files that changed in between are skipped as
	// "unstable" instead of being hashed mid-write.
	StabilityCheck bool
	Interval       time.Duration       // 0 -> 1s when StabilityCheck is on
	Sleep          func(time.Duration) // nil -> time.Sleep
	Stat           safefile.StatFunc   // nil -> safefile.DefaultStat

	Hash HashFunc // nil -> chunk.HashFile
}

// Scanner scans one root against the snapshot held in a store.
type Scanner struct {
	root string
	st   *store.Store
	opts Options
}

// NewScanner builds a Scanner. The store is only read (ListEntries) —
// Scan never writes to it.
func NewScanner(root string, st *store.Store, opts Options) *Scanner {
	if opts.MaxPathLen <= 0 {
		opts.MaxPathLen = validate.DefaultMaxLen
	}
	if opts.Hash == nil {
		opts.Hash = chunk.HashFile
	}
	if opts.Stat == nil {
		opts.Stat = safefile.DefaultStat
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	return &Scanner{root: root, st: st, opts: opts}
}

// candidate is one regular file found during the walk, before hashing.
type candidate struct {
	rel     string
	abs     string
	size    int64
	mtimeNs int64
	mode    fs.FileMode
}

// Scan walks the tree and diff against the stored snapshot. See the
// package comment and Flow A of the job spec for the exact semantics.
func (s *Scanner) Scan() (ScanResult, error) {
	rootInfo, err := os.Stat(s.root)
	if err != nil {
		return ScanResult{}, fmt.Errorf("index: scan root %q: %w", s.root, err)
	}
	if !rootInfo.IsDir() {
		return ScanResult{}, fmt.Errorf("index: scan root %q: not a directory", s.root)
	}
	var snapshot map[string]FileEntry
	if s.st != nil {
		entries, err := s.st.ListEntries()
		if err != nil {
			return ScanResult{}, fmt.Errorf("index: scan: read snapshot: %w", err)
		}
		snapshot = make(map[string]FileEntry, len(entries))
		for _, e := range entries {
			if !e.Tombstone {
				snapshot[e.Path] = e
			}
		}
	}

	cands, skipped := s.walk()
	if s.opts.StabilityCheck {
		cands, skipped = s.filterStable(cands, skipped)
	}

	entries := []FileEntry{}
	current := make(map[string]FileEntry, len(cands))
	for _, c := range cands {
		e, err := s.buildEntry(c, snapshot)
		if err != nil {
			skipped = append(skipped, Skip{Path: c.rel, Reason: "hash_error", Err: err})
			continue
		}
		entries = append(entries, e)
		current[e.Path] = e
	}
	changes := diffSnapshots(snapshot, current)
	sortSkips(skipped)
	return ScanResult{Entries: entries, Changes: changes, Skipped: skipped}, nil
}

// buildEntry reuses stored chunks when path+size+mtime all match the
// snapshot (the fast path — no hashing), and hashes otherwise.
func (s *Scanner) buildEntry(c candidate, snapshot map[string]FileEntry) (FileEntry, error) {
	if old, ok := snapshot[c.rel]; ok && old.Size == c.size && old.MtimeNs == c.mtimeNs {
		e := old
		e.Mode = uint32(c.mode.Perm())
		return e, nil
	}
	metas, total, err := s.opts.Hash(c.abs)
	if err != nil {
		return FileEntry{}, fmt.Errorf("hash %q: %w", c.rel, err)
	}
	return FileEntry{
		Path:    c.rel,
		Size:    total,
		MtimeNs: c.mtimeNs,
		Mode:    uint32(c.mode.Perm()),
		Chunks:  metas,
	}, nil
}

type matcherFrame struct {
	dirRel  string // slash path of the directory this matcher belongs to ("" = root)
	matcher *ignore.Matcher
}

// walk collects candidate files, honouring .syncignore files at every
// level, pruning .filesync, and classifying everything it refuses.
func (s *Scanner) walk() ([]candidate, []Skip) {
	cands := []candidate{}
	skipped := []Skip{}
	var frames []matcherFrame
	seenByDir := map[string]map[string]string{} // dirRel -> lower-cased name -> first original name
	nonASCIIByDir := map[string][]string{}      // dirRel -> previous non-ASCII names (for NFC/NFD checks)

	rootMatcher, err := ignore.LoadFile(filepath.Join(s.root, ".syncignore"))
	if err == nil && rootMatcher != nil {
		frames = append(frames, matcherFrame{dirRel: "", matcher: rootMatcher})
	}

	_ = filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip it, keep scanning the rest
		}
		if p == s.root {
			return nil
		}
		relOS, err := filepath.Rel(s.root, p)
		if err != nil {
			return nil
		}
		rel := filepath.ToSlash(relOS)
		name := d.Name()

		// Prune FileSync's own metadata tree absolutely.
		if name == ".filesync" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// .syncignore files are per-machine policy, never entries.
		if name == ".syncignore" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Case-collision within this directory (names differing only by
		// case or Unicode form collide on some filesystems).
		dirRel := pathDir(rel)
		seen := seenByDir[dirRel]
		if seen == nil {
			seen = map[string]string{}
			seenByDir[dirRel] = seen
		}
		folded := foldName(name)
		collision := ""
		if prev, exists := seen[folded]; exists && prev != name {
			collision = prev
		}
		// NFC vs NFD forms of the same name differ in raw bytes and in
		// simple case folding; they only occur in non-ASCII names, so the
		// pairwise canonical comparison runs just for those.
		if collision == "" && !isASCII(name) {
			for _, prev := range nonASCIIByDir[dirRel] {
				if prev != name && validate.PathsCaseEqual(prev, name) {
					collision = prev
					break
				}
			}
			nonASCIIByDir[dirRel] = append(nonASCIIByDir[dirRel], name)
		}
		if collision != "" {
			skipped = append(skipped, Skip{Path: rel, Reason: "case_collision", Err: fmt.Errorf("collides with %q in same directory", collision)})
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		seen[folded] = name

		// Validate the path (reserved names, forbidden chars, length...).
		if _, err := validate.ValidateRelPath(rel, s.opts.MaxPathLen); err != nil {
			skipped = append(skipped, Skip{Path: rel, Reason: "invalid_path", Err: err})
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if s.ignored(frames, rel, true) {
				return filepath.SkipDir
			}
			// Load this directory's own .syncignore for its subtree.
			if m, err := ignore.LoadFile(filepath.Join(p, ".syncignore")); err == nil && m != nil {
				// A matcher that matches nothing still must be pushed so
				// pops stay aligned; LoadFile returns a non-nil empty
				// matcher for missing files too.
				frames = append(frames, matcherFrame{dirRel: rel, matcher: m})
			}
			// WalkDir has no exit hook; frames for siblings are handled
			// by trimming in ignored() based on prefix — frames whose dir
			// is not an ancestor of the current path are stale. Trim now:
			frames = trimFrames(frames, rel)
			return nil
		}

		// Symlinks are never followed. One pointing outside the root is
		// an escape attempt; any other symlink is simply not indexed.
		if d.Type()&fs.ModeSymlink != 0 {
			reason := "symlink"
			if target, err := filepath.EvalSymlinks(p); err == nil {
				if !withinRoot(s.root, target) {
					reason = "symlink_escape"
				}
			}
			skipped = append(skipped, Skip{Path: rel, Reason: reason})
			return nil
		}
		if !d.Type().IsRegular() {
			skipped = append(skipped, Skip{Path: rel, Reason: "not_regular"})
			return nil
		}
		if s.ignored(frames, rel, false) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			skipped = append(skipped, Skip{Path: rel, Reason: "stat_error", Err: err})
			return nil
		}
		cands = append(cands, candidate{
			rel: rel, abs: p,
			size: info.Size(), mtimeNs: info.ModTime().UnixNano(),
			mode: info.Mode(),
		})
		return nil
	})

	sort.Slice(cands, func(i, j int) bool { return cands[i].rel < cands[j].rel })
	return cands, skipped
}

// ignored reports whether rel is ignored by any applicable .syncignore
// matcher: each matcher sees the path relative to its own directory.
func (s *Scanner) ignored(frames []matcherFrame, rel string, isDir bool) bool {
	ignored := false
	for _, f := range trimFrames(frames, rel) {
		sub := rel
		if f.dirRel != "" {
			if rel == f.dirRel {
				sub = ""
			} else if strings.HasPrefix(rel, f.dirRel+"/") {
				sub = rel[len(f.dirRel)+1:]
			} else {
				continue
			}
		}
		if sub == "" {
			continue
		}
		if f.matcher.Match(sub, isDir) {
			ignored = true
		}
	}
	return ignored
}

// trimFrames drops matcher frames whose directory is not an ancestor of
// rel (they belonged to a subtree the walk has left). The root frame is
// always kept.
func trimFrames(frames []matcherFrame, rel string) []matcherFrame {
	out := frames[:0]
	for _, f := range frames {
		if f.dirRel == "" || rel == f.dirRel || strings.HasPrefix(rel, f.dirRel+"/") {
			out = append(out, f)
		}
	}
	return out
}

func pathDir(rel string) string {
	i := strings.LastIndex(rel, "/")
	if i < 0 {
		return ""
	}
	return rel[:i]
}

func foldName(name string) string {
	return strings.ToLower(name)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func withinRoot(root, target string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	tAbs, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	return tAbs == rootAbs || strings.HasPrefix(tAbs, rootAbs+string(filepath.Separator))
}

// filterStable performs the second stat pass after one interval for the
// whole tree: files whose size or mtime moved are still being written and
// are skipped as unstable instead of being hashed mid-write.
func (s *Scanner) filterStable(cands []candidate, skipped []Skip) ([]candidate, []Skip) {
	interval := s.opts.Interval
	if interval <= 0 {
		interval = time.Second
	}
	s.opts.Sleep(interval)
	kept := cands[:0]
	for _, c := range cands {
		size, mtimeNs, err := s.opts.Stat(c.abs)
		if err != nil {
			skipped = append(skipped, Skip{Path: c.rel, Reason: "stat_error", Err: err})
			continue
		}
		if size != c.size || mtimeNs != c.mtimeNs {
			skipped = append(skipped, Skip{Path: c.rel, Reason: "unstable", Err: safefile.ErrNotStable})
			continue
		}
		kept = append(kept, c)
	}
	return kept, skipped
}

func sortSkips(skips []Skip) {
	sort.Slice(skips, func(i, j int) bool { return skips[i].Path < skips[j].Path })
}
