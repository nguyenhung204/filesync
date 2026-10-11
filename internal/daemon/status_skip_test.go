package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesync/internal/config"
	"filesync/internal/logx"
)

// startTestDaemonQuota is startTestDaemon with a custom
// version-history quota, for the job-005 skip test.
func startTestDaemonQuota(t *testing.T, name string, quota int64) *testDaemon {
	t.Helper()
	base := t.TempDir()
	cfgDir := filepath.Join(base, "config")
	syncDir := filepath.Join(base, "sync")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ConfigDir = cfgDir
	cfg.SyncDir = syncDir
	cfg.DeviceName = name
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.VersionHistoryQuotaBytes = quota
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(loaded, logx.New(os.Stderr, logx.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	td := &testDaemon{d: d, cfg: loaded, cancel: cancel, done: make(chan error, 1)}
	go func() { td.done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-td.done:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop in 10s")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if d.Status().ListenAddr != "" {
			return td
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon did not start listening in 10s")
	return nil
}

func readSyncFile(t *testing.T, dir, rel string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return "", false
	}
	return string(data), true
}

// Job 005: when B (quota 64 B) receives an overwrite of a 200-byte
// file, the old copy is NOT archived — and B's status reports the
// skip with the new additive fields, visible in the JSON contract.
func TestDaemonStatusReportsVersionSkip(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	a := startTestDaemon(t, "skip-a")
	b := startTestDaemonQuota(t, "skip-b", 64)
	a.trustPeer(t, b)
	b.trustPeer(t, a)

	v1 := strings.Repeat("a", 200)
	v2 := strings.Repeat("b", 200)
	writeSyncFile(t, a.cfg.SyncDir, "big.txt", v1)
	waitFor(t, "v1 synced to B", 60*time.Second, func() bool {
		got, ok := readSyncFile(t, b.cfg.SyncDir, "big.txt")
		return ok && got == v1
	})
	writeSyncFile(t, a.cfg.SyncDir, "big.txt", v2)
	waitFor(t, "v2 synced to B", 60*time.Second, func() bool {
		got, ok := readSyncFile(t, b.cfg.SyncDir, "big.txt")
		return ok && got == v2
	})
	waitFor(t, "skip reported in B status", 30*time.Second, func() bool {
		st := b.d.Status()
		return st.SkippedVersionArchives == 1 && st.LastVersionSkip != nil &&
			st.LastVersionSkip.Path == "big.txt" && st.LastVersionSkip.Size == int64(len(v1))
	})

	raw, err := json.Marshal(b.d.Status())
	if err != nil {
		t.Fatal(err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("status JSON does not parse: %v", err)
	}
	if asMap["skipped_version_archives"] != float64(1) {
		t.Fatalf("status JSON skipped_version_archives = %v, want 1", asMap["skipped_version_archives"])
	}
	lvs, ok := asMap["last_version_skip"].(map[string]any)
	if !ok || lvs["path"] != "big.txt" {
		t.Fatalf("status JSON last_version_skip = %v, want object with path big.txt", asMap["last_version_skip"])
	}
	if a.d.Status().SkippedVersionArchives != 0 {
		t.Fatal("A recorded a skip for a purely local edit (archive only runs on receive)")
	}
}
