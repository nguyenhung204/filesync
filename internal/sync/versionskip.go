package sync

// VersionSkip records one skipped version archive (job 005
// large-file policy): the live copy of Path was larger than the
// version-history quota at overwrite time, so the old copy was NOT
// stored in version history. The file itself synced normally.
type VersionSkip struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	At   int64  `json:"at"` // UnixNano, display only, never ordering
}

// SkippedArchives returns the total number of archive skips since
// engine start and a copy of the most recent skip (nil when none).
// Same exposure pattern as LastErrors.
func (e *Engine) SkippedArchives() (int64, *VersionSkip) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var last *VersionSkip
	if e.lastSkip != nil {
		cp := *e.lastSkip
		last = &cp
	}
	return e.skippedArchives, last
}

// noteVersionSkip records a skip of rel: the counter and lastSkip
// update on EVERY skip, but it reports true only for the first skip
// of this path since its last successful archive — the caller logs
// the warning only then, so a frequently-edited large file does not
// spam the log while status still shows the running total (spec D5).
func (e *Engine) noteVersionSkip(rel string, size int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.skippedArchives++
	e.lastSkip = &VersionSkip{Path: rel, Size: size, At: e.now().UnixNano()}
	if e.warnedSkips == nil {
		e.warnedSkips = map[string]bool{}
	}
	if e.warnedSkips[rel] {
		return false
	}
	e.warnedSkips[rel] = true
	return true
}

// clearSkipWarn re-arms the skip warning for rel after a successful
// archive, so a file that later grows past the quota warns again.
func (e *Engine) clearSkipWarn(rel string) {
	e.mu.Lock()
	delete(e.warnedSkips, rel)
	e.mu.Unlock()
}
