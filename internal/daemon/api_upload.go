package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"

	"filesync/internal/safefile"
	fsync "filesync/internal/sync"
	"filesync/internal/validate"
	"filesync/internal/webui"
)

// countingReader counts the bytes safefile streams through it,
// so Upload can report the written size without a second stat.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Upload implements webui.Backend.Upload (job 009 spec D4): a
// browser-dropped file is, semantically, the user copying the
// file into the sync dir — so this writes it with the same
// atomic streaming write any local file gets, and the watcher +
// scanner pick it up from there like any other local file. It
// deliberately does NOT touch the index or the store: an upload
// is indistinguishable from an Explorer copy-in, including the
// overwrite rule (the old live copy is not version-archived on
// THIS side; the receiving side archives when the overwrite syncs
// to it, per the standing sync rules).
//
// The body is streamed straight from the request into the temp
// file (never buffered whole), so size is bounded only by disk.
func (d *Daemon) Upload(_ context.Context, relPath string, sizeHint int64, r io.Reader) (int64, error) {
	clean, err := validate.ValidateRelPath(relPath, d.cfg.MaxPathLength)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", webui.ErrInvalidPath, err)
	}
	// The ONE meta-tree guard (sync.IsMetaTreePath): catches
	// .filesync at any segment, any case, and the Windows
	// dotless-i fold — never reimplement name folding here.
	if fsync.IsMetaTreePath(clean) {
		return 0, fmt.Errorf("%w: %s", webui.ErrMetaPath, clean)
	}
	if sizeHint >= 0 {
		if err := safefile.RequireSpace(d.cfg.SyncDir, sizeHint); err != nil {
			return 0, fmt.Errorf("%w: %v", webui.ErrDiskFull, err)
		}
	}
	cr := &countingReader{r: r}
	err = safefile.WriteAtomicMaxLen(d.cfg.SyncDir, clean, cr, 0o644, d.cfg.MaxPathLength)
	if err != nil {
		switch {
		case safefile.IsLockedErr(err):
			return 0, fmt.Errorf("%w: %v", webui.ErrLocked, err)
		case errors.Is(err, safefile.ErrDiskFull), errors.Is(err, syscall.ENOSPC):
			return 0, fmt.Errorf("%w: %v", webui.ErrDiskFull, err)
		}
		return 0, err
	}
	return cr.n, nil
}
