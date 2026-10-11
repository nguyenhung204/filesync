package safefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"filesync/internal/validate"
)

// Additional sentinels for the symlink-aware containment layer.
var (
	// ErrSymlinkEscape means a path resolves (through a symlinked
	// component) to a location outside the sync root.
	ErrSymlinkEscape = errors.New("safefile: path escapes root through symlink")
	// ErrSymlink means a path that must be followed component-by-component
	// without following links (history sources) contains a symlink.
	ErrSymlink = errors.New("safefile: symlink in path")
)

// Resolve joins a validated slash-separated relative path onto root and
// verifies the result stays inside root. On top of the lexical check
// (defense in depth over validate.ValidateRelPath) it resolves symlinks:
// the deepest existing ancestor of the target is run through
// filepath.EvalSymlinks and the RESOLVED path must still be contained in
// the resolved root, so a symlinked directory planted inside the root
// cannot redirect a write outside it. The returned path is the lexical
// join (callers keep working with the root as configured); containment
// was proven on resolved coordinates.
func Resolve(root, rel string, maxLen int) (string, error) {
	clean, err := validate.ValidateRelPath(rel, maxLen)
	if err != nil {
		return "", fmt.Errorf("safefile: resolve %q: %w", rel, err)
	}
	if clean == "." {
		return root, nil
	}
	joined := filepath.Join(root, filepath.FromSlash(clean))
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("safefile: resolve root: %w", err)
	}
	absClean, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("safefile: resolve %q: %w", rel, err)
	}
	if !withinDir(rootAbs, absClean) {
		return "", fmt.Errorf("safefile: resolve %q: path escapes root", rel)
	}
	resolvedRoot, err := evalPath(rootAbs)
	if err != nil {
		return "", fmt.Errorf("safefile: resolve root: %w", err)
	}
	resolvedTarget := evalExisting(absClean)
	if !withinDir(resolvedRoot, resolvedTarget) {
		return "", fmt.Errorf("safefile: resolve %q: %w", rel, ErrSymlinkEscape)
	}
	return joined, nil
}

// withinDir reports whether p equals dir or lives underneath it. Both
// must be absolute and in the same coordinate system (both lexical or
// both symlink-resolved).
func withinDir(dir, p string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// evalPath returns the symlink-resolved form of an absolute path that
// exists (filepath.EvalSymlinks), falling back to the path itself.
func evalPath(abs string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// evalExisting resolves the deepest existing ancestor of abs with
// EvalSymlinks and re-appends the not-yet-existing remainder lexically
// (components that do not exist cannot be symlinks at this instant).
func evalExisting(abs string) string {
	cur := abs
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Join(parts...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		suffix = append([]string{filepath.Base(cur)}, suffix...)
		cur = parent
	}
}

// PathWithinRoot reports whether target — after resolving symlinks on
// both sides — is the root itself or lives inside it. A target that
// cannot be resolved is judged lexically. Shared with internal/index,
// which classifies symlink entries during a scan with it.
func PathWithinRoot(root, target string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	resolvedRoot, err := evalPath(rootAbs)
	if err != nil {
		return false
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		targetAbs, aerr := filepath.Abs(target)
		if aerr != nil {
			return false
		}
		resolvedTarget = targetAbs
	}
	return withinDir(resolvedRoot, resolvedTarget)
}

// checkContained re-verifies, at the moment of use, that an existing
// directory still resolves inside the resolved root. WriteAtomic calls
// it right before the rename to close the window in which a directory
// checked by Resolve could have been swapped for an escaping symlink.
func checkContained(root, dir string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("safefile: check contained: %w", err)
	}
	resolvedRoot, err := evalPath(rootAbs)
	if err != nil {
		return fmt.Errorf("safefile: check contained: %w", err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("safefile: check contained %q: %w", dir, err)
	}
	if !withinDir(resolvedRoot, resolvedDir) {
		return fmt.Errorf("safefile: directory %q: %w", dir, ErrSymlinkEscape)
	}
	return nil
}

// LstatNoFollow stats abs (which must lie inside root) component by
// component with Lstat, rejecting any symlink along the way, and returns
// the final component's Lstat info. It is the shared no-follow check for
// paths FileSync itself owns (sources under .filesync), complementing
// RequireRegularFile for caller-supplied relative paths.
func LstatNoFollow(root, abs string) (fs.FileInfo, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("safefile: lstat %q: %w", abs, err)
	}
	absClean, err := filepath.Abs(abs)
	if err != nil {
		return nil, fmt.Errorf("safefile: lstat %q: %w", abs, err)
	}
	if !withinDir(rootAbs, absClean) {
		return nil, fmt.Errorf("safefile: lstat %q: path escapes root", abs)
	}
	rel, err := filepath.Rel(rootAbs, absClean)
	if err != nil {
		return nil, fmt.Errorf("safefile: lstat %q: %w", abs, err)
	}
	if rel == "." {
		info, err := os.Lstat(absClean)
		if err != nil {
			return nil, fmt.Errorf("safefile: lstat %q: %w", abs, err)
		}
		return info, nil
	}
	cur := rootAbs
	var info fs.FileInfo
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, err = os.Lstat(cur)
		if err != nil {
			return nil, fmt.Errorf("safefile: lstat %q: %w", abs, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("safefile: lstat %q: component %q: %w", abs, part, ErrSymlink)
		}
	}
	return info, nil
}

// RequireRegularFile resolves rel inside root and proves the source is a
// genuine regular file reached without following any symlink: Resolve
// first (escape through a symlinked directory is rejected), then a
// component-wise Lstat walk that rejects every symlink component,
// including the file itself. History uses it for Archive/MoveToTrash
// sources so user data outside the root can never be copied or moved
// into FileSync's trees through a planted link.
func RequireRegularFile(root, rel string, maxLen int) (string, fs.FileInfo, error) {
	abs, err := Resolve(root, rel, maxLen)
	if err != nil {
		return "", nil, err
	}
	info, err := LstatNoFollow(root, abs)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("safefile: %q: not a regular file", rel)
	}
	return abs, info, nil
}
