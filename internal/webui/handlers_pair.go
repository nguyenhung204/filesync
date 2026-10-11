package webui

import (
	"errors"
	"net/http"
	"strings"

	"filesync/internal/logx"
)

// Pairing handlers. Pairing completes only when BOTH sides confirm
// the fingerprint (pairing.Manager semantics) — no handler here
// confirms anything on the user's behalf; confirm/reject relay
// exactly one explicit user action each.

func (s *Server) handlePairCode(w http.ResponseWriter, r *http.Request) {
	code, expiresAt, err := s.backend.CreatePairCode()
	if err != nil {
		s.log.Warn(r.Context(), "webui: create pair code failed", logx.Err(err))
		writeError(w, http.StatusInternalServerError, "could not create pairing code")
		return
	}
	// The code is returned ONLY here, to the owner who asked for
	// it, for one-time display (spec: API table note).
	writeJSON(w, http.StatusOK, map[string]any{
		"code":       code,
		"expires_at": expiresAt.Unix(),
	})
}

type dialRequest struct {
	Addr string `json:"addr"`
	Code string `json:"code"`
}

func (s *Server) handlePairDial(w http.ResponseWriter, r *http.Request) {
	var req dialRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Addr == "" || req.Code == "" {
		writeError(w, http.StatusBadRequest, "addr and code are required")
		return
	}
	sessionID, err := s.backend.DialPair(r.Context(), req.Addr, req.Code)
	if err != nil {
		// pairing errors are user-safe by precedent (mailbox.go):
		// they never echo the submitted code back.
		writeError(w, http.StatusBadRequest, "pairing failed: "+safeMsg(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"session_id": sessionID})
}

type sessionRequest struct {
	SessionID string `json:"session_id"`
}

func (s *Server) handlePairConfirmReject(confirm bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req sessionRequest
		if !decodeBody(w, r, &req) {
			return
		}
		if req.SessionID == "" {
			writeError(w, http.StatusBadRequest, "session_id is required")
			return
		}
		var err error
		if confirm {
			err = s.backend.ConfirmPair(req.SessionID)
		} else {
			err = s.backend.RejectPair(req.SessionID)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "pairing action failed: "+safeMsg(err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

type revokeRequest struct {
	DeviceID string `json:"device_id"`
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	var req revokeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "device_id is required")
		return
	}
	// Backend re-checks existence / not-revoked / not-self (D9,
	// flow G); a refusal is a client error, not a server fault.
	if err := s.backend.RevokePeer(req.DeviceID); err != nil {
		if errors.Is(err, ErrBadRequest) {
			writeError(w, http.StatusBadRequest, safeMsg(err))
			return
		}
		s.log.Error(r.Context(), "webui: revoke failed", logx.Err(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ErrBadRequest marks a Backend refusal that should surface as
// HTTP 400 with its (user-safe) message instead of a generic 500.
var ErrBadRequest = errors.New("webui: bad request")

// safeMsg renders an error for the API response. Backend errors
// surfaced this way come from pairing or hold only identifiers the
// caller themselves supplied — never tokens, codes, or filesystem
// paths outside the sync/config dirs.
func safeMsg(err error) string {
	msg := err.Error()
	// Strip a wrapped ErrBadRequest marker, keeping the real text.
	if i := strings.Index(msg, ErrBadRequest.Error()); i >= 0 {
		if rest := strings.TrimPrefix(msg[i+len(ErrBadRequest.Error()):], ": "); rest != "" {
			return rest
		}
	}
	return msg
}
