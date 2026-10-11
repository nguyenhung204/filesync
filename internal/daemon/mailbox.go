package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"filesync/internal/logx"
)

// Pair mailbox (D6): the CLI asks the running daemon to perform
// pairing actions by dropping pair-mailbox.json into the config
// dir; the daemon answers in pair-mailbox-result.json. Both files
// are written atomically, mode 0600, inside the 0700 config dir.
const (
	mailboxRequestFile = "pair-mailbox.json"
	mailboxProcessFile = "pair-mailbox.processing"
	mailboxResultFile  = "pair-mailbox-result.json"
	pauseJournalKey    = "daemon:paused"
)

type pairRequest struct {
	Action    string `json:"action"`
	Addr      string `json:"addr,omitempty"`
	Code      string `json:"code,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type pairResult struct {
	Action    string `json:"action"`
	OK        bool   `json:"ok"`
	Code      string `json:"code,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"` // Unix seconds
	SessionID string `json:"session_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// processMailbox handles at most one pending request per call. It
// is driven by the daemon's mailbox goroutine.
func (d *Daemon) processMailbox(ctx context.Context) {
	reqPath := filepath.Join(d.cfg.ConfigDir, mailboxRequestFile)
	procPath := filepath.Join(d.cfg.ConfigDir, mailboxProcessFile)
	if _, err := os.Stat(reqPath); err != nil {
		return
	}
	// Claim the request: whoever renames it owns it.
	if err := os.Rename(reqPath, procPath); err != nil {
		return
	}
	defer os.Remove(procPath)
	raw, err := os.ReadFile(procPath)
	if err != nil {
		d.log.Warn(ctx, "daemon: read pair mailbox failed", logx.Err(err))
		return
	}
	var req pairRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		d.writePairResult(pairResult{OK: false, Error: "invalid mailbox request"})
		return
	}
	res := d.handlePairRequest(ctx, req)
	d.writePairResult(res)
}

// handlePairRequest executes one mailbox action. Error strings are
// user-facing and must never contain a pairing code or proof
// material — pairing's own errors satisfy this (the code is an
// input to DialPair and is never echoed back by pairing).
func (d *Daemon) handlePairRequest(ctx context.Context, req pairRequest) pairResult {
	res := pairResult{Action: req.Action}
	switch req.Action {
	case "create_code":
		code, expires, err := d.pm.CreateCode()
		if err != nil {
			res.Error = "could not create pairing code"
			d.log.Warn(ctx, "daemon: create pairing code failed", logx.Err(err))
			return res
		}
		res.OK = true
		res.Code = code
		res.ExpiresAt = expires.Unix()
	case "dial_pair":
		if req.Addr == "" {
			res.Error = "dial_pair requires an address"
			return res
		}
		pp, err := d.pm.DialPair(ctx, d.tm, req.Addr, req.Code)
		if err != nil {
			res.Error = "pairing failed: " + err.Error()
			return res
		}
		d.addPending(pp)
		res.OK = true
		res.SessionID = pp.SessionID
	case "confirm":
		if err := d.pm.ConfirmFingerprint(req.SessionID); err != nil {
			res.Error = "confirm failed: " + err.Error()
			return res
		}
		d.dropPending(req.SessionID)
		res.OK = true
		res.SessionID = req.SessionID
	case "reject":
		if err := d.pm.RejectFingerprint(req.SessionID); err != nil {
			res.Error = "reject failed: " + err.Error()
			return res
		}
		d.dropPending(req.SessionID)
		res.OK = true
		res.SessionID = req.SessionID
	default:
		res.Error = "unknown mailbox action"
	}
	return res
}

func (d *Daemon) writePairResult(res pairResult) {
	raw, err := json.Marshal(res)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(d.cfg.ConfigDir, ".pair-result-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmpName, filepath.Join(d.cfg.ConfigDir, mailboxResultFile))
}
