package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"filesync/internal/logx"
	"filesync/internal/store"
	fsync "filesync/internal/sync"
	"filesync/internal/validate"
	"filesync/internal/webui"
)

// This file is the daemon side of the web UI (job 006): it starts
// the loopback server inside Run and implements webui.Backend on
// top of the very same daemon/engine/pairing/history objects the
// CLI mailbox drives — the UI adds no second path to the data.

var _ webui.Backend = (*Daemon)(nil)

// startWebUI launches the UI server in its own goroutine, tracked
// by wg so Run does not return before it has shut down. A UI
// failure (port busy, token error) is only a Warn: sync keeps
// running without the UI (spec D1/D3).
func (d *Daemon) startWebUI(ctx context.Context, wg *sync.WaitGroup) {
	if !d.cfg.UIEnabled {
		return
	}
	srv := webui.New(d, webui.Options{
		ConfigDir: d.cfg.ConfigDir,
		Port:      d.cfg.EffectiveUIPort(),
		Log:       d.log,
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Start(ctx); err != nil {
			d.log.Warn(ctx, "daemon: web UI unavailable; sync continues without it", logx.Err(err))
		}
	}()
	// Publish the bound address into status once it exists (same
	// pattern as the transport listen address above).
	go func() {
		for i := 0; i < 500; i++ {
			if addr := srv.Addr(); addr != "" {
				d.mu.Lock()
				d.uiAddr = addr
				d.mu.Unlock()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
}

// Overview maps the daemon Status 1:1 onto the UI view.
func (d *Daemon) Overview() webui.StatusView {
	st := d.Status()
	return webui.StatusView{
		Version: st.Version, State: st.State, PID: st.PID,
		DeviceID: st.DeviceID, DeviceName: st.DeviceName,
		Fingerprint: st.Fingerprint, ListenAddr: st.ListenAddr,
		UIAddr: st.UIAddr, SyncDir: st.SyncDir, ConfigDir: st.ConfigDir,
		StartedAt: st.StartedAt, UpdatedAt: st.UpdatedAt,
		Paused: st.Paused, IndexedFiles: st.IndexedFiles,
		InitialScanMs: st.InitialScanMs, Queue: st.Queue,
		BytesSent: st.BytesSent, BytesReceived: st.BytesReceived,
		RateBps: st.RateBps, Rounds: st.Rounds,
		LastError: st.LastError, RecentErrors: st.RecentErrors,
		SkippedVersionArchives: st.SkippedVersionArchives,
		LastVersionSkip:        st.LastVersionSkip,
		ActiveTransfers:        st.ActiveTransfers,
	}
}

func (d *Daemon) Peers() ([]webui.PeerView, error) {
	if d.st == nil {
		return nil, errors.New("daemon: store not ready")
	}
	peers, err := d.st.ListPeers()
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]webui.PeerView, 0, len(peers))
	for _, p := range peers {
		pv := webui.PeerView{
			DeviceID: p.DeviceID, Name: p.Name,
			Fingerprint: p.CertFingerprint, Addresses: p.Addresses,
			PairedAt: p.PairedAt, Revoked: p.Revoked,
		}
		if pv.Addresses == nil {
			pv.Addresses = []string{}
		}
		if rt, ok := d.peerRT[p.DeviceID]; ok {
			pv.Connected = rt.connected
			pv.LastRoundAt = rt.lastRoundUnix
		}
		out = append(out, pv)
	}
	return out, nil
}

func (d *Daemon) Pending() []webui.PendingView {
	d.mu.Lock()
	defer d.mu.Unlock()
	raw := d.pendingListLocked()
	out := make([]webui.PendingView, 0, len(raw))
	for _, pp := range raw {
		out = append(out, webui.PendingView{
			SessionID: pp.SessionID, DeviceID: pp.DeviceID,
			Name: pp.Name, Fingerprint: pp.Fingerprint,
		})
	}
	return out
}

func (d *Daemon) CreatePairCode() (string, time.Time, error) {
	return d.pm.CreateCode()
}

func (d *Daemon) DialPair(ctx context.Context, addr, code string) (string, error) {
	pp, err := d.pm.DialPair(ctx, d.tm, addr, code)
	if err != nil {
		return "", err
	}
	d.addPending(pp)
	return pp.SessionID, nil
}

func (d *Daemon) ConfirmPair(sessionID string) error {
	if err := d.pm.ConfirmFingerprint(sessionID); err != nil {
		return err
	}
	d.dropPending(sessionID)
	return nil
}

func (d *Daemon) RejectPair(sessionID string) error {
	if err := d.pm.RejectFingerprint(sessionID); err != nil {
		return err
	}
	d.dropPending(sessionID)
	return nil
}

// RevokePeer re-checks the request server-side (spec flow G):
// the device must exist, not already be revoked, and not be this
// machine itself.
func (d *Daemon) RevokePeer(deviceID string) error {
	d.mu.Lock()
	own := ""
	if d.id != nil {
		own = d.id.DeviceID
	}
	d.mu.Unlock()
	if deviceID == own {
		return fmt.Errorf("%w: cannot revoke this device itself", webui.ErrBadRequest)
	}
	peers, err := d.st.ListPeers()
	if err != nil {
		return err
	}
	found := false
	for _, p := range peers {
		if p.DeviceID == deviceID {
			found = true
			if p.Revoked {
				return fmt.Errorf("%w: device %s is already revoked", webui.ErrBadRequest, deviceID)
			}
		}
	}
	if !found {
		return fmt.Errorf("%w: unknown device %s", webui.ErrBadRequest, deviceID)
	}
	if err := d.pm.Revoke(deviceID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: unknown device %s", webui.ErrBadRequest, deviceID)
		}
		return err
	}
	return nil
}

// SetPaused drives the same pause journal the CLI writes.
func (d *Daemon) SetPaused(paused bool) error {
	if paused {
		return d.st.JournalSet(pauseJournalKey, []byte("1"))
	}
	return d.st.JournalDelete(pauseJournalKey)
}

func (d *Daemon) Conflicts() ([]fsync.ConflictInfo, error) {
	entries, err := d.st.ListEntries()
	if err != nil {
		return nil, err
	}
	return fsync.ListConflicts(entries), nil
}

// ResolveConflict mirrors the CLI's candidate rules: an explicit
// conflict copy must belong to the original; without one, exactly
// one copy may exist. keep is pre-validated by the web layer.
func (d *Daemon) ResolveConflict(original, conflictPath, keep string) error {
	clean, err := validate.ValidateRelPath(original, d.cfg.MaxPathLength)
	if err != nil {
		return fmt.Errorf("%w: %v", webui.ErrBadRequest, err)
	}
	conflicts, err := d.Conflicts()
	if err != nil {
		return err
	}
	var copies []string
	for _, ci := range conflicts {
		if ci.Original == clean {
			copies = append(copies, ci.Conflict)
		}
	}
	if conflictPath == "" {
		if len(copies) == 0 {
			return fmt.Errorf("%w: no conflict copy found for %q", webui.ErrBadRequest, clean)
		}
		if len(copies) > 1 {
			return fmt.Errorf("%w: %d conflict copies exist for %q — pick one: %s",
				webui.ErrBadRequest, len(copies), clean, strings.Join(copies, ", "))
		}
		conflictPath = copies[0]
	} else {
		if _, err := validate.ValidateRelPath(conflictPath, d.cfg.MaxPathLength); err != nil {
			return fmt.Errorf("%w: %v", webui.ErrBadRequest, err)
		}
		ok := false
		for _, c := range copies {
			if c == conflictPath {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("%w: %q is not a conflict copy of %q", webui.ErrBadRequest, conflictPath, clean)
		}
	}
	var side fsync.KeepSide
	if keep == "mine" {
		side = fsync.KeepMine
	} else {
		side = fsync.KeepTheirs
	}
	return d.eng.ResolveConflictCopy(clean, conflictPath, side)
}

func (d *Daemon) Files() ([]webui.FileView, error) {
	entries, err := d.st.ListEntries()
	if err != nil {
		return nil, err
	}
	out := []webui.FileView{}
	for _, e := range entries {
		if e.Tombstone {
			continue
		}
		out = append(out, webui.FileView{Path: e.Path, Size: e.Size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (d *Daemon) Versions(path string) ([]webui.VersionView, error) {
	clean, err := validate.ValidateRelPath(path, d.cfg.MaxPathLength)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrBadRequest, err)
	}
	versions, err := d.hist.ListVersions(clean)
	if err != nil {
		return nil, err
	}
	// history returns oldest first; the UI presents newest first
	// (same as the CLI).
	out := []webui.VersionView{}
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		out = append(out, webui.VersionView{
			Name: v.Name, Path: v.Rel, Size: v.Size,
			ModTime: v.ModTime.Format(time.RFC3339),
		})
	}
	return out, nil
}

// Restore mirrors the CLI: empty version = newest; an unknown
// version is a 400 listing what exists. history.Restore archives
// the current content first, so a restore stays undoable.
func (d *Daemon) Restore(path, version string) (webui.VersionView, error) {
	clean, err := validate.ValidateRelPath(path, d.cfg.MaxPathLength)
	if err != nil {
		return webui.VersionView{}, fmt.Errorf("%w: %v", webui.ErrBadRequest, err)
	}
	versions, err := d.hist.ListVersions(clean)
	if err != nil {
		return webui.VersionView{}, err
	}
	if len(versions) == 0 {
		return webui.VersionView{}, fmt.Errorf("%w: no versions for %s", webui.ErrBadRequest, clean)
	}
	target := versions[len(versions)-1] // newest
	if version != "" {
		found := false
		for _, v := range versions {
			if v.Name == version {
				target, found = v, true
				break
			}
		}
		if !found {
			names := make([]string, len(versions))
			for i, v := range versions {
				names[i] = v.Name
			}
			return webui.VersionView{}, fmt.Errorf("%w: version %q not found for %s; available versions: %s",
				webui.ErrBadRequest, version, clean, strings.Join(names, ", "))
		}
	}
	if err := d.hist.Restore(clean, target.Name); err != nil {
		return webui.VersionView{}, err
	}
	return webui.VersionView{
		Name: target.Name, Path: target.Rel, Size: target.Size,
		ModTime: target.ModTime.Format(time.RFC3339),
	}, nil
}
