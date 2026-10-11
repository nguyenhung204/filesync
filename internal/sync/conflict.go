package sync

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"filesync/internal/store"
)

// Conflict copy naming (D3): "<stem> (conflict <shortID> <ts>)<ext>"
// where shortID is the first 8 characters of the LOSING entry's
// author device ID and ts is the losing entry's MtimeNs in UTC,
// formatted 20060102T150405Z. The name is a pure function of the
// losing entry, so both peers compute the identical path and exactly
// one conflict copy exists after a resolution round.

const conflictTimestampFormat = "20060102T150405Z"

// ConflictPath returns the conflict copy path for the losing entry.
func ConflictPath(loser store.Entry) string {
	return conflictPathSuffix(loser, 0)
}

// conflictPathSuffix adds the pathological-repeat suffix: 0 means
// none, n >= 2 produces "-n" before the extension.
func conflictPathSuffix(loser store.Entry, n int) string {
	dir, base := splitDir(loser.Path)
	stem, ext := splitExt(base)
	short := loser.UpdatedBy
	if len(short) > 8 {
		short = short[:8]
	}
	if short == "" {
		short = "unknown"
	}
	ts := time.Unix(0, loser.MtimeNs).UTC().Format(conflictTimestampFormat)
	suffix := ""
	if n >= 2 {
		suffix = fmt.Sprintf("-%d", n)
	}
	return fmt.Sprintf("%s%s (conflict %s %s)%s%s", dir, stem, short, ts, suffix, ext)
}

func splitDir(p string) (dir, base string) {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i+1], p[i+1:]
	}
	return "", p
}

// splitExt splits a base name at its LAST dot; the extension keeps
// the dot. A leading dot (dotfile) is not an extension separator.
func splitExt(base string) (stem, ext string) {
	if i := strings.LastIndex(base, "."); i > 0 {
		return base[:i], base[i:]
	}
	return base, ""
}

// conflictSegmentRe matches the conflict marker on a path's last
// segment (shape pinned by the job 004 spec for the CLI listing).
var conflictSegmentRe = regexp.MustCompile(`^(.*) \(conflict ([0-9A-Za-z]{8}) (\d{8}T\d{6}Z)\)(-\d+)?(\.[^/]*)?$`)

// ParseConflictPath reports whether p is a conflict copy path, and if
// so derives the original path and the losing author's short ID.
func ParseConflictPath(p string) (original, loserShort string, ok bool) {
	dir, base := splitDir(p)
	m := conflictSegmentRe.FindStringSubmatch(base)
	if m == nil {
		return "", "", false
	}
	return dir + m[1] + m[5], m[2], true
}

// IsConflictPath reports whether p looks like a D3 conflict copy.
func IsConflictPath(p string) bool {
	_, _, ok := ParseConflictPath(p)
	return ok
}
