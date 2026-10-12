package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"filesync/internal/pairing"
	"filesync/internal/sync"
	"filesync/internal/transport"
)

// Status is the daemon's public snapshot. The JSON field names are
// a contract with the CLI and the smoke test — do not rename.
type Status struct {
	Version     int    `json:"version"`
	State       string `json:"state"` // "running" | "paused" | "stopped"
	PID         int    `json:"pid"`
	DeviceID    string `json:"device_id"`
	DeviceName  string `json:"device_name"`
	Fingerprint string `json:"fingerprint"`
	ListenAddr  string `json:"listen_addr"`
	// UIAddr is the web UI's loopback address (job 006 D10,
	// additive, Version stays 1); empty when the UI is disabled
	// or failed to bind.
	UIAddr        string           `json:"ui_addr,omitempty"`
	SyncDir       string           `json:"sync_dir"`
	ConfigDir     string           `json:"config_dir"`
	StartedAt     int64            `json:"started_at"` // UnixNano
	UpdatedAt     int64            `json:"updated_at"` // UnixNano
	Paused        bool             `json:"paused"`
	IndexedFiles  int              `json:"indexed_files"`
	InitialScanMs int64            `json:"initial_scan_ms"`
	Peers         []PeerStatus     `json:"peers"`
	Queue         []sync.PullState `json:"queue"`
	// Job 009 additive fields (Version stays 1): Sends is the
	// display-only send progress (spec D3/D5); Discovered lists
	// unpaired machines announcing on the LAN (addendum).
	Sends         []sync.SendState             `json:"sends"`
	Discovered    []transport.DiscoveredDevice `json:"discovered"`
	BytesSent     int64                        `json:"bytes_sent"`
	BytesReceived int64                        `json:"bytes_received"`
	RateBps       int64                        `json:"rate_bps"`
	Rounds        int64                        `json:"rounds"`
	LastError     *sync.SyncError              `json:"last_error,omitempty"`
	RecentErrors  []sync.SyncError             `json:"recent_errors"`
	// Large-file archive skips (job 005): additive fields, Version
	// stays 1 — old consumers keep parsing.
	SkippedVersionArchives int64                 `json:"skipped_version_archives"`
	LastVersionSkip        *sync.VersionSkip     `json:"last_version_skip,omitempty"`
	PendingPairs           []pairing.PendingPair `json:"pending_pairs"`
	ActiveTransfers        []string              `json:"active_transfers"`
}

type PeerStatus struct {
	DeviceID    string `json:"device_id"`
	Name        string `json:"name"`
	Connected   bool   `json:"connected"`
	Revoked     bool   `json:"revoked"`
	LastRoundAt int64  `json:"last_round_at"` // UnixNano, 0 = never
}

const statusFileName = "status.json"

// Status builds the current snapshot. Safe to call any time,
// including before Run (yields identity-less zero values) and after
// shutdown (state "stopped").
func (d *Daemon) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := Status{
		Version:       1,
		State:         d.state,
		PID:           os.Getpid(),
		DeviceName:    d.cfg.DeviceName,
		SyncDir:       d.cfg.SyncDir,
		ConfigDir:     d.cfg.ConfigDir,
		ListenAddr:    d.listenAddr,
		UIAddr:        d.uiAddr,
		StartedAt:     d.startedAtUnix,
		UpdatedAt:     time.Now().UnixNano(),
		Paused:        d.pausedCached,
		IndexedFiles:  d.indexedFiles,
		InitialScanMs: d.initialScanMs,
		RateBps:       d.rateBps,
	}
	if d.id != nil {
		st.DeviceID = d.id.DeviceID
		st.Fingerprint = d.id.Fingerprint
	}
	if st.State == "" {
		st.State = "stopped"
	}
	if d.eng != nil {
		c := d.eng.Counters()
		st.BytesSent = c.BytesSent
		st.BytesReceived = c.BytesReceived
		st.Rounds = c.Rounds
		st.Queue = d.eng.ActivePulls()
		st.Sends = d.eng.ActiveSends()
		st.RecentErrors = d.eng.LastErrors()
		st.SkippedVersionArchives, st.LastVersionSkip = d.eng.SkippedArchives()
	}
	if st.Queue == nil {
		st.Queue = []sync.PullState{}
	}
	if st.Sends == nil {
		st.Sends = []sync.SendState{}
	}
	if st.RecentErrors == nil {
		st.RecentErrors = []sync.SyncError{}
	}
	if n := len(st.RecentErrors); n > 0 {
		last := st.RecentErrors[n-1]
		st.LastError = &last
	}
	st.Peers = []PeerStatus{}
	if d.st != nil {
		if peers, err := d.st.ListPeers(); err == nil {
			for _, p := range peers {
				ps := PeerStatus{DeviceID: p.DeviceID, Name: p.Name, Revoked: p.Revoked}
				if rt, ok := d.peerRT[p.DeviceID]; ok {
					ps.Connected = rt.connected
					ps.LastRoundAt = rt.lastRoundUnix
				}
				st.Peers = append(st.Peers, ps)
			}
		}
		if transfers, err := d.st.ListTransfers(); err == nil {
			st.ActiveTransfers = []string{}
			for _, tr := range transfers {
				st.ActiveTransfers = append(st.ActiveTransfers, tr.Path)
			}
		}
	}
	if st.ActiveTransfers == nil {
		st.ActiveTransfers = []string{}
	}
	// Discovered (job 009 addendum): machines announcing on the
	// LAN that have NO store record here yet — anything already
	// paired (or revoked) belongs to the peers list above, so
	// the wizard's nearby list never double-shows a machine.
	st.Discovered = []transport.DiscoveredDevice{}
	if d.tm != nil {
		known := make(map[string]bool, len(st.Peers))
		for _, p := range st.Peers {
			known[p.DeviceID] = true
		}
		for _, dev := range d.tm.Discovered() {
			if !known[dev.DeviceID] {
				st.Discovered = append(st.Discovered, dev)
			}
		}
	}
	st.PendingPairs = d.pendingListLocked()
	return st
}

// updateRateLocked folds the latest counter totals into rateBps
// using the delta since the previous status write.
func (d *Daemon) updateRateLocked(now time.Time, totalBytes int64) {
	if !d.ratePrevAt.IsZero() {
		dt := now.Sub(d.ratePrevAt).Seconds()
		if dt > 0 {
			d.rateBps = int64(float64(totalBytes-d.ratePrevBytes) / dt)
		}
	}
	d.ratePrevAt = now
	d.ratePrevBytes = totalBytes
}

// writeStatusFile atomically replaces status.json (temp + rename,
// 0600) in the config dir.
func (d *Daemon) writeStatusFile(st Status) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d.cfg.ConfigDir, ".status-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(d.cfg.ConfigDir, statusFileName))
}
