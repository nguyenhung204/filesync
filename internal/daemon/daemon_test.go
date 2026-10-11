package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesync/internal/config"
	"filesync/internal/logx"
	"filesync/internal/store"
)

// testDaemon is a running daemon on loopback with mDNS disabled.
type testDaemon struct {
	d      *Daemon
	cfg    config.Config
	cancel context.CancelFunc
	done   chan error
}

func startTestDaemon(t *testing.T, name string) *testDaemon {
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
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	log := logx.New(os.Stderr, logx.LevelError)
	d, err := New(loaded, log)
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
	// Wait until the daemon reports its real listen address.
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

func (td *testDaemon) trustPeer(t *testing.T, other *testDaemon) {
	t.Helper()
	st, err := store.Open(td.cfg.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	oid := other.d.Status()
	err = st.PutPeer(store.Peer{
		DeviceID:        oid.DeviceID,
		Name:            other.cfg.DeviceName,
		CertFingerprint: oid.Fingerprint,
		Addresses:       []string{oid.ListenAddr},
		PairedAt:        time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func setPaused(t *testing.T, cfg config.Config, paused bool) {
	t.Helper()
	st, err := store.Open(cfg.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if paused {
		if err := st.JournalSet(pauseJournalKey, []byte("1")); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := st.JournalDelete(pauseJournalKey); err != nil {
			t.Fatal(err)
		}
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func writeSyncFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileExists(dir, rel string) bool {
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}

func TestDaemonSecondInstanceFails(t *testing.T) {
	td := startTestDaemon(t, "one")
	// A second daemon for the same config dir must fail with the
	// exact D7 message.
	d2, err := New(td.cfg, logx.New(os.Stderr, logx.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	err = d2.Run(context.Background())
	if err == nil {
		t.Fatal("second daemon instance started on the same config dir")
	}
	want := fmt.Sprintf("filesync: daemon already running for config dir %s (pid %d)", td.cfg.ConfigDir, os.Getpid())
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestDaemonStartupPurgesExpired(t *testing.T) {
	base := t.TempDir()
	cfgDir := filepath.Join(base, "config")
	syncDir := filepath.Join(base, "sync")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed an expired tombstone (>30d) and a fresh one directly.
	st, err := store.Open(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-31 * 24 * time.Hour).UnixNano()
	fresh := time.Now().UnixNano()
	put := func(p string, ts int64) {
		e := store.Entry{Path: p, Tombstone: true, TombstoneTs: ts, UpdatedBy: "X"}
		if err := st.UpsertEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	put("expired.txt", old)
	put("recent.txt", fresh)
	st.Close()

	cfg := config.Default()
	cfg.ConfigDir = cfgDir
	cfg.SyncDir = syncDir
	cfg.DeviceName = "purger"
	cfg.ListenAddr = "127.0.0.1:0"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, _ := config.Load(cfgDir)
	d, err := New(loaded, logx.New(os.Stderr, logx.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitFor(t, "daemon up", 10*time.Second, func() bool { return d.Status().ListenAddr != "" })
	cancel()
	<-done

	st2, err := store.Open(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, found, _ := st2.GetEntry("expired.txt"); found {
		t.Fatal("expired tombstone survived daemon startup purge")
	}
	if _, found, _ := st2.GetEntry("recent.txt"); !found {
		t.Fatal("fresh tombstone was wrongly purged")
	}
}

func TestDaemonRescanEventTriggersFullCommit(t *testing.T) {
	// Disable mDNS for the whole test binary run of this test and
	// keep the rescan interval long: only watcher events may move
	// the file, which is what this test measures.
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	a := startTestDaemon(t, "alice")
	b := startTestDaemon(t, "bob")
	a.trustPeer(t, b)
	b.trustPeer(t, a)
	waitFor(t, "peers connected", 15*time.Second, func() bool {
		for _, p := range b.d.Status().Peers {
			if p.DeviceID == a.d.Status().DeviceID && p.Connected {
				return true
			}
		}
		return false
	})
	writeSyncFile(t, a.cfg.SyncDir, "watched.txt", "seen by watcher")
	waitFor(t, "file synced via watcher event", 15*time.Second, func() bool {
		return fileExists(b.cfg.SyncDir, "watched.txt")
	})
}

func TestDaemonPauseStopsRounds(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	a := startTestDaemon(t, "alice")
	b := startTestDaemon(t, "bob")
	a.trustPeer(t, b)
	b.trustPeer(t, a)

	writeSyncFile(t, a.cfg.SyncDir, "before.txt", "before pause")
	waitFor(t, "initial sync", 15*time.Second, func() bool {
		return fileExists(b.cfg.SyncDir, "before.txt")
	})

	setPaused(t, a.cfg, true)
	waitFor(t, "a reports paused", 10*time.Second, func() bool {
		return a.d.Status().State == "paused"
	})
	writeSyncFile(t, a.cfg.SyncDir, "during.txt", "during pause")
	// Give the daemon ample opportunity to (wrongly) sync.
	time.Sleep(4 * time.Second)
	if fileExists(b.cfg.SyncDir, "during.txt") {
		t.Fatal("file synced while daemon was paused")
	}
	setPaused(t, a.cfg, false)
	waitFor(t, "sync resumes", 15*time.Second, func() bool {
		return fileExists(b.cfg.SyncDir, "during.txt")
	})
}

func TestDaemonRevokedPeerDisconnected(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	a := startTestDaemon(t, "alice")
	b := startTestDaemon(t, "bob")
	a.trustPeer(t, b)
	b.trustPeer(t, a)
	waitFor(t, "peers connected", 15*time.Second, func() bool {
		for _, p := range a.d.Status().Peers {
			if p.Connected {
				return true
			}
		}
		return false
	})
	// Revoke B on A's side, exactly the way the CLI does it: a
	// direct store write while A's daemon is running.
	st, err := store.Open(a.cfg.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RevokePeer(b.d.Status().DeviceID); err != nil {
		t.Fatal(err)
	}
	st.Close()
	waitFor(t, "peer disconnected after revoke", 15*time.Second, func() bool {
		for _, p := range a.d.Status().Peers {
			if p.DeviceID == b.d.Status().DeviceID {
				return !p.Connected && p.Revoked
			}
		}
		return false
	})
	// And no new data flows to the revoked peer.
	writeSyncFile(t, a.cfg.SyncDir, "secret.txt", "should not reach bob")
	time.Sleep(3 * time.Second)
	if fileExists(b.cfg.SyncDir, "secret.txt") {
		t.Fatal("file synced to a revoked peer")
	}
}

func TestDaemonStatusFileWritten(t *testing.T) {
	t.Setenv("FILESYNC_DISABLE_MDNS", "1")
	td := startTestDaemon(t, "stat")
	waitFor(t, "status.json present", 10*time.Second, func() bool {
		raw, err := os.ReadFile(filepath.Join(td.cfg.ConfigDir, statusFileName))
		if err != nil {
			return false
		}
		var st Status
		if err := json.Unmarshal(raw, &st); err != nil {
			return false
		}
		return st.State == "running" && st.DeviceID == td.d.Status().DeviceID && st.ListenAddr != ""
	})
}
