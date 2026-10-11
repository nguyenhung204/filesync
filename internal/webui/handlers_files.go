package webui

import (
	"errors"
	"net/http"

	"filesync/internal/logx"
	fsync "filesync/internal/sync"
)

// Conflict + history handlers. All paths arrive from the client and
// are validated inside the Backend (ValidateRelPath) before they
// touch the store, history, or engine (spec: An toàn #5).

func (s *Server) handleConflicts(w http.ResponseWriter, r *http.Request) {
	conflicts, err := s.backend.Conflicts()
	if err != nil {
		s.log.Error(r.Context(), "webui: list conflicts failed", logx.Err(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if conflicts == nil {
		conflicts = []fsync.ConflictInfo{}
	}
	writeJSON(w, http.StatusOK, conflicts)
}

type resolveRequest struct {
	Original string `json:"original"`
	Conflict string `json:"conflict"`
	Keep     string `json:"keep"`
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	var req resolveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Original == "" {
		writeError(w, http.StatusBadRequest, "original is required")
		return
	}
	if req.Keep != "mine" && req.Keep != "theirs" {
		writeError(w, http.StatusBadRequest, "keep must be mine or theirs")
		return
	}
	if err := s.backend.ResolveConflict(req.Original, req.Conflict, req.Keep); err != nil {
		if errors.Is(err, ErrBadRequest) {
			writeError(w, http.StatusBadRequest, safeMsg(err))
			return
		}
		s.log.Error(r.Context(), "webui: resolve conflict failed", logx.Err(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	versions, err := s.backend.Versions(path)
	if err != nil {
		if errors.Is(err, ErrBadRequest) {
			writeError(w, http.StatusBadRequest, safeMsg(err))
			return
		}
		s.log.Error(r.Context(), "webui: list versions failed", logx.Err(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if versions == nil {
		versions = []VersionView{}
	}
	writeJSON(w, http.StatusOK, versions)
}

type restoreRequest struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req restoreRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	restored, err := s.backend.Restore(req.Path, req.Version)
	if err != nil {
		if errors.Is(err, ErrBadRequest) {
			writeError(w, http.StatusBadRequest, safeMsg(err))
			return
		}
		s.log.Error(r.Context(), "webui: restore failed", logx.Err(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, restored)
}
