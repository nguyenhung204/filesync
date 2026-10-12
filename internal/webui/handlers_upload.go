package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"filesync/internal/logx"
)

// handleUpload serves POST /api/upload?path=<relative path>
// (job 009 spec D4). One request is one file; the body is the
// file's raw bytes, streamed by the daemon backend straight to
// disk — this handler never buffers it (no decodeBody, no
// MaxBytesReader: the 64KiB cap is for JSON only). It sits in the
// same host -> auth -> origin middleware chain as every other
// route, so all of job 006's protections apply unchanged.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	rawPath := r.URL.Query().Get("path")
	if rawPath == "" {
		writeUploadError(w, http.StatusBadRequest, "invalid_path", "missing or empty path")
		return
	}
	// Escape the server-wide ReadTimeout/WriteTimeout for this
	// request only: a multi-GiB upload on a slow disk legitimately
	// takes far longer than 15s/30s. Best effort — a controller
	// that cannot clear deadlines just leaves the defaults.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	n, err := s.backend.Upload(r.Context(), rawPath, r.ContentLength, r.Body)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidPath):
			writeUploadError(w, http.StatusBadRequest, "invalid_path", "invalid file name or path")
		case errors.Is(err, ErrMetaPath):
			writeUploadError(w, http.StatusBadRequest, "meta_path", "cannot upload into the FileSync metadata folder")
		case errors.Is(err, ErrDiskFull):
			writeUploadError(w, http.StatusInsufficientStorage, "disk_full", "not enough free disk space")
		case errors.Is(err, ErrLocked):
			writeUploadError(w, http.StatusConflict, "locked", "the destination file is in use by another program")
		default:
			s.log.Error(r.Context(), "webui: upload failed", logx.Err(err))
			writeUploadError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": rawPath, "size": n})
}

// writeUploadError renders the upload error shape: a user-safe
// message plus the stable code the UI maps to plain-language
// help (spec D7). Codes are part of the API contract.
func writeUploadError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	raw, _ := json.Marshal(map[string]string{"error": msg, "code": code})
	_, _ = w.Write(raw)
}
