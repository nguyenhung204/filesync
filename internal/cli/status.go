package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filesync/internal/daemon"
	"filesync/internal/identity"
)

// statusFreshWindow: a status.json counts as live when it was
// written within this window and does not say "stopped".
const statusFreshWindow = 6 * time.Second

func readLiveStatus(e *env) (daemon.Status, []byte, bool) {
	raw, err := os.ReadFile(filepath.Join(e.configDir, "status.json"))
	if err != nil {
		return daemon.Status{}, nil, false
	}
	var st daemon.Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return daemon.Status{}, nil, false
	}
	age := time.Since(time.Unix(0, st.UpdatedAt))
	if age < 0 || age > statusFreshWindow || st.State == "stopped" || st.State == "" {
		return daemon.Status{}, nil, false
	}
	return st, raw, true
}

// cmdStatus prints the live daemon status when status.json is
// fresh; otherwise it falls back to reading identity + store
// directly and reports the daemon as not running.
func cmdStatus(e *env, _ []string) int {
	if st, raw, live := readLiveStatus(e); live {
		if e.json {
			fmt.Fprintln(e.stdout, string(raw))
			return 0
		}
		printLiveStatus(e, st)
		return 0
	}
	// Offline fallback.
	deviceID := ""
	if e.initialized() {
		if id, _, err := identity.LoadOrCreate(e.configDir); err == nil {
			deviceID = id.DeviceID
		}
	}
	_, st, err := e.openStore()
	if err != nil {
		return e.fail("status: %v", err)
	}
	defer st.Close()
	peers, _ := st.ListPeers()
	entries, _ := st.ListEntries()
	indexed := 0
	for _, en := range entries {
		if !en.Tombstone {
			indexed++
		}
	}
	if e.json {
		out := map[string]any{
			"state":         "offline",
			"device_id":     deviceID,
			"peers":         len(peers),
			"indexed_files": indexed,
		}
		raw, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(e.stdout, string(raw))
		return 0
	}
	fmt.Fprintln(e.stdout, "daemon: not running")
	if deviceID != "" {
		fmt.Fprintf(e.stdout, "device_id:    %s\n", deviceID)
	}
	fmt.Fprintf(e.stdout, "peers:        %d\n", len(peers))
	fmt.Fprintf(e.stdout, "indexed_files: %d\n", indexed)
	return 0
}

func printLiveStatus(e *env, st daemon.Status) {
	fmt.Fprintf(e.stdout, "state:        %s\n", st.State)
	fmt.Fprintf(e.stdout, "device:       %s (%s)\n", st.DeviceName, st.DeviceID)
	fmt.Fprintf(e.stdout, "listen:       %s\n", st.ListenAddr)
	fmt.Fprintf(e.stdout, "sync_dir:     %s\n", st.SyncDir)
	fmt.Fprintf(e.stdout, "indexed:      %d files (initial scan %d ms)\n", st.IndexedFiles, st.InitialScanMs)
	fmt.Fprintf(e.stdout, "transferred:  %d B sent, %d B received, %d B/s now, %d rounds\n",
		st.BytesSent, st.BytesReceived, st.RateBps, st.Rounds)
	if len(st.PendingPairs) > 0 {
		fmt.Fprintln(e.stdout, "pending pairs:")
		for _, pp := range st.PendingPairs {
			fmt.Fprintf(e.stdout, "  %s (%s…) fingerprint %s\n", pp.Name, shortID(pp.DeviceID), pp.Fingerprint)
			if words := fingerprintWords(pp.Fingerprint); len(words) == 8 {
				fmt.Fprintf(e.stdout, "    Từ đối chiếu: %s\n", strings.Join(words, " · "))
			}
		}
	}
	fmt.Fprintln(e.stdout, "peers:")
	if len(st.Peers) == 0 {
		fmt.Fprintln(e.stdout, "  (none)")
	}
	for _, p := range st.Peers {
		last := "never"
		if p.LastRoundAt > 0 {
			last = time.Unix(0, p.LastRoundAt).Format("15:04:05")
		}
		fmt.Fprintf(e.stdout, "  %-16s %s… connected=%v revoked=%v last_round=%s\n",
			truncate(p.Name, 16), shortID(p.DeviceID), p.Connected, p.Revoked, last)
	}
	if len(st.Queue) > 0 {
		fmt.Fprintln(e.stdout, "queue:")
		for _, q := range st.Queue {
			pct := 0
			if q.BytesTotal > 0 {
				pct = int(100 * q.BytesDone / q.BytesTotal)
			}
			fmt.Fprintf(e.stdout, "  %-40s %3d%% from %s… [%s]\n", truncate(q.Path, 40), pct, shortID(q.PeerID), q.State)
		}
	}
	if st.LastError != nil {
		fmt.Fprintf(e.stdout, "last error:   [%s] %s (%s)\n", st.LastError.Code, st.LastError.Path, st.LastError.Message)
	}
	if len(st.ActiveTransfers) > 0 {
		fmt.Fprintf(e.stdout, "active transfers: %v\n", st.ActiveTransfers)
	}
}
