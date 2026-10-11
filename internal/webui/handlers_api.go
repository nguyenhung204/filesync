package webui

import (
	"net/http"

	"filesync/internal/logx"
)

// Read-only API handlers (status is registered inline in routes).

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.backend.Peers()
	if err != nil {
		s.log.Error(r.Context(), "webui: list peers failed", logx.Err(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if peers == nil {
		peers = []PeerView{}
	}
	writeJSON(w, http.StatusOK, peers)
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	files, err := s.backend.Files()
	if err != nil {
		s.log.Error(r.Context(), "webui: list files failed", logx.Err(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if files == nil {
		files = []FileView{}
	}
	writeJSON(w, http.StatusOK, files)
}

// handlePauseResume covers both POST /api/pause and /api/resume:
// pausing suspends sync rounds via the daemon's pause journal, the
// UI itself stays fully usable (spec Flow D).
func (s *Server) handlePauseResume(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.backend.SetPaused(paused); err != nil {
			s.log.Error(r.Context(), "webui: set paused failed", logx.Err(err))
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"paused": paused})
	}
}
