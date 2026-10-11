package ignore

import (
	"path"
	"strings"
)

// Match reports whether relPath (a clean, "/" separated relative path as
// returned by internal/validate) is ignored. Patterns are evaluated in file
// order; the last matching pattern wins. A nil Matcher matches nothing.
func (m *Matcher) Match(relPath string, isDir bool) bool {
	if m == nil || relPath == "" || relPath == "." {
		return false
	}
	result := false
	segs := strings.Split(relPath, "/")
	for _, p := range m.patterns {
		if p.matches(segs, isDir) {
			result = !p.negated
		}
	}
	return result
}

func (p pattern) matches(pathSegs []string, isDir bool) bool {
	if p.dirOnly {
		if isDir && p.matchPath(pathSegs) {
			return true
		}
		// A file under an ignored directory is ignored.
		for k := 1; k < len(pathSegs); k++ {
			if p.matchPath(pathSegs[:k]) {
				return true
			}
		}
		return false
	}
	return p.matchPath(pathSegs)
}

// matchPath applies the anchoring rule to a full (sub)path.
func (p pattern) matchPath(pathSegs []string) bool {
	if p.anchored {
		return matchSegments(p.segments, pathSegs)
	}
	// Unanchored: the pattern may match starting at any segment boundary.
	for start := 0; start <= len(pathSegs); start++ {
		if matchSegments(p.segments, pathSegs[start:]) {
			return true
		}
	}
	return false
}

// matchSegments matches pattern segments against path segments, where a
// "**" pattern segment consumes zero or more whole path segments and other
// segments use path.Match semantics.
func matchSegments(pat, segs []string) bool {
	type key struct{ i, j int }
	memo := map[key]bool{}
	var rec func(i, j int) bool
	rec = func(i, j int) bool {
		if i == len(pat) {
			return j == len(segs)
		}
		k := key{i, j}
		if v, ok := memo[k]; ok {
			return v
		}
		var ok bool
		if pat[i] == "**" && i == len(pat)-1 {
			// A trailing "**" consumes one or more segments: "x/**"
			// matches everything under x, but not x itself.
			ok = j < len(segs)
		} else if pat[i] == "**" {
			ok = rec(i+1, j) || (j < len(segs) && rec(i, j+1))
		} else {
			if j < len(segs) {
				matched, err := path.Match(pat[i], segs[j])
				ok = err == nil && matched && rec(i+1, j+1)
			}
		}
		memo[k] = ok
		return ok
	}
	return rec(0, 0)
}
