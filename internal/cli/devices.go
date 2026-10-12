package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"filesync/internal/identity"
	"filesync/internal/logx"
	"filesync/internal/pairing"
	"filesync/internal/store"
)

// cmdDevices lists paired devices, or (with `revoke`) revokes one.
func cmdDevices(e *env, args []string) int {
	if len(args) > 0 && args[0] == "revoke" {
		return cmdDevicesRevoke(e, args[1:])
	}
	_, st, err := e.openStore()
	if err != nil {
		return e.fail("devices: %v", err)
	}
	defer st.Close()
	peers, err := st.ListPeers()
	if err != nil {
		return e.fail("devices: %v", err)
	}
	if e.json {
		if peers == nil {
			peers = []store.Peer{}
		}
		raw, err := json.MarshalIndent(peers, "", "  ")
		if err != nil {
			return e.fail("devices: %v", err)
		}
		fmt.Fprintln(e.stdout, string(raw))
		return 0
	}
	if len(peers) == 0 {
		fmt.Fprintln(e.stdout, "no paired devices (pair one with: filesync pair)")
		return 0
	}
	fmt.Fprintf(e.stdout, "%-10s %-16s %-14s %-22s %-12s %s\n", "DEVICE", "NAME", "FINGERPRINT", "ADDRESSES", "PAIRED", "REVOKED")
	for _, p := range peers {
		fp := p.CertFingerprint
		if len(fp) > 11 {
			fp = fp[:11] + "…"
		}
		paired := "-"
		if p.PairedAt > 0 {
			paired = time.Unix(0, p.PairedAt).UTC().Format("2006-01-02")
		}
		fmt.Fprintf(e.stdout, "%-10s %-16s %-14s %-22s %-12s %v\n",
			shortID(p.DeviceID)+"…", truncate(p.Name, 16), fp,
			truncate(strings.Join(p.Addresses, ","), 22), paired, p.Revoked)
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

// cmdDevicesRevoke revokes a peer by full ID or unique prefix of
// at least 8 characters. The write goes straight to the store (the
// same pairing.Revoke path the daemon uses); a running daemon
// disconnects the peer within one round tick.
func cmdDevicesRevoke(e *env, args []string) int {
	if len(args) != 1 {
		return e.fail("usage: filesync devices revoke <device-id-or-prefix>")
	}
	target := args[0]
	cfg, st, err := e.openStore()
	if err != nil {
		return e.fail("revoke: %v", err)
	}
	defer st.Close()
	peers, err := st.ListPeers()
	if err != nil {
		return e.fail("revoke: %v", err)
	}
	var matches []store.Peer
	for _, p := range peers {
		if p.DeviceID == target || (len(target) >= 8 && strings.HasPrefix(p.DeviceID, target)) {
			matches = append(matches, p)
		}
	}
	if len(target) < 8 && len(matches) == 0 {
		return e.fail("revoke: %q is not a full device ID; a prefix needs at least 8 characters", target)
	}
	if len(matches) > 1 {
		var ids []string
		for _, m := range matches {
			ids = append(ids, m.DeviceID)
		}
		return e.fail("revoke: prefix %q is ambiguous, candidates:\n  %s", target, strings.Join(ids, "\n  "))
	}
	id, _, err := identity.LoadOrCreate(cfg.ConfigDir)
	if err != nil {
		return e.fail("revoke: %v", err)
	}
	log := logx.New(os.Stderr, logx.LevelError)
	pm := pairing.NewManager(id, st, log, nil, pairing.Options{Name: cfg.DeviceName})
	deviceID := target
	if len(matches) == 1 {
		deviceID = matches[0].DeviceID
	}
	if err := pm.Revoke(deviceID); err != nil {
		return e.fail("revoke: %v", err)
	}
	fmt.Fprintf(e.stdout, "revoked: %s\n", deviceID)
	return 0
}
