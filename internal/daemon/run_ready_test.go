package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"filesync/internal/config"
	"filesync/internal/logx"
)

// newReadyTestDaemon builds (but does not start) a daemon over a
// fresh temp config, mirroring startTestDaemon's setup.
func newReadyTestDaemon(t *testing.T) (*Daemon, config.Config) {
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
	cfg.DeviceName = "ready"
	cfg.ListenAddr = "127.0.0.1:0"
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
	return d, loaded
}

// Job 008 spec D3: the SCM host reports RUNNING off this
// callback, so it must fire exactly once, at the true ready
// point — state already "running", transport already listening.
func TestRunWithReadyFiresOnceWhenRunning(t *testing.T) {
	d, _ := newReadyTestDaemon(t)
	var calls atomic.Int32
	ready := make(chan struct{})
	var stateAtReady, addrAtReady string
	onReady := func() {
		if calls.Add(1) == 1 {
			st := d.Status()
			stateAtReady = st.State
			addrAtReady = st.ListenAddr
			close(ready)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.RunWithReady(ctx, onReady) }()

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("onReady not called within 10s")
	}
	if stateAtReady != "running" {
		t.Errorf("state at onReady = %q, want %q", stateAtReady, "running")
	}
	if addrAtReady == "" {
		t.Error("listen addr empty at onReady: transport not yet serving")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunWithReady returned %v on clean cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not stop in 10s after cancel")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("onReady called %d times, want exactly 1", got)
	}
}

// A daemon that fails before the ready point (here: the
// single-instance lock is already held) must never fire
// onReady — the SCM host would otherwise report RUNNING for a
// service that is about to exit with an error.
func TestRunWithReadySkippedOnStartupError(t *testing.T) {
	td := startTestDaemon(t, "holder")
	d2, err := New(td.cfg, logx.New(os.Stderr, logx.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	err = d2.RunWithReady(context.Background(), func() { calls.Add(1) })
	if err == nil {
		t.Fatal("second daemon instance started on the same config dir")
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("onReady called %d times for a failed startup, want 0", got)
	}
}

// The pre-bind Serve failure branch of the ready gate (job
// 008b): when the transport dies BEFORE it ever binds (here the
// listen port is already taken), onReady must stay unfired and
// RunWithReady must surface the serve error — the SCM host maps
// that to a STOPPED report with a non-zero exit code instead of
// a false RUNNING. Reviewer 008b verified this ad hoc; this test
// locks it in (job 009 inherited item c).
func TestRunWithReadySkippedWhenServeFailsBeforeBind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d, _ := newReadyTestDaemon(t)
	d.cfg.ListenAddr = ln.Addr().String() // occupied: bind must fail

	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- d.RunWithReady(context.Background(), func() { calls.Add(1) })
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunWithReady returned nil although the transport could not bind")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RunWithReady did not return after the pre-bind serve failure")
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("onReady called %d times when Serve failed before bind, want 0", got)
	}
}
